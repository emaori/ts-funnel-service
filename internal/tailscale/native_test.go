package tailscale

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emaori/ts-funnel-service/internal/config"
	"github.com/emaori/ts-funnel-service/internal/observability"
	"github.com/emaori/ts-funnel-service/internal/updater"
)

func TestNativeRootlessPreflightAndNativeUpdaterDisabled(t *testing.T) {
	bin := os.Getenv("TS_FUNNEL_NATIVE_BIN")
	if bin == "" {
		t.Skip("run tests/Dockerfile.native with the official binaries")
	}
	if os.Geteuid() == 0 {
		t.Fatal("native test must exercise an unprivileged UID")
	}
	dir, e := os.MkdirTemp("", "tsf-native-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	version, e := BinaryVersion(ctx, bin)
	if e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(dir, "state")
	store, e := updater.OpenStore(root)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	state, e := store.Seed(bin, version)
	if e != nil {
		t.Fatal(e)
	}
	var output bytes.Buffer
	log := observability.NewLogger(filepath.Join(dir, "logs"), 1<<20, 2, &output)
	socket := filepath.Join(dir, "api.sock")
	cfg := config.Config{StateRoot: root, Socket: socket, StopTimeout: 3 * time.Second}
	r := &Runtime{Config: cfg, Log: log}
	if e := r.Preflight(ctx, bin, version); e != nil {
		t.Fatal(e)
	}
	if e := r.Start(ctx, state.Active); e != nil {
		t.Fatal(e)
	}
	defer r.Shutdown()
	api := NewAPI(socket)
	defer api.Close()
	var prefs struct{ AutoUpdate struct{ Apply *bool } }
	if e := api.get(ctx, "prefs", &prefs); e != nil {
		t.Fatal(e)
	}
	if prefs.AutoUpdate.Apply == nil || *prefs.AutoUpdate.Apply {
		t.Fatal("native updater is not explicitly disabled")
	}
	st, e := api.Status(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if st.BackendState != "NeedsLogin" || st.Self == nil || st.Self.ID != "" {
		t.Fatal("test unexpectedly enrolled an identity")
	}
	if e := r.Bootstrap(ctx, false); e == nil || !strings.Contains(e.Error(), "refusing automatic new-node enrollment") {
		t.Fatal("persisted node requiring login was silently re-enrolled", e)
	}
	if e := r.Stop(ctx); e != nil {
		t.Fatal(e)
	}
	if r.Running() {
		t.Fatal("daemon stop was not confirmed")
	}
	t.Logf("UID %d: isolated preflight and real daemon %s verified without node registration", os.Geteuid(), version)
}
