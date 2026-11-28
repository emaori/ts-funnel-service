package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/emaori/ts-funnel-service/internal/config"
	"github.com/emaori/ts-funnel-service/internal/observability"
	"github.com/emaori/ts-funnel-service/internal/supervisor"
	"github.com/emaori/ts-funnel-service/internal/updater"
)

var serviceVersion = "development"
var tailscaleVersion = "unknown"
var caddyVersion = "unknown"
var alpineVersion = "unknown"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			if err := supervisor.Healthcheck(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		case "retry-update":
			c, err := config.Load()
			if err == nil && !c.AutoUpdate {
				err = fmt.Errorf("automatic updates are disabled")
			}
			if err == nil {
				err = updater.RequestRetry(c.StateRoot)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Println("Queued a retry of the quarantined version; the supervisor checks requests on its watchdog interval.")
			return
		case "version":
			fmt.Printf("ts-funnel %s (image tailscale=%s caddy=%s alpine=%s)\n", serviceVersion, tailscaleVersion, caddyVersion, alpineVersion)
			return
		default:
			fmt.Fprintln(os.Stderr, "usage: ts-funnel [healthcheck|version|retry-update]")
			os.Exit(2)
		}
	}
	c, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := supervisor.Run(ctx, c, observability.Build{Service: serviceVersion, Tailscale: tailscaleVersion, Caddy: caddyVersion, Alpine: alpineVersion}); err != nil {
		message := err.Error()
		if c.AuthKey != "" {
			message = strings.ReplaceAll(message, c.AuthKey, "[REDACTED]")
		}
		fmt.Fprintln(os.Stderr, "ts-funnel:", message)
		os.Exit(1)
	}
}
