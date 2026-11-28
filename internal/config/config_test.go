package config

import "testing"

func env(extra map[string]string) func(string) (string, bool) {
	m := map[string]string{"TAILSCALE_HOSTNAME": "test", "SERVICE_NAME": "web", "SERVICE_PORT": "3000"}
	for k, v := range extra {
		m[k] = v
	}
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}
func TestDefaultsAndOptOut(t *testing.T) {
	c, e := LoadEnv(env(nil))
	if e != nil {
		t.Fatal(e)
	}
	if !c.AutoUpdate || !c.Metrics || c.CheckInterval.Seconds() != 21600 {
		t.Fatal("bad defaults")
	}
	c, e = LoadEnv(env(map[string]string{"TAILSCALE_AUTO_UPDATE_ENABLED": "false", "STATUS_CHECK_INTERVAL_SECONDS": "0"}))
	if e != nil || c.AutoUpdate || c.StatusInterval != 0 {
		t.Fatal(c, e)
	}
}
func TestInvalidSettings(t *testing.T) {
	for k, values := range map[string][]string{
		"TAILSCALE_AUTO_UPDATE_ENABLED":           {"", "FALSE", "yes"},
		"TAILSCALE_UPDATE_CHECK_INTERVAL_SECONDS": {"0", "-1", "9223372036854775807", "abc"},
		"METRICS_LISTEN_ADDRESS":                  {":abc", ":0", ":65536"},
		"TAILSCALE_UPDATE_PROBE_DNS_ADDRESS":      {":53", "1.1.1.1:foo"},
		"WATCHDOG_INTERVAL_SECONDS":               {"0"}, "UPDATE_LOG_DIR": {"relative"}, "UPDATE_LOG_MAX_FILES": {"0", "101"},
		"SERVICE_NAME":                {"web\nrespond hacked", "web {", "web bad"},
		"TAILSCALE_UPDATE_PROBE_PATH": {"//evil.example", "/?secret=1"},
	} {
		for _, v := range values {
			t.Run(k+"="+v, func(t *testing.T) {
				if _, e := LoadEnv(env(map[string]string{k: v})); e == nil {
					t.Fatal("accepted invalid setting")
				}
			})
		}
	}
}
func TestCustomCaddyDoesNotNeedTarget(t *testing.T) {
	if _, e := LoadEnv(env(map[string]string{"USE_CUSTOM_CADDYFILE": "true", "SERVICE_NAME": "", "SERVICE_PORT": ""})); e != nil {
		t.Fatal(e)
	}
}
