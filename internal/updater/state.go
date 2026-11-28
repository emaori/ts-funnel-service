package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

type Slot struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}
type Identity struct {
	NodeID  string `json:"node_id"`
	DNSName string `json:"dns_name"`
}
type Baseline struct {
	Identity   Identity
	HTTPStatus int
}
type Release struct{ Version, Artifact string }
type ErrorRecord struct {
	At      time.Time `json:"at"`
	Phase   string    `json:"phase"`
	Code    string    `json:"code"`
	Message string    `json:"message"`
}
type Attempt struct {
	ID                string    `json:"id"`
	From              Slot      `json:"from"`
	To                Slot      `json:"to"`
	Started           time.Time `json:"started"`
	Finished          time.Time `json:"finished,omitempty"`
	Phase             string    `json:"phase"`
	Result            string    `json:"result,omitempty"`
	HadErrors         bool      `json:"had_errors"`
	Duration          float64   `json:"duration_seconds"`
	Interruption      *float64  `json:"interruption_seconds,omitempty"`
	DisruptionStarted time.Time `json:"disruption_started,omitempty"`
}
type State struct {
	EnrollmentAllowed   bool              `json:"enrollment_allowed"`
	LastResult          string            `json:"last_result"`
	Schema              int               `json:"schema"`
	Active              Slot              `json:"active"`
	PreviousVersion     string            `json:"previous_version,omitempty"`
	RollbackSlot        string            `json:"rollback_slot,omitempty"`
	Identity            Identity          `json:"identity"`
	LastAttempt         *Attempt          `json:"last_attempt,omitempty"`
	LastSuccess         time.Time         `json:"last_success"`
	LastCheck           time.Time         `json:"last_check"`
	LastSuccessfulCheck time.Time         `json:"last_successful_check"`
	NextCheck           time.Time         `json:"next_check"`
	InstallFailures     int               `json:"install_failures"`
	CheckFailures       int               `json:"check_failures"`
	AvailableVersion    string            `json:"available_version,omitempty"`
	QuarantinedVersion  string            `json:"quarantined_version,omitempty"`
	Halted              bool              `json:"halted"`
	LastError           *ErrorRecord      `json:"last_error,omitempty"`
	Checks              map[string]uint64 `json:"checks"`
	Attempts            map[string]uint64 `json:"attempts"`
	Errors              map[string]uint64 `json:"errors"`
}

func NewState(active Slot) State {
	return State{Schema: 1, Active: active, LastResult: "never", Checks: map[string]uint64{}, Attempts: map[string]uint64{}, Errors: map[string]uint64{}}
}
func (s State) Clone() State {
	data, _ := json.Marshal(s)
	var out State
	_ = json.Unmarshal(data, &out)
	return out
}
func (s State) Validate() error {
	if s.Schema != 1 {
		return fmt.Errorf("unsupported recovery schema %d", s.Schema)
	}
	if err := s.Active.Validate(); err != nil {
		return err
	}
	if s.RollbackSlot != "" && (s.RollbackSlot == s.Active.ID || (s.RollbackSlot != "a" && s.RollbackSlot != "b")) {
		return errors.New("invalid rollback slot")
	}
	if s.LastAttempt != nil {
		if err := s.LastAttempt.Validate(); err != nil {
			return err
		}
	}
	for _, version := range []string{s.PreviousVersion, s.AvailableVersion, s.QuarantinedVersion} {
		if version != "" {
			if _, err := ParseVersion(version); err != nil {
				return err
			}
		}
	}
	if s.LastResult != "never" && !slices.Contains(results, s.LastResult) {
		return errors.New("invalid last result")
	}
	if s.CheckFailures < 0 || s.InstallFailures < 0 {
		return errors.New("negative check failures")
	}
	if s.Checks == nil || s.Attempts == nil || s.Errors == nil {
		return errors.New("missing persistent counters")
	}
	return nil
}

var attemptID = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}$`)
var phases = []string{"idle", "checking", "downloading", "verifying", "preflighting", "switching", "validating", "rolling_back", "recovering"}
var results = []string{"success", "failed", "rolled_back", "rollback_failed", "interrupted"}

func (a Attempt) Validate() error {
	if !attemptID.MatchString(a.ID) || a.Started.IsZero() {
		return errors.New("invalid attempt identity or start time")
	}
	if err := a.From.Validate(); err != nil {
		return err
	}
	if err := a.To.Validate(); err != nil {
		return err
	}
	if a.From.ID == a.To.ID || !Newer(a.To.Version, a.From.Version) {
		return errors.New("invalid slot transition")
	}
	if !slices.Contains(phases, a.Phase) || (a.Result != "" && !slices.Contains(results, a.Result)) {
		return errors.New("invalid attempt phase or result")
	}
	if a.Duration < 0 || (a.Interruption != nil && *a.Interruption < 0) {
		return errors.New("negative attempt duration")
	}
	return nil
}
func (s Slot) Validate() error {
	if s.ID != "a" && s.ID != "b" {
		return fmt.Errorf("invalid slot %q", s.ID)
	}
	if _, err := ParseVersion(s.Version); err != nil {
		return err
	}
	return nil
}
func (s Slot) Bin(root, name string) string {
	return filepath.Join(root, "updater", "slots", s.ID, "bin", name)
}
func (s Slot) StateDir(root string) string {
	return filepath.Join(root, "updater", "slots", s.ID, "state")
}
func (s Slot) Verify(root string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	for _, name := range []string{"tailscale", "tailscaled"} {
		f, err := os.Lstat(s.Bin(root, name))
		if err != nil {
			return err
		}
		if !f.Mode().IsRegular() || f.Mode().Perm()&0111 == 0 {
			return fmt.Errorf("invalid executable in slot %s: %s", s.ID, name)
		}
	}
	f, err := os.Lstat(s.StateDir(root))
	if err != nil {
		return err
	}
	if !f.IsDir() || f.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid slot state directory")
	}
	return verifyArtifact(filepath.Dir(s.Bin(root, "tailscale")), s.Version)
}

type Version struct{ Major, Minor, Patch int }

func ParseVersion(s string) (Version, error) {
	var v Version
	// Require exactly a stable semantic version; never accept a prerelease as a candidate.
	var tail string
	n, _ := fmt.Sscanf(s, "v%d.%d.%d%s", &v.Major, &v.Minor, &v.Patch, &tail)
	if n < 3 {
		n, _ = fmt.Sscanf(s, "%d.%d.%d%s", &v.Major, &v.Minor, &v.Patch, &tail)
	}
	if n != 3 || v.Major < 0 || v.Minor < 0 || v.Patch < 0 || tail != "" {
		return v, fmt.Errorf("invalid release version %q", s)
	}
	canonical := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if s != canonical && s != "v"+canonical {
		return v, fmt.Errorf("invalid release version %q", s)
	}
	return v, nil
}
func Newer(candidate, current string) bool {
	a, e := ParseVersion(candidate)
	if e != nil {
		return false
	}
	b, e := ParseVersion(current)
	if e != nil {
		return false
	}
	if a.Major != b.Major {
		return a.Major > b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor > b.Minor
	}
	return a.Patch > b.Patch
}
