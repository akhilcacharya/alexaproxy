package tailnet

import (
	"net/netip"
	"testing"
)

func TestContains(t *testing.T) {
	for addr, want := range map[string]bool{
		"100.64.0.1":         true,
		"100.127.255.254":    true,
		"100.128.0.1":        false,
		"192.168.1.10":       false,
		"127.0.0.1":          false,
		"::ffff:100.100.1.1": true,
		"fd7a:115c:a1e0::1":  true,
		"fd7a:115c:a1e1::1":  false,
	} {
		if got := Contains(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", addr, got, want)
		}
	}
}
