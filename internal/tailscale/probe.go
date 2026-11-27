package tailscale

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// PublicProbe resolves and dials only globally routable destinations. It never
// uses an HTTP proxy or an ambient tailnet route to validate public Funnel.
type PublicProbe struct {
	Resolver *net.Resolver
	Path     string
}

func NewPublicProbe(dns, path string) *PublicProbe {
	r := net.DefaultResolver
	if dns != "" {
		r = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, dns)
		}}
	}
	return &PublicProbe{Resolver: r, Path: path}
}
func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range []string{"100.64.0.0/10", "198.18.0.0/15", "192.0.0.0/24", "192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32"} {
		if netip.MustParsePrefix(prefix).Contains(ip) {
			return false
		}
	}
	return true
}
func (p *PublicProbe) Check(ctx context.Context, hostname string) (int, error) {
	hostname = strings.TrimSuffix(hostname, ".")
	if !strings.HasSuffix(hostname, ".ts.net") || strings.ContainsAny(hostname, "/:@ \r\n") {
		return 0, errors.New("invalid Funnel hostname")
	}
	ips, err := p.Resolver.LookupNetIP(ctx, "ip", hostname)
	if err != nil {
		return 0, err
	}
	var public []netip.Addr
	for _, ip := range ips {
		if publicAddress(ip) {
			public = append(public, ip)
		}
	}
	if len(public) == 0 {
		return 0, errors.New("Funnel DNS returned no public address; configure TAILSCALE_UPDATE_PROBE_DNS_ADDRESS with a public-view resolver")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil || host != hostname || port != "443" {
				return nil, errors.New("probe tried an unexpected destination")
			}
			var last error
			for _, ip := range public {
				conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				last = err
			}
			return nil, last
		},
		DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	target := url.URL{Scheme: "https", Host: hostname, Path: p.Path}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Cache-Control", "no-cache, no-store")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 500 {
		return resp.StatusCode, fmt.Errorf("public endpoint returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}
