package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// ErrorClass names why an outbound request failed. Classes are stable,
// low-cardinality strings that are safe to log, store and use as metric labels.
// They never contain a hostname, URL, header or payload.
type ErrorClass string

// Error classes.
const (
	ClassInvalidURL     ErrorClass = "invalid_url"
	ClassInvalidRequest ErrorClass = "invalid_request"
	ClassBlockedAddress ErrorClass = "blocked_address"
	ClassDNS            ErrorClass = "dns_error"
	ClassDNSTimeout     ErrorClass = "dns_timeout"
	ClassConnect        ErrorClass = "connect_error"
	ClassConnectTimeout ErrorClass = "connect_timeout"
	ClassTLS            ErrorClass = "tls_error"
	ClassTimeout        ErrorClass = "timeout"
	// ClassCanceled means the caller's context ended (for example during shutdown).
	// It says nothing about the destination.
	ClassCanceled ErrorClass = "canceled"
	ClassProtocol ErrorClass = "protocol_error"
)

// RequestError is the only error type Client returns. Its text is the class and
// nothing else: net/http errors embed the full URL, including query tokens, so
// the original error is deliberately not exposed. Only context.Canceled and
// context.DeadlineExceeded are reachable through errors.Is.
type RequestError struct {
	Class ErrorClass
	cause error
}

func (e *RequestError) Error() string { return "delivery request failed: " + string(e.Class) }

// Unwrap exposes only the standard context sentinels.
func (e *RequestError) Unwrap() error { return e.cause }

// Format prints the same sanitized text for every verb, including %+v and %#v,
// which would otherwise dump the struct.
func (e *RequestError) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, e.Error()) }

func newRequestError(class ErrorClass, cause error) *RequestError {
	switch {
	case errors.Is(cause, context.Canceled):
		cause = context.Canceled
	case errors.Is(cause, context.DeadlineExceeded):
		cause = context.DeadlineExceeded
	default:
		cause = nil
	}
	return &RequestError{Class: class, cause: cause}
}

// classedError carries a class through net/http, which wraps dial errors.
type classedError struct {
	class ErrorClass
	cause error
}

func (e *classedError) Error() string { return "dial failed: " + string(e.class) }
func (e *classedError) Unwrap() error { return e.cause }

// Resolver looks up the addresses of a host. *net.Resolver satisfies it, which
// follows CNAME chains; every returned address is validated whatever name it came from.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// ClientConfig configures Client. All timeouts are required.
type ClientConfig struct {
	// Resolver defaults to net.DefaultResolver.
	Resolver Resolver
	// URLPolicy must stay https-only. It is applied to every request, not only when a URL is stored.
	URLPolicy URLPolicy
	// ResolveTimeout bounds the DNS lookup.
	ResolveTimeout time.Duration
	// ConnectTimeout bounds each TCP connection attempt.
	ConnectTimeout time.Duration
	// TLSHandshakeTimeout bounds the TLS handshake.
	TLSHandshakeTimeout time.Duration
	// ResponseHeaderTimeout bounds the wait for response headers after the request is written.
	ResponseHeaderTimeout time.Duration
	// RequestTimeout bounds the whole exchange, response body included.
	RequestTimeout time.Duration
	// MaxResponseBytes is how much of the response body is read; the rest is dropped.
	MaxResponseBytes int64
	// UserAgent is sent on every request and cannot be overridden per request.
	UserAgent string
	// TLSConfig may supply RootCAs (for private test CAs). Certificate
	// verification cannot be disabled and the minimum version is TLS 1.2.
	TLSConfig *tls.Config
}

// DefaultClientConfig returns the production defaults.
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		URLPolicy:             DefaultURLPolicy(),
		ResolveTimeout:        5 * time.Second,
		ConnectTimeout:        5 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		RequestTimeout:        30 * time.Second,
		MaxResponseBytes:      64 << 10,
		UserAgent:             "RelayForge-Delivery/1",
	}
}

const (
	maxResponseLimit  = 1 << 20
	maxDialCandidates = 4
	maxHeaderValue    = 1024
)

// Request is one outbound delivery request. The method is always POST. Only
// Content-Type and X-RelayForge-* headers are accepted: nothing from an
// incoming request, and no credentials, can be forwarded.
type Request struct {
	URL     string
	Headers map[string]string
	Body    []byte
}

// Response is what is kept of a destination's answer: the status, the
// Retry-After header, and how much body was read. The body itself is discarded.
type Response struct {
	StatusCode    int
	RetryAfter    string
	BodyBytesRead int64
	BodyTruncated bool
	Duration      time.Duration
}

// Client sends webhook requests to destinations. It validates the URL on every
// request and pins each connection to an address validated immediately before dialing.
type Client struct {
	cfg       ClientConfig
	dialer    *dialer
	transport *http.Transport
	http      *http.Client
}

// NewClient builds a Client, rejecting insecure or incomplete configuration.
func NewClient(cfg ClientConfig) (*Client, error) {
	switch {
	case cfg.URLPolicy.AllowHTTP:
		return nil, errors.New("egress client: plain http destinations are not allowed")
	case cfg.ResolveTimeout <= 0 || cfg.ConnectTimeout <= 0 || cfg.TLSHandshakeTimeout <= 0 ||
		cfg.ResponseHeaderTimeout <= 0 || cfg.RequestTimeout <= 0:
		return nil, errors.New("egress client: every timeout must be positive")
	case cfg.MaxResponseBytes <= 0 || cfg.MaxResponseBytes > maxResponseLimit:
		return nil, errors.New("egress client: MaxResponseBytes must be between 1 and 1 MiB")
	case strings.TrimSpace(cfg.UserAgent) == "":
		return nil, errors.New("egress client: UserAgent is required")
	}
	resolver := cfg.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.TLSConfig != nil {
		tlsCfg.RootCAs = cfg.TLSConfig.RootCAs // the only setting taken over
	}

	d := &dialer{resolver: resolver, resolveTimeout: cfg.ResolveTimeout, connectTimeout: cfg.ConnectTimeout, policy: BlockedReason}
	d.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{Control: d.control}).DialContext(ctx, network, addr)
	}

	tr := &http.Transport{
		Proxy:                  nil, // explicit: proxy environment variables are never consulted
		DialContext:            d.DialContext,
		TLSClientConfig:        tlsCfg,
		TLSHandshakeTimeout:    cfg.TLSHandshakeTimeout,
		ResponseHeaderTimeout:  cfg.ResponseHeaderTimeout,
		MaxResponseHeaderBytes: 32 << 10,
		DisableKeepAlives:      true, // every delivery is resolved, validated and dialed afresh
		DisableCompression:     true,
		ForceAttemptHTTP2:      false,
	}
	httpClient := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Client{cfg: cfg, dialer: d, transport: tr, http: httpClient}, nil
}

func validHeaderName(name string) bool {
	return strings.EqualFold(name, "Content-Type") || strings.HasPrefix(strings.ToLower(name), "x-relayforge-")
}

func validHeaderValue(v string) bool {
	if v == "" || len(v) > maxHeaderValue {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}

// Do sends one request. The returned error is always a *RequestError.
func (c *Client) Do(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	fail := func(class ErrorClass, cause error) (Response, error) {
		return Response{Duration: time.Since(start)}, newRequestError(class, cause)
	}

	target, err := ValidateDestinationURL(req.URL, c.cfg.URLPolicy)
	if err != nil {
		return fail(ClassInvalidURL, nil)
	}
	hasContentType := false
	for name, value := range req.Headers {
		if !validHeaderName(name) || !validHeaderValue(value) {
			return fail(ClassInvalidRequest, nil)
		}
		if strings.EqualFold(name, "Content-Type") {
			hasContentType = true
		}
	}
	if !hasContentType {
		return fail(ClassInvalidRequest, nil)
	}

	rctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(rctx, http.MethodPost, target.Normalized, bytes.NewReader(req.Body))
	if err != nil {
		return fail(ClassInvalidRequest, nil)
	}
	for name, value := range req.Headers {
		httpReq.Header.Set(name, value)
	}
	httpReq.Header.Set("User-Agent", c.cfg.UserAgent)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Response{Duration: time.Since(start)}, c.classify(ctx, rctx, err)
	}
	defer resp.Body.Close()

	// Read at most MaxResponseBytes (plus one byte to detect truncation). The body is never kept.
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, c.cfg.MaxResponseBytes+1))
	out := Response{
		StatusCode:    resp.StatusCode,
		RetryAfter:    resp.Header.Get("Retry-After"),
		BodyBytesRead: n,
		BodyTruncated: n > c.cfg.MaxResponseBytes,
		Duration:      time.Since(start),
	}
	return out, nil
}

// classify turns any net/http failure into a sanitized RequestError.
func (c *Client) classify(parent, request context.Context, err error) *RequestError {
	var ce *classedError
	if errors.As(err, &ce) {
		return newRequestError(ce.class, ce.cause)
	}
	if parent.Err() != nil && errors.Is(parent.Err(), context.Canceled) {
		return newRequestError(ClassCanceled, context.Canceled)
	}
	if errors.Is(request.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return newRequestError(ClassTimeout, context.DeadlineExceeded)
	}
	var (
		verify  *tls.CertificateVerificationError
		unknown x509.UnknownAuthorityError
		host    x509.HostnameError
		invalid x509.CertificateInvalidError
		alert   tls.AlertError
		record  tls.RecordHeaderError
	)
	if errors.As(err, &verify) || errors.As(err, &unknown) || errors.As(err, &host) || errors.As(err, &invalid) ||
		errors.As(err, &alert) || errors.As(err, &record) || strings.Contains(err.Error(), "tls:") {
		return newRequestError(ClassTLS, nil)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return newRequestError(ClassTimeout, context.DeadlineExceeded)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return newRequestError(ClassConnect, nil)
	}
	return newRequestError(ClassProtocol, nil)
}

// dialer resolves, validates and dials. http.Transport never resolves a
// hostname itself, because it only ever receives this DialContext.
type dialer struct {
	resolver       Resolver
	resolveTimeout time.Duration
	connectTimeout time.Duration
	// policy returns "" for acceptable addresses. It is BlockedReason in
	// production and replaced only by tests in this package.
	policy func(netip.Addr) string
	// dial opens the TCP connection to an already validated ip:port.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (d *dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, &classedError{class: ClassInvalidRequest}
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, &classedError{class: ClassInvalidRequest}
	}

	// 1. Resolve (or take an address literal as is) under a bounded context.
	var answer []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		answer = []netip.Addr{ip}
	} else {
		rctx, cancel := context.WithTimeout(ctx, d.resolveTimeout)
		answer, err = d.resolver.LookupNetIP(rctx, "ip", host)
		resolveErr := rctx.Err()
		cancel()
		switch {
		case err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled):
			return nil, &classedError{class: ClassCanceled, cause: context.Canceled}
		case err != nil && errors.Is(resolveErr, context.DeadlineExceeded):
			return nil, &classedError{class: ClassDNSTimeout, cause: context.DeadlineExceeded}
		case err != nil:
			return nil, &classedError{class: ClassDNS}
		}
	}

	// 2. Validate every returned address; one bad address rejects the whole answer.
	if len(answer) == 0 {
		return nil, &classedError{class: ClassDNS}
	}
	valid, err := requireAllWith(answer, d.policy)
	if err != nil {
		return nil, &classedError{class: ClassBlockedAddress}
	}
	if len(valid) > maxDialCandidates {
		valid = valid[:maxDialCandidates]
	}

	// 3. Dial the validated addresses directly, in order. The hostname is never resolved again.
	var last error = &classedError{class: ClassConnect}
	for _, ip := range valid {
		actx, cancel := context.WithTimeout(ctx, d.connectTimeout)
		conn, derr := d.dial(actx, network, net.JoinHostPort(ip.String(), port))
		attemptExpired := errors.Is(actx.Err(), context.DeadlineExceeded)
		cancel()
		if derr == nil {
			return conn, nil
		}
		var ce *classedError
		switch {
		case errors.As(derr, &ce):
			return nil, ce // the connect-time address check failed: do not try other addresses
		case ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled):
			return nil, &classedError{class: ClassCanceled, cause: context.Canceled}
		case ctx.Err() != nil:
			return nil, &classedError{class: ClassTimeout, cause: context.DeadlineExceeded}
		case attemptExpired:
			last = &classedError{class: ClassConnectTimeout, cause: context.DeadlineExceeded}
		default:
			last = &classedError{class: ClassConnect}
		}
	}
	return nil, last
}

// control runs when the socket is about to connect. It checks the address the
// operating system is really connecting to, independently of the check above.
func (d *dialer) control(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || d.policy(ap.Addr()) != "" {
		return &classedError{class: ClassBlockedAddress}
	}
	return nil
}
