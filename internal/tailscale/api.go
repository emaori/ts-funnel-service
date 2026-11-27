package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type Status struct {
	Version      string
	BackendState string
	Self         *struct {
		ID      string
		DNSName string
		Online  bool
	}
}
type ServeConfig struct {
	AllowFunnel map[string]bool
	Web         map[string]struct {
		Handlers map[string]struct{ Proxy string }
	}
	TCP map[string]struct{ HTTPS bool }
}
type API struct{ client *http.Client }

func NewAPI(socket string) *API {
	t := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &API{client: &http.Client{Transport: t, Timeout: 5 * time.Second}}
}
func (a *API) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://local-tailscaled.sock/localapi/v0/"+path, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("LocalAPI %s returned HTTP %d", strings.Split(path, "?")[0], resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}
func (a *API) Status(ctx context.Context) (Status, error) {
	var s Status
	err := a.get(ctx, "status?peers=false", &s)
	return s, err
}
func (a *API) Serve(ctx context.Context) (ServeConfig, error) {
	var s ServeConfig
	err := a.get(ctx, "serve-config", &s)
	return s, err
}
func (a *API) Close() { a.client.CloseIdleConnections() }
