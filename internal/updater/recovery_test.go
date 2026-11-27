package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVolumeLockAndLegacyMigration(t *testing.T) {
	root := t.TempDir()
	for name, text := range map[string]string{"tailscaled.state": "identity+serve", "certs/test.key": "certificate", "tka/key": "tailnet-lock"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	seed := t.TempDir()
	for _, name := range []string{"tailscale", "tailscaled"} {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if other, err := OpenStore(root); err == nil {
		other.Close()
		t.Fatal("shared identity volume not locked")
	}
	s, err := store.Seed(seed, "v1.104.1")
	if err != nil {
		t.Fatal(err)
	}
	if s.EnrollmentAllowed {
		t.Fatal("legacy identity can be re-enrolled")
	}
	for _, name := range []string{"tailscaled.state", "certs/test.key", "tka/key"} {
		src, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		dst, err := os.ReadFile(filepath.Join(s.Active.StateDir(root), name))
		if err != nil || string(src) != string(dst) {
			t.Fatal(name, err)
		}
	}
	// A newer image must not replace a valid committed runtime slot.
	again, err := store.Seed("/no-seed-needed", "v1.106.0")
	if err != nil || again.Active != s.Active {
		t.Fatal("image seed replaced runtime", err)
	}
	if err := os.Remove(store.Path("active-record")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Seed(seed, "v1.104.1"); err == nil {
		t.Fatal("missing committed metadata silently reset identity")
	}
}
func TestEnrollmentPermissionSurvivesUnenrolledRestart(t *testing.T) {
	store, e, _ := fixture(t)
	s := e.Snapshot().State
	s.Identity = Identity{}
	s.EnrollmentAllowed = true
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	// tailscaled may write a local state file before enrollment is attempted.
	s, err := store.Load()
	if err != nil || !s.EnrollmentAllowed {
		t.Fatal("first enrollment permission lost", err)
	}
	fresh := NewEngine(store, s, e.driver, e.log, e.opts)
	if err := fresh.SetIdentity(Identity{NodeID: "enrolled", DNSName: "node.example.ts.net"}); err != nil {
		t.Fatal(err)
	}
	s, err = store.Load()
	if err != nil || s.EnrollmentAllowed {
		t.Fatal("enrollment remained allowed", err)
	}
	if err := fresh.SetIdentity(Identity{NodeID: "different"}); err == nil {
		t.Fatal("identity replacement accepted")
	}
}

func TestStateCopyFailureRestoresOutgoingPair(t *testing.T) {
	_, e, d := fixture(t)
	// The second Stop sees the already-present link; permit normal restoration.
	e.driver = &copyOnceFailureDriver{fakeDriver: d}
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := e.Snapshot().State
	if s.Active.Version != "v1.100.0" || s.LastAttempt.Result != "rolled_back" || d.overlap {
		t.Fatalf("unsafe recovery: %+v", s)
	}
}

type copyOnceFailureDriver struct {
	*fakeDriver
	injected bool
}

func (d *copyOnceFailureDriver) Stop(ctx context.Context) error {
	if err := d.fakeDriver.Stop(ctx); err != nil {
		return err
	}
	if !d.injected {
		d.injected = true
		return os.Symlink("tailscaled.state", filepath.Join(d.slot.StateDir(d.root), "unsafe-link"))
	}
	return nil
}

type commitFailureDriver struct {
	*fakeDriver
	store    *Store
	injected bool
}

func (d *commitFailureDriver) Validate(ctx context.Context, s Slot, b Baseline) error {
	if s.Version == d.release.Version && !d.injected {
		d.injected = true
		return os.Chmod(d.store.Path(""), 0500)
	}
	return d.fakeDriver.Validate(ctx, s, b)
}
func (d *commitFailureDriver) Start(ctx context.Context, s Slot) error {
	if s.Version != d.release.Version {
		if err := os.Chmod(d.store.Path(""), 0700); err != nil {
			return err
		}
	}
	return d.fakeDriver.Start(ctx, s)
}
func TestCommitFailureRestoresAndRetainsError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires unprivileged directory permissions")
	}
	store, e, d := fixture(t)
	e.driver = &commitFailureDriver{fakeDriver: d, store: store}
	t.Cleanup(func() { _ = os.Chmod(store.Path(""), 0700) })
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Active.Version != "v1.100.0" || s.LastAttempt.Result != "rolled_back" || !s.LastAttempt.HadErrors || s.Errors["validating:persistence"] != 1 {
		t.Fatalf("lost recovery/error: %+v", s)
	}
}
func TestManualRetryResumesHaltedScheduler(t *testing.T) {
	store, e, d := fixture(t)
	d.failAll = true
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := RequestRetry(store.Root); err != nil {
		t.Fatal(err)
	}
	if err := e.ConsumeRetry(); err == nil {
		t.Fatal("unready service accepted retry")
	}
	d.failAll = false
	e.SetObservation(Observation{Running: true, Ready: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	// Wait until Run has persisted startup scheduling before queuing the wake.
	deadline := time.Now().Add(5 * time.Second)
	for e.Snapshot().State.NextCheck.IsZero() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for e.Snapshot().State.Halted && time.Now().Before(deadline) {
		if err := e.ConsumeRetry(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	for e.Snapshot().State.LastResult != "success" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().State.LastResult != "success" {
		t.Fatalf("halted scheduler never resumed: %+v discoveries=%d starts=%v", e.Snapshot(), d.discoveries, d.starts)
	}
}
func TestFailedRecoveryRemainsObservableAndHalts(t *testing.T) {
	store, e, _ := fixture(t)
	s := e.Snapshot().State
	a := Attempt{ID: "20261127T120000.000000000", From: s.Active, To: Slot{ID: "b", Version: "v1.102.0"}, Started: time.Now(), Phase: "validating"}
	if err := store.SaveAttempt(a); err != nil {
		t.Fatal(err)
	}
	if err := e.RecoverBeforeStart(); err != nil {
		t.Fatal(err)
	}
	if err := e.FailRecovery(errors.New("committed daemon cannot start")); err != nil {
		t.Fatal(err)
	}
	s, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Halted || s.Attempts["rollback_failed"] != 1 || e.Snapshot().Busy {
		t.Fatal("failed recovery hidden")
	}
}
func TestRejectCorruptTransactionPathsAndTransitions(t *testing.T) {
	store, e, _ := fixture(t)
	s := e.Snapshot().State
	for _, mutate := range []func(*Attempt){
		func(a *Attempt) { a.ID = "../../escape" },
		func(a *Attempt) { a.To.ID = a.From.ID },
		func(a *Attempt) { a.Phase = "unknown" },
	} {
		a := Attempt{ID: "20261127T120000.000000000", From: s.Active, To: Slot{ID: "b", Version: "v1.102.0"}, Started: time.Now(), Phase: "validating"}
		mutate(&a)
		if err := AtomicJSON(store.Path("transaction-record"), a); err != nil {
			t.Fatal(err)
		}
		if err := e.RecoverBeforeStart(); err == nil {
			t.Fatal("corrupt transaction accepted")
		}
	}
}
func TestPersistedBinaryTamperFailsClosed(t *testing.T) {
	store, e, _ := fixture(t)
	if err := os.WriteFile(e.Snapshot().State.Active.Bin(store.Root, "tailscaled"), []byte("changed"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("changed binary accepted")
	}
}

func TestPreparationFailureBackoffAndIdentityGuard(t *testing.T) {
	_, e, d := fixture(t)
	d.prepareErr = errors.New("network timeout")
	var first time.Duration
	for i := 0; i < 2; i++ {
		if err := e.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		s := e.Snapshot().State
		delay := time.Until(s.NextCheck)
		if i == 0 {
			first = delay
		} else if delay < first*15/10 {
			t.Fatal("preparation retry did not back off")
		}
	}
	d.prepareErr = nil
	d.identity = Identity{NodeID: "unexpected-node", DNSName: "other.example.ts.net"}
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.stops != 0 || e.Snapshot().State.LastError.Code != "identity_mismatch" {
		t.Fatal("identity mismatch disrupted production")
	}
}
func TestInsufficientSpaceDoesNotStopProduction(t *testing.T) {
	store, e, d := fixture(t)
	path := filepath.Join(e.Snapshot().State.Active.StateDir(store.Root), "large-sparse-state")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// File length affects the copy budget without allocating real disk space.
	if err := f.Truncate(1 << 60); err != nil {
		f.Close()
		t.Skipf("filesystem does not support large sparse file: %v", err)
	}
	f.Close()
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.stops != 0 || len(d.starts) != 0 || e.Snapshot().State.LastAttempt.Result != "failed" {
		t.Fatal("low-space check disrupted production")
	}
}
