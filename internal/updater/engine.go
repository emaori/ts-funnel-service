package updater

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Logger interface {
	Printf(string, ...any)
	Check() error
}
type Driver interface {
	Discover(context.Context) (Release, error)
	Prepare(context.Context, Release, string) error
	Preflight(context.Context, string, string) error
	Baseline(context.Context) (Baseline, error)
	Start(context.Context, Slot) error
	Stop(context.Context) error
	Validate(context.Context, Slot, Baseline) error
	Observe(context.Context) (Observation, error)
}
type Observation struct {
	Running, Ready, ProbeOK bool
	Version                 string
	Identity                Identity
	ProbeAt                 time.Time
}
type Options struct {
	Enabled                                                bool
	Interval, InitialDelay, StopTimeout, ValidationTimeout time.Duration
}
type Snapshot struct {
	State       State
	Phase       string
	Busy        bool
	Observation Observation
	Enabled     bool
	Interval    time.Duration
}
type Engine struct {
	store       *Store
	driver      Driver
	log         Logger
	opts        Options
	op          sync.Mutex
	mu          sync.RWMutex
	state       State
	phase       string
	busy        bool
	observation Observation
	recovery    *Attempt
	now         func() time.Time
	wake        chan struct{}
}

func NewEngine(s *Store, state State, d Driver, l Logger, o Options) *Engine {
	return &Engine{store: s, state: state, driver: d, log: l, opts: o, phase: "idle", now: time.Now, wake: make(chan struct{}, 1)}
}
func (e *Engine) Snapshot() Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return Snapshot{State: e.state.Clone(), Phase: e.phase, Busy: e.busy, Observation: e.observation, Enabled: e.opts.Enabled, Interval: e.opts.Interval}
}
func (e *Engine) edit(fn func(*State)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := e.state.Clone()
	fn(&next)
	if err := e.store.Save(next); err != nil {
		return err
	}
	e.state = next
	return nil
}
func (e *Engine) setPhase(phase string) error {
	e.mu.Lock()
	e.phase = phase
	var a *Attempt
	if e.state.LastAttempt != nil && e.busy {
		copy := *e.state.LastAttempt
		copy.Phase = phase
		a = &copy
	}
	e.mu.Unlock()
	if a != nil {
		if err := e.store.SaveAttempt(*a); err != nil {
			return err
		}
		e.mu.Lock()
		e.state.LastAttempt = a
		e.mu.Unlock()
		e.log.Printf("update=%s component=tailscale phase=%s from=%s to=%s", a.ID, phase, a.From.Version, a.To.Version)
	}
	return nil
}
func (e *Engine) SetObservation(o Observation) { e.mu.Lock(); e.observation = o; e.mu.Unlock() }
func (e *Engine) SetIdentity(id Identity) error {
	if id.NodeID == "" {
		return errors.New("cannot persist an empty production identity")
	}
	if old := e.Snapshot().State.Identity; old.NodeID != "" && old != id {
		return errors.New("production identity changed")
	}
	return e.edit(func(s *State) { s.Identity = id; s.EnrollmentAllowed = false })
}
func (e *Engine) Error(phase, code string, err error) {
	attempt := "-"
	if a := e.Snapshot().State.LastAttempt; a != nil && a.Result == "" {
		attempt = a.ID
	}
	e.log.Printf("ERROR update=%s component=tailscale phase=%s code=%s %s", attempt, phase, code, err)
	saveErr := e.edit(func(s *State) {
		s.LastError = &ErrorRecord{At: e.now().UTC(), Phase: phase, Code: code, Message: code}
		s.Errors[phase+":"+code]++
		if e.busy && s.LastAttempt != nil {
			s.LastAttempt.HadErrors = true
		}
	})
	if saveErr != nil {
		e.mu.Lock()
		e.state.LastError = &ErrorRecord{At: e.now().UTC(), Phase: phase, Code: code, Message: code}
		e.state.Errors[phase+":"+code]++
		if e.busy && e.state.LastAttempt != nil {
			e.state.LastAttempt.HadErrors = true
		}
		e.mu.Unlock()
		e.log.Printf("ERROR component=supervisor code=persistence cannot persist error summary: %v", saveErr)
	}
}
func (e *Engine) WithIdle(ctx context.Context, fn func(context.Context)) bool {
	if !e.op.TryLock() {
		return false
	}
	defer e.op.Unlock()
	fn(ctx)
	return true
}

// RecoverBeforeStart runs before any production child. A terminal active record
// wins over an interrupted transaction cleanup; an uncommitted candidate never wins.
func (e *Engine) RecoverBeforeStart() error {
	var a Attempt
	err := ReadJSON(e.store.Path("transaction-record"), &a)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := a.Validate(); err != nil {
		return err
	}
	s := e.Snapshot().State
	if s.LastAttempt != nil && s.LastAttempt.ID == a.ID && s.LastAttempt.Result != "" {
		if err := e.store.Archive(*s.LastAttempt); err != nil {
			return err
		}
		return e.store.ClearAttempt()
	}
	if s.Active != a.From {
		return errors.New("transaction disagrees with committed slot; refusing identity reset")
	}
	e.recovery = &a
	e.phase = "recovering"
	e.busy = true
	e.log.Printf("update=%s component=tailscale phase=recovering committed_slot=%s version=%s", a.ID, s.Active.ID, s.Active.Version)
	return e.edit(func(s *State) { s.LastAttempt = &a })
}

// FailRecovery retains telemetry when the committed daemon cannot be restored.
func (e *Engine) FailRecovery(cause error) error {
	if e.recovery == nil {
		return cause
	}
	e.Error("recovering", "rollback", cause)
	if err := e.finish("rollback_failed", nil, true); err != nil {
		return err
	}
	e.recovery = nil
	return nil
}
func (e *Engine) CompleteRecovery(ctx context.Context) error {
	if e.recovery == nil {
		return nil
	}
	base, err := e.driver.Baseline(ctx)
	if err == nil && e.Snapshot().State.Identity.NodeID != "" && base.Identity != e.Snapshot().State.Identity {
		err = errors.New("recovered daemon identity differs from committed identity")
	}
	if err != nil {
		e.Error("recovering", "rollback", err)
		return e.finish("rollback_failed", nil, true)
	}
	var downtime *float64
	if !e.recovery.DisruptionStarted.IsZero() {
		n := e.now().Sub(e.recovery.DisruptionStarted).Seconds()
		downtime = &n
	}
	if err := e.finish("interrupted", downtime, false); err != nil {
		return err
	}
	e.recovery = nil
	return nil
}
func (e *Engine) finish(result string, interruption *float64, halt bool) error {
	now := e.now().UTC()
	err := e.edit(func(s *State) {
		if s.LastAttempt == nil || s.LastAttempt.Result != "" {
			return
		}
		a := s.LastAttempt
		a.Result = result
		a.Finished = now
		a.Phase = "idle"
		a.Duration = now.Sub(a.Started).Seconds()
		a.Interruption = interruption
		s.Attempts[result]++
		s.LastResult = result
		if result == "success" {
			s.PreviousVersion = a.From.Version
			s.RollbackSlot = a.From.ID
			s.Active = a.To
			s.LastSuccess = now
			s.QuarantinedVersion = ""
			s.AvailableVersion = ""
			s.InstallFailures = 0
		} else if !a.DisruptionStarted.IsZero() {
			s.QuarantinedVersion = a.To.Version
		}
		if result == "failed" && a.DisruptionStarted.IsZero() {
			s.InstallFailures++
			s.NextCheck = e.now().Add(jitter(backoff(s.InstallFailures, e.opts.Interval)))
		}
		s.Halted = halt
	})
	if err != nil {
		return err
	}
	s := e.Snapshot().State
	if s.LastAttempt != nil {
		e.log.Printf("update=%s component=tailscale result=%s running_version=%s duration_seconds=%.3f had_errors=%t", s.LastAttempt.ID, result, s.Active.Version, s.LastAttempt.Duration, s.LastAttempt.HadErrors)
		if err := e.store.Archive(*s.LastAttempt); err != nil {
			e.log.Printf("WARNING component=supervisor code=persistence archive: %v", err)
		}
	}
	if err := e.store.ClearAttempt(); err != nil {
		return err
	}
	e.mu.Lock()
	e.busy = false
	e.phase = "idle"
	e.mu.Unlock()
	return nil
}
func (e *Engine) Run(ctx context.Context) error {
	if !e.opts.Enabled {
		<-ctx.Done()
		return nil
	}
	// Serialize initialization with local retry requests so a wake cannot be
	// overwritten by an old persisted schedule.
	e.op.Lock()
	manual := false
	select {
	case <-e.wake:
		manual = true
	default:
	}
	next := e.Snapshot().State.NextCheck
	if next.IsZero() || (next.Before(e.now()) && !manual) {
		next = e.now().Add(jitter(e.opts.InitialDelay))
	}
	err := e.edit(func(s *State) { s.NextCheck = next })
	e.op.Unlock()
	if err != nil {
		return err
	}
	for {
		if e.Snapshot().State.Halted {
			select {
			case <-ctx.Done():
				return nil
			case <-e.wake:
				next = e.Snapshot().State.NextCheck
				continue
			}
		}
		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-e.wake:
			t.Stop()
			next = e.Snapshot().State.NextCheck
			continue
		case <-t.C:
		}
		if err := e.Check(ctx); err != nil {
			return err
		}
		next = e.Snapshot().State.NextCheck
	}
}
func backoff(failures int, limit time.Duration) time.Duration {
	delay := 15 * time.Minute
	for i := 1; i < failures && delay < limit; i++ {
		delay *= 2
	}
	if delay > limit {
		return limit
	}
	return delay
}
func jitter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return time.Second
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(interval/5)+1))
	if err != nil {
		return interval
	}
	return interval - interval/10 + time.Duration(n.Int64())
}
func (e *Engine) Check(ctx context.Context) error {
	e.op.Lock()
	defer e.op.Unlock()
	defer func() {
		e.mu.Lock()
		if !e.busy {
			e.phase = "idle"
		}
		e.mu.Unlock()
	}()
	s := e.Snapshot().State
	if !e.opts.Enabled || s.Halted {
		return nil
	}
	e.log.Printf("component=tailscale check=starting")
	if err := e.log.Check(); err != nil {
		e.Error("checking", "logging", err)
		return e.edit(func(s *State) { s.NextCheck = e.now().Add(jitter(e.opts.Interval)) })
	}
	e.mu.Lock()
	e.phase = "checking"
	e.mu.Unlock()
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	release, err := e.driver.Discover(checkCtx)
	cancel()
	if err != nil {
		e.Error("checking", "metadata_fetch", err)
		return e.edit(func(s *State) {
			s.LastCheck = e.now().UTC()
			s.Checks["error"]++
			s.CheckFailures++
			delay := backoff(s.CheckFailures, e.opts.Interval)
			s.NextCheck = e.now().Add(jitter(delay))
		})
	}
	if err := e.edit(func(s *State) {
		s.LastCheck = e.now().UTC()
		s.LastSuccessfulCheck = s.LastCheck
		s.Checks["success"]++
		s.CheckFailures = 0
		s.NextCheck = e.now().Add(jitter(e.opts.Interval))
		if Newer(release.Version, s.Active.Version) {
			s.AvailableVersion = release.Version
		} else {
			s.AvailableVersion = ""
		}
	}); err != nil {
		return err
	}
	s = e.Snapshot().State
	if !Newer(release.Version, s.Active.Version) {
		e.log.Printf("component=tailscale check=success current=%s result=already_current", s.Active.Version)
		return nil
	}
	if release.Version == s.QuarantinedVersion {
		e.log.Printf("component=tailscale candidate=%s result=quarantined", release.Version)
		return nil
	}
	return e.install(ctx, release)
}
func (e *Engine) install(ctx context.Context, release Release) error {
	s := e.Snapshot().State
	to := Slot{ID: "b", Version: release.Version}
	if s.Active.ID == "b" {
		to.ID = "a"
	}
	a := Attempt{ID: e.now().UTC().Format("20060102T150405.000000000"), From: s.Active, To: to, Started: e.now().UTC(), Phase: "preflighting"}
	if err := e.store.SaveAttempt(a); err != nil {
		return err
	}
	e.mu.Lock()
	e.busy = true
	e.state.LastAttempt = &a
	e.mu.Unlock()
	fail := func(phase, code string, err error) error {
		e.Error(phase, code, err)
		zero := 0.0
		if ctx.Err() != nil {
			return e.finish("interrupted", &zero, false)
		}
		return e.finish("failed", &zero, false)
	}
	if err := e.setPhase("preflighting"); err != nil {
		return err
	}
	preCtx, preCancel := context.WithTimeout(ctx, e.opts.ValidationTimeout)
	base, err := e.driver.Baseline(preCtx)
	preCancel()
	if err != nil {
		return fail("preflighting", "public_probe", err)
	}
	if s.Identity.NodeID != "" && base.Identity != s.Identity {
		return fail("preflighting", "identity_mismatch", errors.New("production identity changed"))
	}
	size, err := TreeSize(s.Active.StateDir(e.store.Root))
	if err != nil {
		return fail("preflighting", "state_copy", err)
	}
	if err := HasSpace(e.store.Root, size+(768<<20)); err != nil {
		return fail("preflighting", "extraction", err)
	}
	if err := os.MkdirAll(e.store.Path("staging"), 0700); err != nil {
		return fail("downloading", "download", err)
	}
	stage, err := os.MkdirTemp(e.store.Path("staging"), "candidate-")
	if err != nil {
		return fail("downloading", "download", err)
	}
	defer os.RemoveAll(stage)
	if err := e.setPhase("downloading"); err != nil {
		return err
	}
	downloadCtx, downloadCancel := context.WithTimeout(ctx, 10*time.Minute)
	err = e.driver.Prepare(downloadCtx, release, stage)
	downloadCancel()
	if err != nil {
		return fail("verifying", ErrorCode(err, "download"), err)
	}
	if err := e.setPhase("verifying"); err != nil {
		return err
	}
	if err := e.setPhase("preflighting"); err != nil {
		return err
	}
	preCtx, preCancel = context.WithTimeout(ctx, e.opts.ValidationTimeout)
	err = e.driver.Preflight(preCtx, stage, release.Version)
	preCancel()
	if err != nil {
		return fail("preflighting", "preflight", err)
	}
	if ctx.Err() != nil {
		return fail("preflighting", "preflight", ctx.Err())
	}
	preCtx, preCancel = context.WithTimeout(ctx, e.opts.ValidationTimeout)
	current, err := e.driver.Baseline(preCtx)
	preCancel()
	if err != nil || current != base {
		if err == nil {
			err = errors.New("production baseline changed during staging")
		}
		return fail("preflighting", "public_probe", err)
	}
	e.log.Printf("update=%s component=tailscale phase=preflighting ready_for_switch=true", a.ID)
	if err := e.log.Check(); err != nil {
		return fail("preflighting", "logging", err)
	}
	if err := e.edit(func(s *State) { s.RollbackSlot = "" }); err != nil {
		return err
	}
	inactive := filepath.Dir(to.StateDir(e.store.Root))
	if err := os.RemoveAll(inactive); err != nil {
		return fail("verifying", "extraction", err)
	}
	if err := os.MkdirAll(filepath.Dir(to.Bin(e.store.Root, "tailscale")), 0700); err != nil {
		return fail("verifying", "extraction", err)
	}
	for _, name := range []string{"tailscale", "tailscaled", "artifact-record"} {
		if err := CopyFile(filepath.Join(stage, name), to.Bin(e.store.Root, name), 0755); err != nil {
			return fail("verifying", "extraction", err)
		}
	}
	if err := e.setPhase("switching"); err != nil {
		return err
	}
	if err := e.edit(func(s *State) { s.LastAttempt.DisruptionStarted = e.now().UTC() }); err != nil {
		return err
	}
	a = *e.Snapshot().State.LastAttempt
	if err := e.store.SaveAttempt(a); err != nil {
		return err
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), e.opts.StopTimeout)
	err = e.driver.Stop(stopCtx)
	stopCancel()
	if err != nil {
		e.Error("switching", "stop_timeout", err)
		return e.finish("failed", nil, false)
	}
	e.SetObservation(Observation{})
	restore := func(cause error) error {
		e.Error("validating", ErrorCode(cause, "startup"), cause)
		if err := e.setPhase("rolling_back"); err != nil {
			e.log.Printf("ERROR code=persistence cannot journal rollback: %v", err)
		}
		recoveryCtx, cancel := context.WithTimeout(context.Background(), e.opts.StopTimeout+e.opts.ValidationTimeout+30*time.Second)
		defer cancel()
		stopCtx, c := context.WithTimeout(recoveryCtx, e.opts.StopTimeout)
		stopErr := e.driver.Stop(stopCtx)
		c()
		if stopErr != nil {
			e.Error("rolling_back", "rollback", stopErr)
			return e.finish("rollback_failed", nil, true)
		}
		if err := e.driver.Start(recoveryCtx, a.From); err != nil {
			e.Error("rolling_back", "rollback", err)
			return e.finish("rollback_failed", nil, true)
		}
		if err := e.driver.Validate(recoveryCtx, a.From, base); err != nil {
			e.Error("rolling_back", "rollback", err)
			return e.finish("rollback_failed", nil, true)
		}
		if o, err := e.driver.Observe(recoveryCtx); err == nil {
			e.SetObservation(o)
		}
		n := e.now().Sub(a.DisruptionStarted).Seconds()
		result := "rolled_back"
		if ctx.Err() != nil {
			result = "interrupted"
		}
		return e.finish(result, &n, false)
	}
	if err := e.store.CopyState(a.From, to); err != nil {
		return restore(&CodedError{Code: "state_copy", Err: err})
	}
	if ctx.Err() != nil {
		return restore(ctx.Err())
	}
	startCtx, startCancel := context.WithTimeout(ctx, 30*time.Second)
	err = e.driver.Start(startCtx, to)
	startCancel()
	if err != nil {
		return restore(err)
	}
	if err := e.setPhase("validating"); err != nil {
		return restore(&CodedError{Code: "persistence", Err: err})
	}
	validCtx, validCancel := context.WithTimeout(ctx, e.opts.ValidationTimeout)
	err = e.driver.Validate(validCtx, to, base)
	validCancel()
	if err != nil {
		return restore(err)
	}
	if o, err := e.driver.Observe(ctx); err == nil {
		e.SetObservation(o)
	}
	n := e.now().Sub(a.DisruptionStarted).Seconds()
	if err := e.finish("success", &n, false); err != nil {
		loaded, loadErr := e.store.Load()
		if loadErr == nil && loaded.LastAttempt != nil && loaded.LastAttempt.ID == a.ID && loaded.LastAttempt.Result == "success" {
			e.mu.Lock()
			e.state = loaded
			e.busy = false
			e.phase = "idle"
			e.mu.Unlock()
			return fmt.Errorf("candidate committed but cleanup failed: %w", err)
		}
		return restore(&CodedError{Code: "persistence", Err: err})
	}
	return nil
}

type CodedError struct {
	Code string
	Err  error
}

func (e *CodedError) Error() string { return e.Err.Error() }
func (e *CodedError) Unwrap() error { return e.Err }
func ErrorCode(err error, fallback string) string {
	var e *CodedError
	if errors.As(err, &e) {
		return e.Code
	}
	return fallback
}
