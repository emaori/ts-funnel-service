package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

var ErrStopTimeout = errors.New("process did not stop before deadline")

type Child struct {
	cmd  *exec.Cmd
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func Start(binary string, args []string, output io.Writer) (*Child, error) {
	cmd := exec.Command(binary, args...)
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Child{cmd: cmd, done: make(chan struct{})}
	go func() { err := cmd.Wait(); c.mu.Lock(); c.err = err; c.mu.Unlock(); close(c.done) }()
	return c, nil
}
func (c *Child) PID() int { return c.cmd.Process.Pid }
func (c *Child) Running() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}
func (c *Child) Done() <-chan struct{} { return c.done }
func (c *Child) Err() error            { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
func (c *Child) Stop(ctx context.Context) error {
	if !c.Running() {
		return nil
	}
	if err := syscall.Kill(-c.PID(), syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		if !c.Running() {
			return nil
		}
		return fmt.Errorf("%w: pid %d", ErrStopTimeout, c.PID())
	}
}
func (c *Child) Shutdown(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		_ = syscall.Kill(-c.PID(), syscall.SIGKILL)
		select {
		case <-c.done:
			return nil
		case <-time.After(5 * time.Second):
			return err
		}
	}
	return nil
}
func Command(ctx context.Context, binary string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = os.Environ()
	output := &boundedOutput{}
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	err := cmd.Run()
	out := output.data
	if err != nil {
		return out, fmt.Errorf("command failed: %w: %s", err, out)
	}
	return out, nil
}

// boundedOutput consumes all child output while retaining only a diagnostic prefix.
type boundedOutput struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if room := 16384 - len(b.data); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}
