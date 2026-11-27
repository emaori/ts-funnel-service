package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AutoUpdate, Metrics, CustomCaddy, AllowOrigin                             bool
	CheckInterval, ValidationTimeout, WatchdogInterval, StatusInterval        time.Duration
	InitialDelay, StopTimeout, StartTimeout                                   time.Duration
	MetricsAddress, LogDir, StateRoot, Socket, SeedBin, CaddyBin, CaddyConfig string
	AuthKey, Hostname, ServiceName, ServicePort, ProbeDNS, ProbePath          string
	LogMaxBytes                                                               int64
	LogMaxFiles                                                               int
}

func Load() (Config, error) { return LoadEnv(os.LookupEnv) }

func LoadEnv(lookup func(string) (string, bool)) (Config, error) {
	c := Config{
		StateRoot: "/var/lib/tailscale", Socket: "/var/run/tailscale/tailscaled.sock",
		SeedBin: "/usr/local/bin", CaddyBin: "/usr/local/bin/caddy", CaddyConfig: "/etc/caddy/Caddyfile",
		InitialDelay: 2 * time.Minute, StopTimeout: 10 * time.Second, StartTimeout: 30 * time.Second,
	}
	get := func(k, def string) string {
		if v, ok := lookup(k); ok {
			return v
		}
		return def
	}
	boolean := func(k string, def bool) (bool, error) {
		v := get(k, strconv.FormatBool(def))
		if v != "true" && v != "false" {
			return false, fmt.Errorf("%s must be true or false", k)
		}
		return v == "true", nil
	}
	var err error
	for _, setting := range []struct {
		name   string
		target *bool
		def    bool
	}{
		{"TAILSCALE_AUTO_UPDATE_ENABLED", &c.AutoUpdate, true}, {"METRICS_ENABLED", &c.Metrics, true},
		{"USE_CUSTOM_CADDYFILE", &c.CustomCaddy, false}, {"ALLOW_ALL_ORIGIN", &c.AllowOrigin, false},
	} {
		if *setting.target, err = boolean(setting.name, setting.def); err != nil {
			return c, err
		}
	}
	for _, setting := range []struct {
		name   string
		target *time.Duration
		def    int64
		zero   bool
	}{
		{"TAILSCALE_UPDATE_CHECK_INTERVAL_SECONDS", &c.CheckInterval, 21600, false},
		{"TAILSCALE_UPDATE_VALIDATION_TIMEOUT_SECONDS", &c.ValidationTimeout, 60, false},
		{"WATCHDOG_INTERVAL_SECONDS", &c.WatchdogInterval, 30, false},
		{"STATUS_CHECK_INTERVAL_SECONDS", &c.StatusInterval, 300, true},
	} {
		n, e := strconv.ParseInt(get(setting.name, strconv.FormatInt(setting.def, 10)), 10, 64)
		if e != nil || n < 0 || (!setting.zero && n == 0) || n > 31536000 {
			return c, fmt.Errorf("%s must be a valid interval in seconds", setting.name)
		}
		*setting.target = time.Duration(n) * time.Second
	}
	c.MetricsAddress = get("METRICS_LISTEN_ADDRESS", "0.0.0.0:9090")
	if _, p, e := net.SplitHostPort(c.MetricsAddress); e != nil || !validPort(p) {
		return c, fmt.Errorf("METRICS_LISTEN_ADDRESS must be host:port")
	}
	c.LogDir = get("UPDATE_LOG_DIR", filepath.Join(c.StateRoot, "updater", "logs"))
	if !filepath.IsAbs(c.LogDir) {
		return c, fmt.Errorf("UPDATE_LOG_DIR must be absolute")
	}
	c.LogMaxBytes, err = strconv.ParseInt(get("UPDATE_LOG_MAX_SIZE_BYTES", "5242880"), 10, 64)
	if err != nil || c.LogMaxBytes < 1024 || c.LogMaxBytes > 1<<30 {
		return c, fmt.Errorf("UPDATE_LOG_MAX_SIZE_BYTES must be between 1024 and 1073741824")
	}
	n, e := strconv.Atoi(get("UPDATE_LOG_MAX_FILES", "5"))
	if e != nil || n < 1 || n > 100 {
		return c, fmt.Errorf("UPDATE_LOG_MAX_FILES must be between 1 and 100")
	}
	c.LogMaxFiles = n
	c.AuthKey = get("TAILSCALE_AUTHKEY", "")
	c.Hostname = get("TAILSCALE_HOSTNAME", "")
	if c.Hostname == "" || strings.ContainsAny(c.Hostname, "\r\n\x00") {
		return c, fmt.Errorf("TAILSCALE_HOSTNAME is required and must be a single line")
	}
	c.ServiceName = get("SERVICE_NAME", "")
	c.ServicePort = get("SERVICE_PORT", "")
	if !c.CustomCaddy {
		if c.ServiceName == "" || strings.ContainsAny(c.ServiceName, " \t\r\n{}\";\\") {
			return c, fmt.Errorf("SERVICE_NAME must be a hostname or IP address")
		}
		p, e := strconv.Atoi(c.ServicePort)
		if e != nil || p < 1 || p > 65535 {
			return c, fmt.Errorf("SERVICE_PORT must be a TCP port (1-65535)")
		}
	}
	c.ProbeDNS = get("TAILSCALE_UPDATE_PROBE_DNS_ADDRESS", "")
	if c.ProbeDNS != "" {
		if host, port, e := net.SplitHostPort(c.ProbeDNS); e != nil || host == "" || !validPort(port) {
			return c, fmt.Errorf("TAILSCALE_UPDATE_PROBE_DNS_ADDRESS must be host:port")
		}
	}
	c.ProbePath = get("TAILSCALE_UPDATE_PROBE_PATH", "/")
	if !strings.HasPrefix(c.ProbePath, "/") || strings.HasPrefix(c.ProbePath, "//") || strings.ContainsAny(c.ProbePath, "?#\r\n") {
		return c, fmt.Errorf("TAILSCALE_UPDATE_PROBE_PATH must be an absolute URL path without query or fragment")
	}
	return c, nil
}

func validPort(s string) bool { n, e := strconv.Atoi(s); return e == nil && n > 0 && n <= 65535 }
