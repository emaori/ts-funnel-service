package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emaori/ts-funnel-service/internal/config"
)

func TestNativeGeneratedAndReadOnlyCustomCaddyfile(t *testing.T) {
	bin := os.Getenv("TS_FUNNEL_NATIVE_CADDY")
	if bin == "" {
		t.Skip("native image supplies official Caddy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "Caddyfile")
	c := config.Config{CaddyBin: bin, CaddyConfig: path, ServiceName: "web", ServicePort: "3000", AllowOrigin: true}
	if e := PrepareCaddy(ctx, c); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"trusted_proxies static 127.0.0.1 ::1", "header_up X-Forwarded-For {client_ip}", "header_down Access-Control-Allow-Origin *"} {
		if !strings.Contains(string(b), want) {
			t.Fatal("lost existing Caddy behavior", want)
		}
	}
	if e := os.Chmod(path, 0400); e != nil {
		t.Fatal(e)
	}
	c.CustomCaddy = true
	if e := PrepareCaddy(ctx, c); e != nil {
		t.Fatal(e)
	}
	after, e := os.ReadFile(path)
	if e != nil || string(after) != string(b) {
		t.Fatal("custom read-only config changed")
	}
}
