package tailscale

import (
	"net/netip"
	"testing"
)

func parseAddress(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, e := netip.ParseAddr(s)
	if e != nil {
		t.Fatal(e)
	}
	return a
}
