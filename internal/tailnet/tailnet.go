// Package tailnet finds this machine's Tailscale address.
package tailnet

import (
	"errors"
	"net"
	"net/netip"
)

var (
	CGNAT = netip.MustParsePrefix("100.64.0.0/10")
	ULA   = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// IPv4 returns the first up interface address in 100.64.0.0/10, so we don't
// need the tailscale CLI (or its socket) to be available.
func IPv4() (netip.Addr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			p, err := netip.ParsePrefix(a.String())
			if err != nil {
				continue
			}
			if ip := p.Addr().Unmap(); CGNAT.Contains(ip) {
				return ip, nil
			}
		}
	}
	return netip.Addr{}, errors.New("no Tailscale address found (is tailscaled up?)")
}

// Contains reports whether addr is a Tailscale address.
func Contains(addr netip.Addr) bool {
	addr = addr.Unmap()
	return CGNAT.Contains(addr) || ULA.Contains(addr)
}
