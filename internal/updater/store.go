package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

type Store struct {
	Root string
	lock *os.File
}

func OpenStore(root string) (*Store, error) {
	dir := filepath.Join(root, "updater")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "update.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("state volume is already in use: %w", err)
	}
	return &Store{Root: root, lock: f}, nil
}
func (s *Store) Close() error            { return s.lock.Close() }
func (s *Store) Path(name string) string { return filepath.Join(s.Root, "updater", name) }

func ReadJSON(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 4<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("invalid %s: %w", filepath.Base(path), err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing data in %s", filepath.Base(path))
	}
	return nil
}
func AtomicJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return AtomicFile(path, b, 0600)
}
func AtomicFile(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return SyncDir(dir)
}
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (s *Store) Load() (State, error) {
	var v State
	err := ReadJSON(s.Path("active-record"), &v)
	if err != nil {
		return v, err
	}
	if err = v.Validate(); err != nil {
		return v, err
	}
	return v, v.Active.Verify(s.Root)
}
func (s *Store) Save(v State) error {
	if err := v.Validate(); err != nil {
		return err
	}
	return AtomicJSON(s.Path("active-record"), v)
}
func (s *Store) SaveAttempt(a Attempt) error {
	if err := a.Validate(); err != nil {
		return err
	}
	return AtomicJSON(s.Path("transaction-record"), a)
}
func (s *Store) ClearAttempt() error {
	err := os.Remove(s.Path("transaction-record"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return SyncDir(s.Path(""))
}
func (s *Store) Archive(a Attempt) error {
	if err := a.Validate(); err != nil {
		return err
	}
	dir := s.Path("history")
	if err := AtomicJSON(filepath.Join(dir, a.ID+".json"), a); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	// Attempt IDs begin with UTC time and sort chronologically.
	for len(entries) > 100 {
		if err := os.Remove(filepath.Join(dir, entries[0].Name())); err != nil {
			return err
		}
		entries = entries[1:]
	}
	return nil
}

// Seed never discards a committed slot. The original flat state remains intact
// until the atomic active record exists, making first-start migration restartable.
func (s *Store) Seed(seedDir, version string) (State, error) {
	if v, err := s.Load(); err == nil {
		if err := AtomicFile(s.Path("bootstrap-complete"), []byte("1\n"), 0600); err != nil {
			return v, err
		}
		return v, nil
	} else if !os.IsNotExist(err) {
		return State{}, err
	}
	if _, err := os.Stat(s.Path("bootstrap-complete")); err == nil {
		return State{}, errors.New("active record missing from initialized volume; refusing to replace identity")
	}
	if _, err := os.Stat(s.Path("transaction-record")); err == nil {
		return State{}, errors.New("active record missing during an update; manual recovery required")
	}
	slot := Slot{ID: "a", Version: version}
	if _, err := os.Stat(s.Path("bootstrap-started")); os.IsNotExist(err) {
		if _, e := os.Stat(filepath.Dir(slot.StateDir(s.Root))); e == nil {
			return State{}, errors.New("unrecognized existing slots; refusing to replace state")
		}
		if err := AtomicFile(s.Path("bootstrap-started"), []byte("1\n"), 0600); err != nil {
			return State{}, err
		}
	}
	if err := os.RemoveAll(filepath.Dir(slot.StateDir(s.Root))); err != nil {
		return State{}, err
	}
	for _, name := range []string{"tailscale", "tailscaled"} {
		if err := CopyFile(filepath.Join(seedDir, name), slot.Bin(s.Root, name), 0755); err != nil {
			return State{}, err
		}
	}
	if err := CopyTree(s.Root, slot.StateDir(s.Root), map[string]bool{"updater": true}); err != nil {
		return State{}, fmt.Errorf("migrate legacy state: %w", err)
	}
	if err := RecordArtifact(filepath.Dir(slot.Bin(s.Root, "tailscale")), version, runtime.GOARCH, "image", ""); err != nil {
		return State{}, err
	}
	if err := s.SyncSlot(slot); err != nil {
		return State{}, err
	}
	v := NewState(slot)
	_, legacyErr := os.Stat(filepath.Join(s.Root, "tailscaled.state"))
	v.EnrollmentAllowed = os.IsNotExist(legacyErr)
	if err := s.Save(v); err != nil {
		return v, err
	}
	if err := AtomicFile(s.Path("bootstrap-complete"), []byte("1\n"), 0600); err != nil {
		return v, err
	}
	return v, nil
}
func CopyFile(src, dst string, mode fs.FileMode) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular file %s", src)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return SyncDir(filepath.Dir(dst))
}
func CopyTree(src, dst string, skip map[string]bool) error {
	if err := os.MkdirAll(dst, 0700); err != nil {
		return err
	}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(src, path)
		if e != nil {
			return e
		}
		if rel == "." {
			return nil
		}
		top := strings.Split(rel, string(filepath.Separator))[0]
		if skip[top] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		to := filepath.Join(dst, rel)
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing state symlink %s", rel)
		}
		if d.IsDir() {
			if err := os.MkdirAll(to, 0700); err != nil {
				return err
			}
			return SyncDir(filepath.Dir(to))
		}
		if d.Type()&os.ModeSocket != 0 {
			return nil
		} // transient sockets are recreated
		if err := CopyFile(path, to, 0600); err != nil {
			return err
		}
		return SyncDir(filepath.Dir(to))
	})
	if err != nil {
		return err
	}
	return SyncDir(dst)
}
func (s *Store) CopyState(from, to Slot) error {
	dst := to.StateDir(s.Root)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := CopyTree(from.StateDir(s.Root), dst, nil); err != nil {
		return err
	}
	return s.SyncSlot(to)
}
func HasSpace(root string, bytes uint64) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err != nil {
		return err
	}
	if stat.Bavail*uint64(stat.Bsize) < bytes {
		return errors.New("insufficient space for update staging and state copy")
	}
	return nil
}
func TreeSize(root string) (uint64, error) {
	var n uint64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			i, e := d.Info()
			if e != nil {
				return e
			}
			n += uint64(i.Size())
		}
		return nil
	})
	return n, err
}

func (s *Store) SyncSlot(slot Slot) error {
	for _, dir := range []string{slot.StateDir(s.Root), filepath.Dir(slot.Bin(s.Root, "tailscale")), filepath.Dir(slot.StateDir(s.Root)), s.Path("slots"), s.Path(""), s.Root} {
		if err := SyncDir(dir); err != nil {
			return err
		}
	}
	return nil
}
