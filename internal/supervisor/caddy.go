package supervisor

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/emaori/ts-funnel-service/internal/config"
	"github.com/emaori/ts-funnel-service/internal/process"
)

func PrepareCaddy(ctx context.Context, c config.Config) error {
	if !c.CustomCaddy {
		origin := ""
		if c.AllowOrigin {
			origin = "\n        header_down Access-Control-Allow-Origin *\n        header_down Access-Control-Allow-Credentials true"
		}
		host := c.ServiceName
		if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
			host = "[" + host + "]"
		}
		text := fmt.Sprintf(`{
    auto_https off
    servers {
        trusted_proxies static 127.0.0.1 ::1
    }
}

:8080 {
    log {
        output stdout
        format console
    }
    reverse_proxy %s:%s {
        header_up Host {http.request.host}
        header_up X-Forwarded-Proto {http.request.scheme}
        header_up X-Forwarded-For {client_ip}
        header_up X-Real-IP {client_ip}%s
    }
}
`, host, c.ServicePort, origin)
		if err := os.WriteFile(c.CaddyConfig, []byte(text), 0600); err != nil {
			return err
		}
	} else if _, err := os.Stat(c.CaddyConfig); err != nil {
		return fmt.Errorf("custom Caddyfile: %w", err)
	}
	out, err := process.Command(ctx, c.CaddyBin, []string{"validate", "--config", c.CaddyConfig, "--adapter", "caddyfile"})
	if err != nil {
		return fmt.Errorf("Caddyfile validation failed: %w: %s", err, out)
	}
	return nil
}
func caddyReachable(ctx context.Context) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:8080")
	if err != nil {
		return err
	}
	return conn.Close()
}
