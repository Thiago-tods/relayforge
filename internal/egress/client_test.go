package egress

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- test infrastructure: a controlled resolver, a private CA and local TLS servers ----

type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr // host -> answer per call; the last one repeats
	err     error
	block   bool // block until the context ends
	calls   map[string]int
}

func newResolver(host string, answers ...[]netip.Addr) *fakeResolver {
	return &fakeResolver{answers: map[string][][]netip.Addr{host: answers}, calls: map[string]int{}}
}

func (r *fakeResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	r.calls[host]++
	n := r.calls[host]
	block, err := r.block, r.err
	answers := r.answers[host]
	r.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if len(answers) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	if n > len(answers) {
		n = len(answers)
	}
	return answers[n-1], nil
}

func (r *fakeResolver) callCount(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

func ips(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, v := range s {
		out[i] = netip.MustParseAddr(v)
	}
	return out
}

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "RelayForge test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

func (ca *testCA) leaf(t *testing.T, dnsNames ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: dnsNames[0]}, DNSNames: dnsNames,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

type seen struct {
	mu         sync.Mutex
	hits       int
	host       string
	method     string
	path       string
	body       string
	header     http.Header
	serverName string
}

func (s *seen) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.hits }

// startServer starts a local TLS server on 127.0.0.1 whose certificate covers dnsNames.
func startServer(t *testing.T, ca *testCA, dnsNames []string, handler func(http.ResponseWriter, *http.Request)) (*seen, int) {
	t.Helper()
	rec := &seen{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.hits++
		rec.host, rec.method, rec.path, rec.body, rec.header = r.Host, r.Method, r.URL.RequestURI(), string(b), r.Header.Clone()
		rec.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.leaf(t, dnsNames...)},
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			rec.mu.Lock()
			rec.serverName = h.ServerName
			rec.mu.Unlock()
			return nil, nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return rec, srv.Listener.Addr().(*net.TCPAddr).Port
}

const testHost = "hook.example.test" // the .test TLD never resolves in the real DNS

var loopback = netip.MustParseAddr("127.0.0.1")

// allowLoopbackOnly relaxes the address policy for the single test address 127.0.0.1.
// It exists only in this test file: production code cannot reach it.
func allowLoopbackOnly(a netip.Addr) string {
	if a == loopback {
		return ""
	}
	return BlockedReason(a)
}

func testClient(t *testing.T, ca *testCA, port int, res Resolver, mutate func(*ClientConfig)) *Client {
	t.Helper()
	cfg := DefaultClientConfig()
	cfg.Resolver = res
	cfg.URLPolicy = URLPolicy{AllowedPorts: []int{port}, MaxLength: 2048}
	cfg.TLSConfig = &tls.Config{RootCAs: ca.pool}
	cfg.ResolveTimeout = 500 * time.Millisecond
	cfg.ConnectTimeout = 500 * time.Millisecond
	cfg.TLSHandshakeTimeout = 2 * time.Second
	cfg.ResponseHeaderTimeout = 2 * time.Second
	cfg.RequestTimeout = 5 * time.Second
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.dialer.policy = allowLoopbackOnly
	return c
}

func goodRequest(port int) Request {
	return Request{
		URL:  fmt.Sprintf("https://%s:%d/hooks/abc?token=SECRETTOKEN", testHost, port),
		Body: []byte(`{"exact": "bytes",  "kept": true}`),
		Headers: map[string]string{
			"Content-Type": "application/json", "X-RelayForge-Delivery-Id": "dlv-1", "X-RelayForge-Event-Id": "evt-1",
			"X-RelayForge-Signature": "v1=deadbeefdeadbeef",
		},
	}
}

func classOf(t *testing.T, err error) ErrorClass {
	t.Helper()
	var re *RequestError
	if !errors.As(err, &re) {
		t.Fatalf("error %v is not a *RequestError", err)
	}
	return re.Class
}

// ---- tests ----

func TestDeliversToAValidatedPinnedAddressPreservingHostAndSNI(t *testing.T) {
	ca := newCA(t)
	rec, port := startServer(t, ca, []string{testHost}, nil)
	res := newResolver(testHost, ips("127.0.0.1"))
	c := testClient(t, ca, port, res, nil)

	// The name does not exist in real DNS, so success proves the transport used the pinned address.
	resp, err := c.Do(context.Background(), goodRequest(port))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("Do: %+v %v", resp, err)
	}
	if res.callCount(testHost) != 1 {
		t.Fatalf("the controlled resolver must be asked exactly once, got %d", res.callCount(testHost))
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.host != fmt.Sprintf("%s:%d", testHost, port) {
		t.Errorf("Host header = %q, want the original hostname", rec.host)
	}
	if rec.serverName != testHost {
		t.Errorf("TLS SNI = %q, want %q", rec.serverName, testHost)
	}
	if rec.method != "POST" || rec.path != "/hooks/abc?token=SECRETTOKEN" || rec.body != `{"exact": "bytes",  "kept": true}` {
		t.Errorf("request as received: %s %s %q", rec.method, rec.path, rec.body)
	}
	if rec.header.Get("User-Agent") != DefaultClientConfig().UserAgent || rec.header.Get("Content-Type") != "application/json" ||
		rec.header.Get("X-RelayForge-Delivery-Id") != "dlv-1" || rec.header.Get("X-RelayForge-Event-Id") != "evt-1" {
		t.Errorf("headers as received: %v", rec.header)
	}
	for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Forwarded-For"} {
		if rec.header.Get(h) != "" {
			t.Errorf("unexpected header %s", h)
		}
	}
}

func TestBlockedAddressesAreNeverContacted(t *testing.T) {
	ca := newCA(t)
	rec, port := startServer(t, ca, []string{testHost}, nil)
	for _, addr := range []string{
		"10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "127.0.0.2", "0.0.0.0", "100.64.0.1", "224.0.0.1", "240.0.0.1",
		"::1", "::", "fe80::1", "fc00::1", "fd00:ec2::254", "ff02::1", "::ffff:10.0.0.1", "::ffff:127.0.0.2", "64:ff9b::a00:1", "2002:a00:1::1",
	} {
		c := testClient(t, ca, port, newResolver(testHost, ips(addr)), nil)
		_, err := c.Do(context.Background(), goodRequest(port))
		if err == nil || classOf(t, err) != ClassBlockedAddress {
			t.Errorf("%s: got %v, want blocked_address", addr, err)
		}
	}
	// A zone identifier on an otherwise public address is refused too.
	zoned := netip.MustParseAddr("2606:4700:4700::1111").WithZone("eth0")
	c := testClient(t, ca, port, newResolver(testHost, []netip.Addr{zoned}), nil)
	if _, err := c.Do(context.Background(), goodRequest(port)); err == nil || classOf(t, err) != ClassBlockedAddress {
		t.Errorf("zoned address: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("a blocked address was contacted %d times", rec.count())
	}
}

func TestMixedPublicAndPrivateAnswersAreRejectedAsAWhole(t *testing.T) {
	ca := newCA(t)
	rec, port := startServer(t, ca, []string{testHost}, nil)
	for _, answer := range [][]netip.Addr{ips("127.0.0.1", "10.0.0.1"), ips("10.0.0.1", "127.0.0.1"), ips("127.0.0.1", "::1")} {
		c := testClient(t, ca, port, newResolver(testHost, answer), nil)
		if _, err := c.Do(context.Background(), goodRequest(port)); err == nil || classOf(t, err) != ClassBlockedAddress {
			t.Errorf("%v: got %v", answer, err)
		}
	}
	if rec.count() != 0 {
		t.Fatal("the allowed address of a mixed answer must not be used")
	}
}

func TestDNSFailuresTimeoutsAndCancellation(t *testing.T) {
	ca := newCA(t)
	_, port := startServer(t, ca, []string{testHost}, nil)

	t.Run("no such host", func(t *testing.T) {
		c := testClient(t, ca, port, newResolver(testHost), nil)
		if _, err := c.Do(context.Background(), goodRequest(port)); err == nil || classOf(t, err) != ClassDNS {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("empty answer", func(t *testing.T) {
		c := testClient(t, ca, port, newResolver(testHost, []netip.Addr{}), nil)
		if _, err := c.Do(context.Background(), goodRequest(port)); err == nil || classOf(t, err) != ClassBlockedAddress && classOf(t, err) != ClassDNS {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("resolver error", func(t *testing.T) {
		r := newResolver(testHost)
		r.err = errors.New("resolver exploded with SECRETTOKEN")
		c := testClient(t, ca, port, r, nil)
		_, err := c.Do(context.Background(), goodRequest(port))
		if err == nil || classOf(t, err) != ClassDNS || strings.Contains(err.Error(), "SECRETTOKEN") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		r := newResolver(testHost)
		r.block = true
		c := testClient(t, ca, port, r, func(cfg *ClientConfig) { cfg.ResolveTimeout = 50 * time.Millisecond })
		start := time.Now()
		_, err := c.Do(context.Background(), goodRequest(port))
		if err == nil || classOf(t, err) != ClassDNSTimeout {
			t.Fatalf("got %v", err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("the resolver timeout was not applied")
		}
	})
	t.Run("context cancellation", func(t *testing.T) {
		r := newResolver(testHost)
		r.block = true
		c := testClient(t, ca, port, r, func(cfg *ClientConfig) { cfg.ResolveTimeout = 5 * time.Second })
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, err := c.Do(ctx, goodRequest(port))
		if err == nil || classOf(t, err) != ClassCanceled || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestDNSRebindingCannotRedirectTheConnection(t *testing.T) {
	ca := newCA(t)
	rec, port := startServer(t, ca, []string{testHost}, nil)

	t.Run("a later answer that turns private is rejected", func(t *testing.T) {
		res := newResolver(testHost, ips("127.0.0.1"), ips("10.0.0.1"))
		c := testClient(t, ca, port, res, nil)
		if _, err := c.Do(context.Background(), goodRequest(port)); err != nil {
			t.Fatalf("first request: %v", err)
		}
		before := rec.count()
		if _, err := c.Do(context.Background(), goodRequest(port)); err == nil || classOf(t, err) != ClassBlockedAddress {
			t.Fatalf("the rebound answer must be rejected: %v", err)
		}
		if rec.count() != before {
			t.Fatal("the rebound address was contacted")
		}
		if res.callCount(testHost) != 2 {
			t.Fatalf("each request must resolve once, got %d lookups in total", res.callCount(testHost))
		}
	})

	t.Run("the address is checked again when the socket connects", func(t *testing.T) {
		c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), nil)
		var calls atomic.Int32
		c.dialer.policy = func(a netip.Addr) string {
			if calls.Add(1) == 1 { // the resolution check allows it ...
				return ""
			}
			return "rebound" // ... the connect-time check does not
		}
		before := rec.count()
		if _, err := c.Do(context.Background(), goodRequest(port)); err == nil || classOf(t, err) != ClassBlockedAddress {
			t.Fatalf("got %v", err)
		}
		if rec.count() != before || calls.Load() < 2 {
			t.Fatalf("connect-time recheck missing: %d policy calls, %d hits", calls.Load(), rec.count()-before)
		}
	})

	t.Run("an address literal in the URL is judged without any lookup", func(t *testing.T) {
		res := newResolver(testHost, ips("127.0.0.1"))
		c := testClient(t, ca, port, res, nil)
		for _, u := range []string{"https://127.0.0.1:%d/", "https://[::1]:%d/", "https://2130706433:%d/", "https://127.1:%d/", "https://0x7f000001:%d/", "https://169.254.169.254:%d/"} {
			req := goodRequest(port)
			req.URL = fmt.Sprintf(u, port)
			if _, err := c.Do(context.Background(), req); err == nil || classOf(t, err) != ClassInvalidURL {
				t.Errorf("%s: got %v", u, err)
			}
		}
	})
}

func TestRedirectsAreNeverFollowed(t *testing.T) {
	ca := newCA(t)
	target, targetPort := startServer(t, ca, []string{testHost}, nil)
	for _, location := range []string{
		fmt.Sprintf("https://%s:%d/other", testHost, targetPort), "https://169.254.169.254/latest/meta-data",
		"https://127.0.0.1/", "/relative",
	} {
		rec, port := startServer(t, ca, []string{testHost}, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusFound)
		})
		c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), nil)
		resp, err := c.Do(context.Background(), goodRequest(port))
		if err != nil || resp.StatusCode != http.StatusFound {
			t.Fatalf("%s: a redirect must be reported as a 302 response: %+v %v", location, resp, err)
		}
		if rec.count() != 1 || target.count() != 0 {
			t.Fatalf("%s: redirect was followed (%d hits, %d at the target)", location, rec.count(), target.count())
		}
	}
}

func TestProxyEnvironmentVariablesAreIgnored(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	var connections atomic.Int32
	go func() {
		for {
			conn, err := proxy.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			conn.Close()
		}
	}()
	proxyURL := "http://" + proxy.Addr().String()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, proxyURL)
	}
	t.Setenv("NO_PROXY", "")

	ca := newCA(t)
	rec, port := startServer(t, ca, []string{testHost}, nil)
	c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), nil)
	if c.transport.Proxy != nil {
		t.Fatal("the transport must not consult proxy settings")
	}
	if resp, err := c.Do(context.Background(), goodRequest(port)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("Do: %v", err)
	}
	if connections.Load() != 0 || rec.count() != 1 {
		t.Fatalf("the proxy saw %d connections and the destination %d requests", connections.Load(), rec.count())
	}
}

func TestTLSCertificatesAreVerifiedAgainstTheOriginalHostname(t *testing.T) {
	ca := newCA(t)
	t.Run("certificate for another name", func(t *testing.T) {
		rec, port := startServer(t, ca, []string{"other.example.test"}, nil)
		c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), nil)
		if _, err := c.Do(context.Background(), goodRequest(port)); err == nil || classOf(t, err) != ClassTLS {
			t.Fatalf("got %v", err)
		}
		if rec.count() != 0 {
			t.Fatal("the request must not be sent after a failed verification")
		}
	})
	t.Run("an IP address in the certificate is not a substitute for the name", func(t *testing.T) {
		_, port := startServer(t, ca, []string{"127.0.0.1"}, nil)
		c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), nil)
		if _, err := c.Do(context.Background(), goodRequest(port)); err == nil || classOf(t, err) != ClassTLS {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("untrusted issuer", func(t *testing.T) {
		_, port := startServer(t, ca, []string{testHost}, nil)
		c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), func(cfg *ClientConfig) { cfg.TLSConfig = nil })
		if _, err := c.Do(context.Background(), goodRequest(port)); err == nil || classOf(t, err) != ClassTLS {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("certificate verification cannot be disabled", func(t *testing.T) {
		_, port := startServer(t, ca, []string{"other.example.test"}, nil)
		c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), func(cfg *ClientConfig) {
			cfg.TLSConfig = &tls.Config{RootCAs: ca.pool, InsecureSkipVerify: true}
		})
		if _, err := c.Do(context.Background(), goodRequest(port)); err == nil || classOf(t, err) != ClassTLS {
			t.Fatalf("InsecureSkipVerify must be ignored: %v", err)
		}
	})
}

func TestTimeouts(t *testing.T) {
	ca := newCA(t)

	t.Run("connect timeout", func(t *testing.T) {
		_, port := startServer(t, ca, []string{testHost}, nil)
		c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), func(cfg *ClientConfig) { cfg.ConnectTimeout = 50 * time.Millisecond })
		c.dialer.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			<-ctx.Done() // a black-holed address
			return nil, ctx.Err()
		}
		start := time.Now()
		_, err := c.Do(context.Background(), goodRequest(port))
		if err == nil || classOf(t, err) != ClassConnectTimeout || time.Since(start) > 2*time.Second {
			t.Fatalf("got %v after %v", err, time.Since(start))
		}
	})
	t.Run("response header timeout", func(t *testing.T) {
		_, port := startServer(t, ca, []string{testHost}, func(w http.ResponseWriter, r *http.Request) { time.Sleep(400 * time.Millisecond) })
		c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), func(cfg *ClientConfig) { cfg.ResponseHeaderTimeout = 80 * time.Millisecond })
		if _, err := c.Do(context.Background(), goodRequest(port)); err == nil || classOf(t, err) != ClassTimeout {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("overall request timeout", func(t *testing.T) {
		_, port := startServer(t, ca, []string{testHost}, func(w http.ResponseWriter, r *http.Request) { time.Sleep(500 * time.Millisecond) })
		c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), func(cfg *ClientConfig) {
			cfg.RequestTimeout = 100 * time.Millisecond
			cfg.ResponseHeaderTimeout = 5 * time.Second
		})
		if _, err := c.Do(context.Background(), goodRequest(port)); err == nil || classOf(t, err) != ClassTimeout {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("cancellation during the request", func(t *testing.T) {
		_, port := startServer(t, ca, []string{testHost}, func(w http.ResponseWriter, r *http.Request) { time.Sleep(500 * time.Millisecond) })
		c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), nil)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(80*time.Millisecond, cancel)
		_, err := c.Do(ctx, goodRequest(port))
		if err == nil || classOf(t, err) != ClassCanceled {
			t.Fatalf("a cancelled request must be reported as canceled, not as a timeout or a failure of the destination: %v", err)
		}
	})
}

func TestOnlyBoundedResponseDataIsRead(t *testing.T) {
	ca := newCA(t)
	var written atomic.Int64
	_, port := startServer(t, ca, []string{testHost}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusServiceUnavailable)
		chunk := []byte(strings.Repeat("x", 4096))
		for i := 0; i < 2048; i++ { // 8 MiB, far more than the client reads
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), func(cfg *ClientConfig) { cfg.MaxResponseBytes = 1024 })
	resp, err := c.Do(context.Background(), goodRequest(port))
	if err != nil || resp.StatusCode != 503 {
		t.Fatalf("Do: %+v %v", resp, err)
	}
	if !resp.BodyTruncated || resp.BodyBytesRead > 1025 {
		t.Fatalf("read %d bytes (truncated=%v), want the 1 KiB limit to apply", resp.BodyBytesRead, resp.BodyTruncated)
	}
	if resp.RetryAfter != "17" {
		t.Fatalf("RetryAfter = %q", resp.RetryAfter)
	}
}

func TestSmallResponsesAreNotTruncated(t *testing.T) {
	ca := newCA(t)
	_, port := startServer(t, ca, []string{testHost}, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), nil)
	resp, err := c.Do(context.Background(), goodRequest(port))
	if err != nil || resp.BodyTruncated || resp.BodyBytesRead != 2 {
		t.Fatalf("%+v %v", resp, err)
	}
}

func TestMultipleAnswersAreTriedInOrder(t *testing.T) {
	ca := newCA(t)
	rec, port := startServer(t, ca, []string{testHost}, nil)
	c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.2", "127.0.0.1")), nil)
	c.dialer.policy = func(a netip.Addr) string { // both loopback addresses are allowed in this test
		if a == netip.MustParseAddr("127.0.0.2") || a == loopback {
			return ""
		}
		return BlockedReason(a)
	}
	var tried []string
	var mu sync.Mutex
	c.dialer.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		tried = append(tried, addr)
		mu.Unlock()
		if strings.HasPrefix(addr, "127.0.0.2:") {
			return nil, errors.New("connection refused")
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	if resp, err := c.Do(context.Background(), goodRequest(port)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("Do: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tried) != 2 || !strings.HasPrefix(tried[0], "127.0.0.2:") || !strings.HasPrefix(tried[1], "127.0.0.1:") || rec.count() != 1 {
		t.Fatalf("addresses tried: %v", tried)
	}
}

func TestRequestsAreRestrictedToTheSafeSubset(t *testing.T) {
	ca := newCA(t)
	rec, port := startServer(t, ca, []string{testHost}, nil)
	c := testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), nil)
	with := func(mut func(*Request)) Request { r := goodRequest(port); mut(&r); return r }

	cases := map[string]struct {
		req   Request
		class ErrorClass
	}{
		"plain http":          {with(func(r *Request) { r.URL = fmt.Sprintf("http://%s:%d/", testHost, port) }), ClassInvalidURL},
		"credentials in url":  {with(func(r *Request) { r.URL = fmt.Sprintf("https://u:p@%s:%d/", testHost, port) }), ClassInvalidURL},
		"fragment":            {with(func(r *Request) { r.URL += "#x" }), ClassInvalidURL},
		"port not allowed":    {with(func(r *Request) { r.URL = fmt.Sprintf("https://%s:%d/", testHost, port+1) }), ClassInvalidURL},
		"localhost":           {with(func(r *Request) { r.URL = fmt.Sprintf("https://localhost:%d/", port) }), ClassInvalidURL},
		"authorization":       {with(func(r *Request) { r.Headers["Authorization"] = "Bearer x" }), ClassInvalidRequest},
		"cookie":              {with(func(r *Request) { r.Headers["Cookie"] = "a=b" }), ClassInvalidRequest},
		"host override":       {with(func(r *Request) { r.Headers["Host"] = "evil.example" }), ClassInvalidRequest},
		"user agent override": {with(func(r *Request) { r.Headers["User-Agent"] = "evil" }), ClassInvalidRequest},
		"header injection":    {with(func(r *Request) { r.Headers["X-RelayForge-Event-Id"] = "a\r\nX-Evil: 1" }), ClassInvalidRequest},
		"empty content type":  {with(func(r *Request) { delete(r.Headers, "Content-Type") }), ClassInvalidRequest},
	}
	for name, tc := range cases {
		if _, err := c.Do(context.Background(), tc.req); err == nil || classOf(t, err) != tc.class {
			t.Errorf("%s: got %v, want %s", name, err, tc.class)
		}
	}
	if rec.count() != 0 {
		t.Fatalf("%d rejected requests reached the server", rec.count())
	}
}

func TestErrorsNeverContainSecretsTokensOrTheDestinationURL(t *testing.T) {
	ca := newCA(t)
	_, port := startServer(t, ca, []string{"other.example.test"}, nil)
	secrets := []string{"SECRETTOKEN", "deadbeefdeadbeef", testHost, `"exact"`}

	failing := map[string]*Client{
		"tls":     testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), nil),
		"blocked": testClient(t, ca, port, newResolver(testHost, ips("10.0.0.1")), nil),
		"dns":     testClient(t, ca, port, newResolver(testHost), nil),
		"timeout": testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), func(c *ClientConfig) { c.TLSHandshakeTimeout = time.Nanosecond }),
		"connect": testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), nil),
		"badurl":  testClient(t, ca, port, newResolver(testHost, ips("127.0.0.1")), nil),
	}
	failing["connect"].dialer.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, fmt.Errorf("dial %s failed for https://%s/?token=SECRETTOKEN", addr, testHost)
	}
	for name, c := range failing {
		req := goodRequest(port)
		if name == "badurl" {
			req.URL = "http://" + testHost + "/?token=SECRETTOKEN"
		}
		_, err := c.Do(context.Background(), req)
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		for _, text := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
			for _, s := range secrets {
				if strings.Contains(text, s) {
					t.Errorf("%s: error text leaks %q: %s", name, s, text)
				}
			}
		}
	}
}

func TestClientConfigurationIsValidatedAndHardened(t *testing.T) {
	cfg := DefaultClientConfig()
	if cfg.URLPolicy.AllowHTTP || len(cfg.URLPolicy.AllowedPorts) != 1 || cfg.URLPolicy.AllowedPorts[0] != 443 {
		t.Fatalf("defaults must be https on 443: %+v", cfg.URLPolicy)
	}
	bad := map[string]func(*ClientConfig){
		"zero request timeout": func(c *ClientConfig) { c.RequestTimeout = 0 },
		"zero connect timeout": func(c *ClientConfig) { c.ConnectTimeout = 0 },
		"zero resolve timeout": func(c *ClientConfig) { c.ResolveTimeout = 0 },
		"zero handshake":       func(c *ClientConfig) { c.TLSHandshakeTimeout = 0 },
		"zero header timeout":  func(c *ClientConfig) { c.ResponseHeaderTimeout = 0 },
		"no response limit":    func(c *ClientConfig) { c.MaxResponseBytes = 0 },
		"huge response limit":  func(c *ClientConfig) { c.MaxResponseBytes = 1 << 30 },
		"empty user agent":     func(c *ClientConfig) { c.UserAgent = "" },
		"http allowed":         func(c *ClientConfig) { c.URLPolicy.AllowHTTP = true },
	}
	for name, mut := range bad {
		c := DefaultClientConfig()
		mut(&c)
		if _, err := NewClient(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c, err := NewClient(DefaultClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	tr := c.transport
	if tr.Proxy != nil || !tr.DisableKeepAlives || !tr.DisableCompression || tr.TLSClientConfig.MinVersion < tls.VersionTLS12 || tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("transport is not hardened: %+v", tr)
	}
	if c.http.CheckRedirect == nil || c.http.Timeout != 0 && c.http.Timeout < time.Second {
		t.Fatal("redirects must be refused")
	}
}
