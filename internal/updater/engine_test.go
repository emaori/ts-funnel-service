package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type testLogger struct{ failure error }

func (*testLogger) Printf(string, ...any) {}
func (l *testLogger) Check() error        { return l.failure }

type fakeDriver struct {
	root                                           string
	release                                        Release
	running                                        bool
	slot                                           Slot
	identity                                       Identity
	prepareErr, preflightErr, baselineErr, stopErr error
	failVersion                                    string
	failAll                                        bool
	starts                                         []Slot
	stops, discoveries                             int
	overlap                                        bool
}

func (f *fakeDriver) Discover(context.Context) (Release, error) {
	f.discoveries++
	return f.release, nil
}
func (f *fakeDriver) Prepare(_ context.Context, _ Release, dir string) error {
	if f.prepareErr != nil {
		return f.prepareErr
	}
	for _, n := range []string{"tailscale", "tailscaled"} {
		if e := os.WriteFile(filepath.Join(dir, n), []byte("verified candidate"), 0755); e != nil {
			return e
		}
	}
	return RecordArtifact(dir, f.release.Version, runtime.GOARCH, "official-signed", "test-artifact")
}
func (f *fakeDriver) Preflight(context.Context, string, string) error { return f.preflightErr }
func (f *fakeDriver) Baseline(context.Context) (Baseline, error) {
	if f.baselineErr != nil {
		return Baseline{}, f.baselineErr
	}
	if !f.running {
		return Baseline{}, errors.New("offline")
	}
	return Baseline{Identity: f.identity, HTTPStatus: 401}, nil
}
func (f *fakeDriver) Start(_ context.Context, s Slot) error {
	if f.running {
		f.overlap = true
		return errors.New("overlap")
	}
	f.running = true
	f.slot = s
	f.starts = append(f.starts, s)
	return nil
}
func (f *fakeDriver) Stop(context.Context) error {
	f.stops++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.running = false
	return nil
}
func (f *fakeDriver) Validate(_ context.Context, s Slot, _ Baseline) error {
	if f.failAll || s.Version == f.failVersion {
		_ = os.WriteFile(filepath.Join(s.StateDir(f.root), "candidate-migration"), []byte("new format"), 0600)
		return &CodedError{Code: "public_probe", Err: errors.New("candidate unreachable")}
	}
	return nil
}
func (f *fakeDriver) Observe(context.Context) (Observation, error) {
	return Observation{Running: f.running, Ready: f.running, ProbeOK: f.running, Version: f.slot.Version, Identity: f.identity, ProbeAt: time.Now()}, nil
}
func fixture(t *testing.T) (*Store, *Engine, *fakeDriver) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	seed := filepath.Join(t.TempDir(), "seed")
	if e := os.MkdirAll(seed, 0700); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"tailscale", "tailscaled"} {
		if e := os.WriteFile(filepath.Join(seed, name), []byte("seed executable"), 0755); e != nil {
			t.Fatal(e)
		}
	}
	store, e := OpenStore(root)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.Close() })
	s, e := store.Seed(seed, "v1.100.0")
	if e != nil {
		t.Fatal(e)
	}
	id := Identity{NodeID: "node-1", DNSName: "test.example.ts.net"}
	s.Identity = id
	if e := os.WriteFile(filepath.Join(s.Active.StateDir(root), "tailscaled.state"), []byte("original identity"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := store.Save(s); e != nil {
		t.Fatal(e)
	}
	d := &fakeDriver{root: root, release: Release{Version: "v1.102.0"}, running: true, slot: s.Active, identity: id}
	engine := NewEngine(store, s, d, &testLogger{}, Options{Enabled: true, Interval: 6 * time.Hour, InitialDelay: time.Millisecond, StopTimeout: time.Second, ValidationTimeout: time.Second})
	return store, engine, d
}
func TestConsecutiveUpdatesReuseInactiveSlot(t *testing.T) {
	store, e, d := fixture(t)
	for _, version := range []string{"v1.102.0", "v1.104.0", "v1.106.0"} {
		old := e.Snapshot().State.Active
		d.release.Version = version
		if err := e.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		state, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if state.AvailableVersion != "" {
			t.Fatal("committed version is still advertised as an update")
		}
		if state.Active.ID == old.ID || state.Active.Version != version || state.PreviousVersion != old.Version {
			t.Fatalf("wrong rotation: %+v", state)
		}
		if state.Identity != d.identity {
			t.Fatal("identity changed")
		}
		if b, err := os.ReadFile(filepath.Join(state.Active.StateDir(store.Root), "tailscaled.state")); err != nil || string(b) != "original identity" {
			t.Fatal("identity not copied", err)
		}
		if state.LastAttempt.Result != "success" || state.LastAttempt.Interruption == nil {
			t.Fatalf("bad result %+v", state.LastAttempt)
		}
	}
	if d.overlap || e.Snapshot().State.Attempts["success"] != 3 {
		t.Fatal("overlap or wrong persistent count")
	}
	before := e.Snapshot().State.LastSuccess
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !e.Snapshot().State.LastSuccess.Equal(before) || e.Snapshot().State.Attempts["success"] != 3 {
		t.Fatal("no-update check became an install")
	}
}
func TestFailedCandidateRestoresUntouchedStateAndQuarantines(t *testing.T) {
	store, e, d := fixture(t)
	d.failVersion = d.release.Version
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := e.Snapshot().State
	if state.Active.Version != "v1.100.0" || state.LastAttempt.Result != "rolled_back" || !state.LastAttempt.HadErrors {
		t.Fatalf("bad rollback %+v", state)
	}
	if !state.LastSuccess.IsZero() || state.PreviousVersion != "" {
		t.Fatal("rollback reported as successful update")
	}
	if _, err := os.Stat(filepath.Join(state.Active.StateDir(store.Root), "candidate-migration")); !os.IsNotExist(err) {
		t.Fatal("candidate modified old state")
	}
	starts := len(d.starts)
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.starts) != starts || state.QuarantinedVersion != d.release.Version {
		t.Fatal("quarantined version retried")
	}
	if d.overlap {
		t.Fatal("production identity overlapped")
	}
}
func TestPreparationFailuresNeverStopProduction(t *testing.T) {
	for _, kind := range []string{"signature", "preflight", "baseline"} {
		t.Run(kind, func(t *testing.T) {
			_, e, d := fixture(t)
			switch kind {
			case "signature":
				d.prepareErr = &CodedError{Code: "signature", Err: errors.New("invalid signature")}
			case "preflight":
				d.preflightErr = errors.New("bad executable")
			case "baseline":
				d.baselineErr = errors.New("no public path")
			}
			if err := e.Check(context.Background()); err != nil {
				t.Fatal(err)
			}
			if d.stops != 0 || len(d.starts) != 0 || !d.running {
				t.Fatal("preparation disrupted production")
			}
			s := e.Snapshot().State
			if s.LastAttempt.Result != "failed" || s.LastAttempt.Interruption == nil || *s.LastAttempt.Interruption != 0 {
				t.Fatal("failure result incorrect")
			}
		})
	}
}
func TestStopTimeoutNeverStartsCandidate(t *testing.T) {
	_, e, d := fixture(t)
	d.stopErr = errors.New("still running")
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.starts) != 0 || !d.running || d.overlap {
		t.Fatal("started overlapping candidate")
	}
	if e.Snapshot().State.LastAttempt.Result != "failed" {
		t.Fatal("wrong timeout result")
	}
}
func TestRollbackFailureHaltsUpdates(t *testing.T) {
	_, e, d := fixture(t)
	d.failAll = true
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := e.Snapshot().State
	if !s.Halted || s.LastAttempt.Result != "rollback_failed" {
		t.Fatal("rollback failure hidden")
	}
	n := d.discoveries
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.discoveries != n {
		t.Fatal("continued updates after rollback failure")
	}
}
func TestInterruptedTransactionsRecoveredExactlyOnce(t *testing.T) {
	for _, phase := range []string{"downloading", "preflighting", "switching", "validating", "rolling_back"} {
		t.Run(phase, func(t *testing.T) {
			store, e, d := fixture(t)
			state := e.Snapshot().State
			a := Attempt{ID: "20261127T120000.000000000", From: state.Active, To: Slot{ID: "b", Version: "v1.102.0"}, Started: time.Now().Add(-time.Minute), Phase: phase}
			if phase == "switching" || phase == "validating" || phase == "rolling_back" {
				a.DisruptionStarted = time.Now().Add(-time.Second)
			}
			if err := store.SaveAttempt(a); err != nil {
				t.Fatal(err)
			}
			recovered := NewEngine(store, state, d, &testLogger{}, e.opts)
			if err := recovered.RecoverBeforeStart(); err != nil {
				t.Fatal(err)
			}
			if err := recovered.CompleteRecovery(context.Background()); err != nil {
				t.Fatal(err)
			}
			s, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if s.Active != state.Active || s.Attempts["interrupted"] != 1 || s.LastAttempt.Result != "interrupted" {
				t.Fatalf("bad recovery %+v", s)
			}
			again := NewEngine(store, s, d, &testLogger{}, e.opts)
			if err := again.RecoverBeforeStart(); err != nil {
				t.Fatal(err)
			}
			if again.Snapshot().State.Attempts["interrupted"] != 1 {
				t.Fatal("recovery counted twice")
			}
		})
	}
}
func TestCommittedCandidateWinsOverStaleTransaction(t *testing.T) {
	store, e, d := fixture(t)
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := e.Snapshot().State
	a := *s.LastAttempt
	a.Result = ""
	a.Phase = "validating"
	if err := store.SaveAttempt(a); err != nil {
		t.Fatal(err)
	}
	recovered := NewEngine(store, s, d, &testLogger{}, e.opts)
	if err := recovered.RecoverBeforeStart(); err != nil {
		t.Fatal(err)
	}
	if recovered.Snapshot().State.Active != s.Active || recovered.Snapshot().State.Attempts["success"] != 1 {
		t.Fatal("committed candidate rolled back or counted twice")
	}
}
func TestDisabledAndUnwritableLogDoNotCheckOrDisrupt(t *testing.T) {
	_, e, d := fixture(t)
	e.opts.Enabled = false
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.discoveries != 0 {
		t.Fatal("disabled feature queried upstream")
	}
	e.opts.Enabled = true
	e.log = &testLogger{failure: errors.New("read-only logs")}
	if err := e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.discoveries != 0 || d.stops != 0 {
		t.Fatal("missing durable logs did not defer update")
	}
}
func TestCorruptStateDoesNotReinitialize(t *testing.T) {
	store, _, _ := fixture(t)
	if err := os.WriteFile(store.Path("active-record"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Seed("/missing", "v1.104.0"); err == nil {
		t.Fatal("corrupt state reinitialized")
	}
}
func TestVersionComparison(t *testing.T) {
	for _, bad := range []string{"v1.104.1-rc1", "1.104.1junk", "01.2.3", "-1.2.3", "1.2", "1.2.3.4", "1.2.3 "} {
		if _, err := ParseVersion(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if !Newer("v1.104.10", "v1.104.9") || Newer("v1.100.0", "v1.104.0") {
		t.Fatal("lexical version comparison")
	}
}
