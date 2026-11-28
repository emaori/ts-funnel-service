package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

type readyWriter struct{ ready chan struct{} }

func (w readyWriter) Write(b []byte) (int, error) {
	if strings.Contains(string(b), "READY") {
		select {
		case w.ready <- struct{}{}:
		default:
		}
	}
	return len(b), nil
}
func TestProcessHelper(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "ignore-term" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	fmt.Println("READY")
	for {
		time.Sleep(time.Hour)
	}
}
func TestTimeoutNeverPretendsChildStopped(t *testing.T) {
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	w := readyWriter{ready: make(chan struct{}, 1)}
	child, e := Start(exe, []string{"-test.run=TestProcessHelper", "--", "ignore-term"}, w)
	if e != nil {
		t.Fatal(e)
	}
	defer child.Shutdown(time.Millisecond)
	select {
	case <-w.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("helper not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if e := child.Stop(ctx); !errors.Is(e, ErrStopTimeout) || !child.Running() {
		t.Fatal("timed-out child reported stopped", e)
	}
	if e := child.Shutdown(30 * time.Millisecond); e != nil || child.Running() {
		t.Fatal("forced cleanup failed", e)
	}
}
