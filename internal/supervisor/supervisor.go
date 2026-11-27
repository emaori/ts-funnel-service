package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emaori/ts-funnel-service/internal/config"
	"github.com/emaori/ts-funnel-service/internal/observability"
	"github.com/emaori/ts-funnel-service/internal/process"
	"github.com/emaori/ts-funnel-service/internal/tailscale"
	"github.com/emaori/ts-funnel-service/internal/updater"
)

const readyFile = "/var/run/tailscale/supervisor-ready"

func Run(ctx context.Context, c config.Config, build observability.Build) (runErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	store, err := updater.OpenStore(c.StateRoot)
	if err != nil {
		return err
	}
	defer store.Close()
	log := observability.NewLogger(c.LogDir, c.LogMaxBytes, c.LogMaxFiles, os.Stdout, c.AuthKey)
	defer func() {
		if runErr != nil {
			log.Printf("ERROR component=supervisor result=stopped %v", runErr)
		} else {
			log.Printf("component=supervisor result=stopped")
		}
	}()
	defer os.Remove(readyFile)
	log.Printf("component=supervisor starting service=%s auto_update_enabled=%t", c.Hostname, c.AutoUpdate)
	state, err := store.Load()
	if os.IsNotExist(err) {
		checkCtx, done := context.WithTimeout(ctx, 10*time.Second)
		version, e := tailscale.BinaryVersion(checkCtx, c.SeedBin)
		done()
		if e != nil {
			return e
		}
		state, err = store.Seed(c.SeedBin, version)
	} else if err == nil {
		if e := updater.AtomicFile(store.Path("bootstrap-complete"), []byte("1\n"), 0600); e != nil {
			return e
		}
	}
	if err != nil {
		return fmt.Errorf("load/seed persisted runtime: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(c.CaddyConfig), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.Socket), 0700); err != nil {
		return err
	}
	distro, err := tailscale.NewDistribution()
	if err != nil {
		return err
	}
	driver := &tailscale.Runtime{Config: c, Distribution: distro, Log: log, Probe: tailscale.NewPublicProbe(c.ProbeDNS, c.ProbePath)}
	engine := updater.NewEngine(store, state, driver, log, updater.Options{
		Enabled: c.AutoUpdate, Interval: c.CheckInterval, InitialDelay: c.InitialDelay, StopTimeout: c.StopTimeout, ValidationTimeout: c.ValidationTimeout,
	})
	driver.Report = engine.Error
	if err := engine.RecoverBeforeStart(); err != nil {
		return err
	}
	// Only uncommitted download scratch is discarded, under the volume lock.
	if err := os.RemoveAll(store.Path("staging")); err != nil {
		return err
	}
	var server *http.Server
	if c.Metrics {
		listener, err := net.Listen("tcp", c.MetricsAddress)
		if err != nil {
			log.Printf("ERROR component=metrics listener unavailable: %v", err)
		} else {
			server = &http.Server{Handler: observability.Handler(observability.NewCollector(engine.Snapshot, build)), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 65536}
			go func() {
				if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Printf("ERROR component=metrics %v", err)
				}
			}()
			log.Printf("component=metrics listening=%s path=/metrics", listener.Addr())
			defer func() {
				shutdown, done := context.WithTimeout(context.Background(), 3*time.Second)
				defer done()
				_ = server.Shutdown(shutdown)
			}()
		}
	}
	setupCtx, setupCancel := context.WithTimeout(ctx, 30*time.Second)
	err = PrepareCaddy(setupCtx, c)
	setupCancel()
	if err != nil {
		return err
	}
	caddy, err := process.Start(c.CaddyBin, []string{"run", "--config", c.CaddyConfig, "--adapter", "caddyfile"}, os.Stdout)
	if err != nil {
		return err
	}
	defer caddy.Shutdown(c.StopTimeout)
	driver.CaddyAlive = caddy.Running
	for deadline := time.Now().Add(10 * time.Second); ; {
		checkCtx, done := context.WithTimeout(ctx, time.Second)
		err = caddyReachable(checkCtx)
		done()
		if err == nil {
			break
		}
		if !caddy.Running() {
			return fmt.Errorf("Caddy exited: %v", caddy.Err())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Caddy readiness timeout: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	defer driver.Shutdown()
	active := engine.Snapshot().State.Active
	allowEnrollment := engine.Snapshot().State.EnrollmentAllowed && engine.Snapshot().State.Identity.NodeID == ""
	startupErr := func() error {
		startCtx, startCancel := context.WithTimeout(ctx, c.StartTimeout)
		err = driver.Start(startCtx, active)
		startCancel()
		if err != nil {
			return err
		}
		bootstrapCtx, bootstrapCancel := context.WithTimeout(ctx, 2*time.Minute)
		err = driver.Bootstrap(bootstrapCtx, allowEnrollment)
		bootstrapCancel()
		if err != nil {
			return err
		}
		observeCtx, done := context.WithTimeout(ctx, 15*time.Second)
		observation, observeErr := driver.Observe(observeCtx)
		done()
		engine.SetObservation(observation)
		expected := engine.Snapshot().State.Identity
		if expected.NodeID != "" && observation.Identity != expected {
			return errors.New("startup identity differs from committed node; refusing to accept it")
		}
		if observation.Identity.NodeID == "" {
			return errors.New("startup did not establish a node identity")
		}
		if err := engine.SetIdentity(observation.Identity); err != nil {
			return err
		}
		if observeErr != nil {
			log.Printf("WARNING component=tailscale initial public validation: %v", observeErr)
		}
		return nil
	}()
	if startupErr != nil {
		if !engine.Snapshot().Busy {
			return startupErr
		}
		if err := engine.FailRecovery(startupErr); err != nil {
			return err
		}
	}
	recoveryCtx, recoveryCancel := context.WithTimeout(ctx, c.ValidationTimeout)
	err = engine.CompleteRecovery(recoveryCtx)
	recoveryCancel()
	if err != nil {
		return err
	}
	updateDone := make(chan error, 1)
	updateFinished := make(chan struct{})
	go func() { defer close(updateFinished); updateDone <- engine.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-updateFinished:
		case <-time.After(c.StopTimeout + c.ValidationTimeout + 35*time.Second):
			log.Printf("WARNING component=supervisor update shutdown deadline exceeded; recovery journal retained")
		}
	}()
	if !engine.Snapshot().State.Halted {
		if err := updater.AtomicFile(readyFile, []byte("1\n"), 0600); err != nil {
			return err
		}
	}
	log.Printf("component=supervisor setup_complete=true running_version=%s", active.Version)
	watch := time.NewTicker(c.WatchdogInterval)
	defer watch.Stop()
	lastStatus := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-caddy.Done():
			return fmt.Errorf("Caddy died: %v", caddy.Err())
		case err := <-updateDone:
			// A persistence failure stops new updates without stopping serving.
			if err != nil {
				log.Printf("ERROR component=updater scheduling stopped: %v", err)
				engine.HaltScheduling(err)
				_ = os.Remove(readyFile)
			}
			updateDone = nil
		case <-watch.C:
			if retryErr := engine.ConsumeRetry(); retryErr != nil {
				log.Printf("WARNING component=updater manual retry: %v", retryErr)
			}
			if !caddy.Running() {
				return fmt.Errorf("Caddy died: %v", caddy.Err())
			}
			engine.WithIdle(ctx, func(idleCtx context.Context) {
				snapshot := engine.Snapshot()
				if !driver.Running() && !snapshot.State.Halted {
					err = errors.New("tailscaled died outside an update")
					cancel()
					return
				}
				if c.StatusInterval > 0 && time.Since(lastStatus) >= c.StatusInterval {
					statusCtx, done := context.WithTimeout(idleCtx, 15*time.Second)
					o, e := driver.Observe(statusCtx)
					done()
					engine.SetObservation(o)
					lastStatus = time.Now()
					if e != nil {
						log.Printf("WARNING component=tailscale connectivity degraded: %v", e)
					}
				}
				if snapshot.State.Halted {
					_ = os.Remove(readyFile)
				} else if _, markerErr := os.Stat(readyFile); os.IsNotExist(markerErr) {
					_ = updater.AtomicFile(readyFile, []byte("1\n"), 0600)
				}
			})
			if err != nil {
				return err
			}
		}
	}
}

func Healthcheck() error {
	b, err := os.ReadFile(readyFile)
	if err != nil || strings.TrimSpace(string(b)) != "1" {
		return errors.New("supervisor is not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return caddyReachable(ctx)
}
