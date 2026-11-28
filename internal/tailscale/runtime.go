package tailscale

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/emaori/ts-funnel-service/internal/config"
	"github.com/emaori/ts-funnel-service/internal/observability"
	"github.com/emaori/ts-funnel-service/internal/process"
	"github.com/emaori/ts-funnel-service/internal/updater"
)

type Prober interface {
	Check(context.Context, string) (int, error)
}
type Runtime struct {
	Config       config.Config
	Distribution *Distribution
	Log          *observability.Logger
	Probe        Prober
	CaddyAlive   func() bool
	Report       func(string, string, error)
	mu           sync.Mutex
	child        *process.Child
	slot         updater.Slot
	api          *API
}

var shortVersion = regexp.MustCompile(`^v?([0-9]+\.[0-9]+\.[0-9]+)(?:[-\s]|$)`)

func NormalizeVersion(s string) (string, error) {
	m := shortVersion.FindStringSubmatch(strings.TrimSpace(s))
	if len(m) != 2 {
		return "", fmt.Errorf("unrecognized binary version")
	}
	v := "v" + m[1]
	if _, err := updater.ParseVersion(v); err != nil {
		return "", err
	}
	return v, nil
}
func BinaryVersion(ctx context.Context, binDir string) (string, error) {
	var version string
	for _, entry := range []struct {
		name string
		args []string
	}{{"tailscale", []string{"version"}}, {"tailscaled", []string{"--version"}}} {
		out, err := process.Command(ctx, filepath.Join(binDir, entry.name), entry.args)
		if err != nil {
			return "", err
		}
		v, err := NormalizeVersion(string(out))
		if err != nil {
			return "", err
		}
		if version != "" && v != version {
			return "", errors.New("tailscale/tailscaled version mismatch")
		}
		version = v
	}
	return version, nil
}
func (r *Runtime) Discover(ctx context.Context) (updater.Release, error) {
	return r.Distribution.Discover(ctx)
}
func (r *Runtime) Prepare(ctx context.Context, release updater.Release, stage string) error {
	r.Log.Printf("component=tailscale phase=downloading target=%s artifact=%s", release.Version, release.Artifact)
	err := r.Distribution.Prepare(ctx, release, stage)
	if err == nil {
		r.Log.Printf("component=tailscale phase=verifying target=%s signature=verified", release.Version)
	}
	return err
}
func (r *Runtime) Preflight(ctx context.Context, binDir, version string) error {
	actual, err := BinaryVersion(ctx, binDir)
	if err != nil {
		return err
	}
	if actual != version {
		return errors.New("candidate binary version differs from release metadata")
	}
	dir, err := os.MkdirTemp(filepath.Dir(r.Config.Socket), "preflight-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "api.sock")
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0700); err != nil {
		return err
	}
	child, err := process.Start(filepath.Join(binDir, "tailscaled"), daemonArgs(state, sock), &observability.ChildWriter{Logger: r.Log, Source: "preflight"})
	if err != nil {
		return err
	}
	defer child.Shutdown(r.Config.StopTimeout)
	api := NewAPI(sock)
	defer api.Close()
	err = waitFor(ctx, 250*time.Millisecond, func() error {
		if !child.Running() {
			return fmt.Errorf("preflight daemon exited: %v", child.Err())
		}
		status, err := api.Status(ctx)
		if err != nil {
			return err
		}
		v, err := NormalizeVersion(status.Version)
		if err != nil || v != version {
			return errors.New("preflight LocalAPI version mismatch")
		}
		if status.BackendState != "NeedsLogin" && status.BackendState != "NoState" && status.BackendState != "Stopped" {
			return fmt.Errorf("unexpected preflight state %s", status.BackendState)
		}
		return nil
	})
	if err != nil {
		return err
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), r.Config.StopTimeout)
	defer cancel()
	return child.Stop(stopCtx)
}
func daemonArgs(state, socket string) []string {
	return []string{"--statedir=" + state, "--state=" + filepath.Join(state, "tailscaled.state"), "--socket=" + socket, "--tun=userspace-networking", "--port=0"}
}
func (r *Runtime) Start(ctx context.Context, slot updater.Slot) error {
	r.mu.Lock()
	if r.child != nil && r.child.Running() {
		r.mu.Unlock()
		return errors.New("refusing overlapping production daemons")
	}
	if r.api != nil {
		r.api.Close()
	}
	if err := os.Remove(r.Config.Socket); err != nil && !os.IsNotExist(err) {
		r.mu.Unlock()
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.Config.Socket), 0700); err != nil {
		r.mu.Unlock()
		return err
	}
	child, err := process.Start(slot.Bin(r.Config.StateRoot, "tailscaled"), daemonArgs(slot.StateDir(r.Config.StateRoot), r.Config.Socket), &observability.ChildWriter{Logger: r.Log, Source: "tailscaled"})
	if err != nil {
		r.mu.Unlock()
		return err
	}
	r.child = child
	r.slot = slot
	r.api = NewAPI(r.Config.Socket)
	api := r.api
	r.mu.Unlock()
	r.Log.Printf("component=tailscale phase=startup slot=%s version=%s pid=%d", slot.ID, slot.Version, child.PID())
	if err := waitFor(ctx, 250*time.Millisecond, func() error {
		if !child.Running() {
			return fmt.Errorf("daemon exited during startup: %v", child.Err())
		}
		st, err := api.Status(ctx)
		if err != nil {
			return err
		}
		v, err := NormalizeVersion(st.Version)
		if err != nil || v != slot.Version {
			return errors.New("daemon LocalAPI version mismatch")
		}
		return nil
	}); err != nil {
		return &updater.CodedError{Code: "startup", Err: err}
	}
	if _, err := r.cli(ctx, "set", "--auto-update=false"); err != nil {
		return &updater.CodedError{Code: "startup", Err: fmt.Errorf("disable native updater: %w", err)}
	}
	return nil
}
func (r *Runtime) cli(ctx context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	slot := r.slot
	r.mu.Unlock()
	args = append([]string{"--socket=" + r.Config.Socket}, args...)
	out, err := process.Command(ctx, slot.Bin(r.Config.StateRoot, "tailscale"), args)
	if len(out) > 0 {
		r.Log.Printf("component=tailscale cli_output=%s", string(out))
	}
	return out, err
}
func (r *Runtime) Bootstrap(ctx context.Context, allowEnrollment bool) error {
	r.mu.Lock()
	api := r.api
	r.mu.Unlock()
	st, err := api.Status(ctx)
	if err != nil {
		return err
	}
	if st.BackendState == "NeedsLogin" || st.BackendState == "NoState" {
		if !allowEnrollment {
			return errors.New("persisted identity requires reauthentication; refusing automatic new-node enrollment")
		}
		if r.Config.AuthKey == "" {
			return errors.New("TAILSCALE_AUTHKEY is required to enroll a new node")
		}
		if _, err := r.cli(ctx, "up", "--authkey="+r.Config.AuthKey, "--hostname="+r.Config.Hostname, "--accept-dns=false"); err != nil {
			return err
		}
	} else {
		if _, err := r.cli(ctx, "set", "--hostname="+r.Config.Hostname, "--accept-dns=false", "--auto-update=false"); err != nil {
			return err
		}
		if st.BackendState == "Stopped" {
			if _, err := r.cli(ctx, "up"); err != nil {
				return err
			}
		}
	}
	if err := waitFor(ctx, time.Second, func() error {
		st, err := api.Status(ctx)
		if err != nil {
			return err
		}
		if st.BackendState != "Running" || st.Self == nil || st.Self.ID == "" || st.Self.DNSName == "" {
			return fmt.Errorf("waiting for connected node (%s)", st.BackendState)
		}
		return nil
	}); err != nil {
		return err
	}
	_, err = r.cli(ctx, "funnel", "--bg", "--https=443", "--set-path=/", "http://127.0.0.1:8080")
	return err
}
func (r *Runtime) Stop(ctx context.Context) error {
	r.mu.Lock()
	c := r.child
	r.mu.Unlock()
	if c == nil {
		return nil
	}
	r.Log.Printf("component=tailscale phase=stopping pid=%d", c.PID())
	if err := c.Stop(ctx); err != nil {
		return err
	}
	r.Log.Printf("component=tailscale phase=stopped pid=%d", c.PID())
	return nil
}
func (r *Runtime) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.child != nil && r.child.Running()
}
func (r *Runtime) Shutdown() {
	r.mu.Lock()
	c := r.child
	r.mu.Unlock()
	if c != nil {
		_ = c.Shutdown(r.Config.StopTimeout)
	}
}
func (r *Runtime) Observe(ctx context.Context) (updater.Observation, error) {
	r.mu.Lock()
	child := r.child
	api := r.api
	slot := r.slot
	r.mu.Unlock()
	o := updater.Observation{Running: child != nil && child.Running()}
	if !o.Running || api == nil {
		return o, errors.New("production daemon is not running")
	}
	st, err := api.Status(ctx)
	if err != nil {
		return o, err
	}
	o.Version, _ = NormalizeVersion(st.Version)
	if st.Self != nil {
		o.Identity = updater.Identity{NodeID: st.Self.ID, DNSName: strings.TrimSuffix(st.Self.DNSName, ".")}
	}
	if st.BackendState != "Running" || o.Version != slot.Version || o.Identity.NodeID == "" {
		return o, errors.New("production daemon is not ready")
	}
	serve, err := api.Serve(ctx)
	if err != nil {
		return o, err
	}
	target := o.Identity.DNSName + ":443"
	if !serve.AllowFunnel[target] || !serve.TCP["443"].HTTPS || serve.Web[target].Handlers["/"].Proxy != "http://127.0.0.1:8080" {
		return o, &updater.CodedError{Code: "funnel_config", Err: errors.New("expected background Funnel forwarding is missing")}
	}
	if r.CaddyAlive != nil && !r.CaddyAlive() {
		return o, errors.New("Caddy is not running")
	}
	c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", "127.0.0.1:8080")
	if err != nil {
		return o, err
	}
	c.Close()
	o.Ready = true
	o.ProbeAt = time.Now().UTC()
	_, err = r.Probe.Check(ctx, o.Identity.DNSName)
	o.ProbeOK = err == nil
	return o, err
}
func (r *Runtime) Baseline(ctx context.Context) (updater.Baseline, error) {
	o, err := r.Observe(ctx)
	if err != nil {
		return updater.Baseline{}, err
	}
	status, err := r.Probe.Check(ctx, o.Identity.DNSName)
	if err != nil {
		return updater.Baseline{}, err
	}
	return updater.Baseline{Identity: o.Identity, HTTPStatus: status}, nil
}
func (r *Runtime) Validate(ctx context.Context, slot updater.Slot, base updater.Baseline) error {
	consecutive := 0
	reported := map[string]bool{}
	return waitFor(ctx, time.Second, func() error {
		o, err := r.Observe(ctx)
		if err == nil && o.Identity != base.Identity {
			err = &updater.CodedError{Code: "identity_mismatch", Err: errors.New("candidate identity differs from outgoing daemon")}
		}
		if err == nil && o.Version != slot.Version {
			err = errors.New("candidate daemon version mismatch")
		}
		if err == nil {
			status, probeErr := r.Probe.Check(ctx, base.Identity.DNSName)
			err = probeErr
			if err == nil && status != base.HTTPStatus {
				err = fmt.Errorf("public response changed from HTTP %d to %d", base.HTTPStatus, status)
			}
		}
		if err != nil {
			consecutive = 0
			code := updater.ErrorCode(err, "public_probe")
			if !reported[code] && r.Report != nil {
				r.Report("validating", code, err)
				reported[code] = true
			}
			return err
		}
		consecutive++
		if consecutive < 3 {
			return errors.New("waiting for consecutive successful validation probes")
		}
		return nil
	})
}
func waitFor(ctx context.Context, interval time.Duration, check func() error) error {
	var last error
	for {
		if ctx.Err() != nil {
			if last != nil {
				return fmt.Errorf("%w: %v", ctx.Err(), last)
			}
			return ctx.Err()
		}
		if last = check(); last == nil {
			return nil
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w: %v", ctx.Err(), last)
		case <-t.C:
		}
	}
}
