package egress

import (
	"net/netip"
	"testing"
)

func TestBlockedAddresses(t *testing.T) {
	blocked := []string{
		// IPv4: first, last and typical addresses of every special-purpose range.
		"0.0.0.0", "0.255.255.255", "10.0.0.1", "10.255.255.255", "100.64.0.1", "100.127.255.255",
		"127.0.0.1", "127.255.255.255", "169.254.0.1", "169.254.169.254", "172.16.0.1", "172.31.255.255",
		"192.0.0.1", "192.0.2.1", "192.88.99.1", "192.168.0.1", "192.168.255.255", "198.18.0.1", "198.19.255.255",
		"198.51.100.1", "203.0.113.1", "224.0.0.1", "239.255.255.255", "240.0.0.1", "255.255.255.254", "255.255.255.255",
		// IPv6 special-purpose and non-global ranges.
		"::", "::1", "::2", "100::1", "2001::1", "2001:1::1", "2001:db8::1", "2002::1", "2002:7f00:1::1",
		"3fff::1", "fc00::1", "fd00:ec2::254", "fe80::1", "febf::1", "fec0::1", "ff02::1", "ff00::1",
		// Outside 2000::/3: default deny.
		"4000::1", "8000::1", "c000::1", "e000::1",
		// IPv4 carried inside IPv6 must be judged as IPv4, or by the IPv6 rules.
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "::ffff:192.168.1.1", "::ffff:0.0.0.0",
		"::127.0.0.1", "::10.0.0.1", "64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b:1::1",
	}
	for _, s := range blocked {
		addr := netip.MustParseAddr(s)
		if reason := BlockedReason(addr); reason == "" {
			t.Errorf("%s must be blocked", s)
		}
		if IsAllowedIP(addr) {
			t.Errorf("IsAllowedIP(%s) = true", s)
		}
	}
}

func TestAllowedAddresses(t *testing.T) {
	allowed := []string{
		"1.1.1.1", "8.8.8.8", "9.9.9.9", "93.184.216.34", "11.0.0.1", "126.255.255.255", "128.0.0.1",
		// Neighbours just outside blocked ranges.
		"172.15.255.255", "172.32.0.1", "100.63.255.255", "100.128.0.1", "169.253.255.255", "169.255.0.1",
		"192.0.1.1", "192.0.3.1", "198.17.255.255", "198.20.0.1", "223.255.255.255", "9.255.255.255", "192.167.255.255", "192.169.0.1",
		"2000::1", "2606:4700:4700::1111", "2001:4860:4860::8888", "2a00:1450:4001::1", "2001:200::1", "2620:fe::fe",
	}
	for _, s := range allowed {
		addr := netip.MustParseAddr(s)
		if reason := BlockedReason(addr); reason != "" {
			t.Errorf("%s must be allowed, blocked as %q", s, reason)
		}
		if !IsAllowedIP(addr) {
			t.Errorf("IsAllowedIP(%s) = false", s)
		}
	}
}

func TestZonedAndInvalidAddressesAreBlocked(t *testing.T) {
	if BlockedReason(netip.MustParseAddr("fe80::1%eth0")) == "" || BlockedReason(netip.MustParseAddr("2606:4700:4700::1111%eth0")) == "" {
		t.Error("zoned addresses must be blocked")
	}
	if BlockedReason(netip.Addr{}) == "" {
		t.Error("the zero Addr must be blocked")
	}
}

func TestMappedIPv4IsJudgedLikeIPv4(t *testing.T) {
	// A public IPv4 address wrapped in IPv6 is allowed exactly as the bare address is.
	if !IsAllowedIP(netip.MustParseAddr("::ffff:8.8.8.8")) {
		t.Error("::ffff:8.8.8.8 must be allowed like 8.8.8.8")
	}
	for _, private := range []string{"10.1.2.3", "127.0.0.1", "169.254.169.254"} {
		bare := IsAllowedIP(netip.MustParseAddr(private))
		mapped := IsAllowedIP(netip.MustParseAddr("::ffff:" + private))
		if bare || mapped {
			t.Errorf("%s: bare allowed=%v mapped allowed=%v", private, bare, mapped)
		}
	}
}

func TestBlockedReasonNamesTheRange(t *testing.T) {
	cases := map[string]string{
		"10.1.1.1": "private", "127.0.0.1": "loopback", "169.254.169.254": "link-local", "224.0.0.1": "multicast",
		"::1": "loopback", "fe80::1": "link-local", "fc00::1": "unique-local", "ff02::1": "multicast",
	}
	for ip, want := range cases {
		if got := BlockedReason(netip.MustParseAddr(ip)); got != want {
			t.Errorf("BlockedReason(%s) = %q, want %q", ip, got, want)
		}
	}
}

func TestFilterAllRequiresEveryAddressToBeAllowed(t *testing.T) {
	pub := netip.MustParseAddr("8.8.8.8")
	pub6 := netip.MustParseAddr("2606:4700:4700::1111")
	priv := netip.MustParseAddr("10.0.0.1")

	if got, err := RequireAllAllowed([]netip.Addr{pub, pub6}); err != nil || len(got) != 2 {
		t.Fatalf("all-public answers must pass: %v %v", got, err)
	}
	// A mixed DNS answer is rejected as a whole: an attacker-controlled name that
	// returns one public and one private address must not be usable.
	if _, err := RequireAllAllowed([]netip.Addr{pub, priv}); err == nil {
		t.Fatal("a mixed answer must be rejected")
	}
	if _, err := RequireAllAllowed(nil); err == nil {
		t.Fatal("an empty answer must be rejected")
	}
	// Mapped addresses are normalized in the result.
	got, err := RequireAllAllowed([]netip.Addr{netip.MustParseAddr("::ffff:8.8.8.8")})
	if err != nil || got[0] != pub {
		t.Fatalf("mapped address must be unmapped: %v %v", got, err)
	}
}
