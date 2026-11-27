package updater

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RequestRetry is a local administrative file request, not a network API.
// The running supervisor consumes only a request for its quarantined version.
func RequestRetry(root string) error {
	var s State
	if err := ReadJSON(filepath.Join(root, "updater", "active-record"), &s); err != nil {
		return err
	}
	if err := s.Validate(); err != nil {
		return err
	}
	if s.QuarantinedVersion == "" {
		return errors.New("no quarantined candidate to retry")
	}
	return AtomicFile(filepath.Join(root, "updater", "retry-request"), []byte(s.QuarantinedVersion+"\n"), 0600)
}
func (e *Engine) ConsumeRetry() error {
	if !e.opts.Enabled {
		return nil
	}
	if !e.op.TryLock() {
		return nil
	}
	defer e.op.Unlock()
	data, err := os.ReadFile(e.store.Path("retry-request"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	version := strings.TrimSpace(string(data))
	s := e.Snapshot()
	if version != s.State.QuarantinedVersion || version == "" {
		return os.Remove(e.store.Path("retry-request"))
	}
	if !s.Observation.Ready {
		return errors.New("cannot retry an update until the outgoing service is locally ready")
	}
	if err := e.edit(func(s *State) { s.QuarantinedVersion = ""; s.Halted = false; s.NextCheck = e.now().UTC() }); err != nil {
		return err
	}
	if err := os.Remove(e.store.Path("retry-request")); err != nil {
		return err
	}
	e.log.Printf("component=tailscale manual_retry=true version=%s", version)
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return nil
}
func (e *Engine) HaltScheduling(cause error) {
	e.Error("recovering", "persistence", fmt.Errorf("new automatic updates halted: %w", cause))
	_ = e.edit(func(s *State) { s.Halted = true })
	e.mu.Lock()
	e.observation.Ready = false
	e.mu.Unlock()
}
