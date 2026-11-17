// Package egress holds the pure rules that decide where RelayForge may send
// outbound requests: which IP addresses are acceptable and which destination
// URLs may be stored.
//
// Nothing here performs network I/O. Validating a URL when it is stored is only
// a first filter, because DNS answers change. The decisive control is applying
// BlockedReason to the addresses the connection actually uses, at connect time
// (see ADR 0023).
package egress

import (
	"errors"
	"fmt"
	"net/netip"
)

type blockedRange struct {
	prefix netip.Prefix
	reason string
}

func rng(cidr, reason string) blockedRange {
	return blockedRange{prefix: netip.MustParsePrefix(cidr), reason: reason}
}

// blockedIPv4 lists the IPv4 special-purpose ranges. Everything else is global unicast.
var blockedIPv4 = []blockedRange{
	rng("0.0.0.0/8", "unspecified"),
	rng("10.0.0.0/8", "private"),
	rng("100.64.0.0/10", "carrier-grade-nat"),
	rng("127.0.0.0/8", "loopback"),
	rng("169.254.0.0/16", "link-local"),
	rng("172.16.0.0/12", "private"),
	rng("192.0.0.0/24", "ietf-protocol"),
	rng("192.0.2.0/24", "documentation"),
	rng("192.88.99.0/24", "6to4-relay"),
	rng("192.168.0.0/16", "private"),
	rng("198.18.0.0/15", "benchmarking"),
	rng("198.51.100.0/24", "documentation"),
	rng("203.0.113.0/24", "documentation"),
	rng("224.0.0.0/4", "multicast"),
	rng("240.0.0.0/4", "reserved"), // includes 255.255.255.255
}

// blockedIPv6 lists IPv6 ranges that are not global unicast. Addresses outside
// 2000::/3 are denied by default as well, so a range missing here fails closed.
var blockedIPv6 = []blockedRange{
	rng("::1/128", "loopback"),
	rng("::/8", "unspecified"), // ::, and the deprecated IPv4-compatible ::a.b.c.d
	rng("64:ff9b::/96", "nat64"),
	rng("64:ff9b:1::/48", "nat64"),
	rng("100::/64", "discard"),
	rng("2001::/23", "ietf-protocol"), // includes Teredo 2001::/32
	rng("2001:db8::/32", "documentation"),
	rng("2002::/16", "6to4"),
	rng("3fff::/20", "documentation"),
	rng("fc00::/7", "unique-local"),
	rng("fe80::/10", "link-local"),
	rng("fec0::/10", "site-local"),
	rng("ff00::/8", "multicast"),
}

var globalUnicastIPv6 = netip.MustParsePrefix("2000::/3")

// BlockedReason returns why addr must not be contacted, or "" if it is an
// acceptable public address. IPv4-mapped IPv6 addresses (::ffff:a.b.c.d) are
// judged as the IPv4 address they carry. The zero Addr and zoned addresses are
// blocked.
func BlockedReason(addr netip.Addr) string {
	if !addr.IsValid() {
		return "invalid"
	}
	if addr.Zone() != "" {
		return "zoned"
	}
	addr = addr.Unmap()

	if addr.Is4() {
		for _, r := range blockedIPv4 {
			if r.prefix.Contains(addr) {
				return r.reason
			}
		}
		return ""
	}
	for _, r := range blockedIPv6 {
		if r.prefix.Contains(addr) {
			return r.reason
		}
	}
	if !globalUnicastIPv6.Contains(addr) {
		return "unassigned"
	}
	return ""
}

// IsAllowedIP reports whether addr is an acceptable public address.
func IsAllowedIP(addr netip.Addr) bool { return BlockedReason(addr) == "" }

// ErrBlockedAddress is wrapped by every address-policy rejection.
var ErrBlockedAddress = errors.New("destination address is not allowed")

// RequireAllAllowed checks a complete DNS answer. It returns the addresses with
// IPv4-mapped forms unwrapped, or an error if the answer is empty or if ANY
// address is blocked. Rejecting mixed answers closes the case where a
// destination name resolves to both a public and an internal address.
func RequireAllAllowed(addrs []netip.Addr) ([]netip.Addr, error) {
	return requireAllWith(addrs, BlockedReason)
}

// requireAllWith is RequireAllAllowed with an injectable policy, used only by tests in this package.
func requireAllWith(addrs []netip.Addr, policy func(netip.Addr) string) ([]netip.Addr, error) {
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: no addresses", ErrBlockedAddress)
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if reason := policy(a); reason != "" {
			return nil, fmt.Errorf("%w: %s", ErrBlockedAddress, reason)
		}
		out = append(out, a.Unmap())
	}
	return out, nil
}
