package observability

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Logger struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	maxFiles int
	out      io.Writer
	secrets  []string
	lastErr  error
}

func NewLogger(dir string, maxBytes int64, maxFiles int, out io.Writer, secrets ...string) *Logger {
	l := &Logger{dir: dir, maxBytes: maxBytes, maxFiles: maxFiles, out: out, secrets: secrets}
	if err := l.Check(); err != nil {
		_, _ = fmt.Fprintf(out, "%s ERROR component=supervisor code=logging file log unavailable: %s\n", time.Now().UTC().Format(time.RFC3339), l.sanitize(err.Error()))
	}
	return l
}

func (l *Logger) sanitize(s string) string {
	for _, secret := range l.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	s = strings.ReplaceAll(s, "\x00", "")
	// Keep each event on one physical line, including command errors.
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " | ")
	if len(s) > 4096 {
		s = s[:4096] + " [truncated]"
	}
	return s
}

func (l *Logger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := time.Now().UTC().Format(time.RFC3339Nano) + " " + l.sanitize(fmt.Sprintf(format, args...)) + "\n"
	if int64(len(line)) > l.maxBytes {
		line = line[:int(l.maxBytes)-14] + " [truncated]\n"
	}
	_, _ = io.WriteString(l.out, line)
	if err := l.write(line); err != nil {
		if l.lastErr == nil {
			_, _ = fmt.Fprintf(l.out, "%s ERROR component=supervisor code=logging file log unavailable: %s\n", time.Now().UTC().Format(time.RFC3339), l.sanitize(err.Error()))
		}
		l.lastErr = err
	} else {
		l.lastErr = nil
	}
}

func (l *Logger) write(line string) error {
	if err := os.MkdirAll(l.dir, 0700); err != nil {
		return err
	}
	p := filepath.Join(l.dir, "updates.log")
	info, err := os.Stat(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && info.Size()+int64(len(line)) > l.maxBytes {
		if err := os.Remove(filepath.Join(l.dir, fmt.Sprintf("updates.log.%d", l.maxFiles-1))); err != nil && !os.IsNotExist(err) {
			return err
		}
		for i := l.maxFiles - 2; i >= 1; i-- {
			if err := os.Rename(filepath.Join(l.dir, fmt.Sprintf("updates.log.%d", i)), filepath.Join(l.dir, fmt.Sprintf("updates.log.%d", i+1))); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if l.maxFiles > 1 {
			if err := os.Rename(p, p+".1"); err != nil {
				return err
			}
		} else if err := os.Remove(p); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (l *Logger) Check() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, 0700); err != nil {
		l.lastErr = err
		return err
	}
	f, err := os.OpenFile(filepath.Join(l.dir, "updates.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err == nil {
		err = f.Sync()
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	}
	// A successful real event write clears an earlier write/rotation failure.
	if err == nil && l.lastErr != nil {
		return l.lastErr
	}
	l.lastErr = err
	return err
}

// ChildWriter logs bounded command/daemon diagnostics through both update sinks.
type ChildWriter struct {
	Logger  *Logger
	Source  string
	mu      sync.Mutex
	partial []byte
}

func (w *ChildWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		end := strings.IndexByte(string(p), '\n')
		if end < 0 {
			end = len(p)
		}
		room := 4096 - len(w.partial)
		take := end
		if take > room {
			take = room
		}
		w.partial = append(w.partial, p[:take]...)
		if end < len(p) || len(w.partial) == 4096 {
			w.Logger.Printf("component=%s %s", w.Source, string(w.partial))
			w.partial = nil
		}
		if end == len(p) {
			break
		}
		p = p[end+1:]
	}
	return n, nil
}
