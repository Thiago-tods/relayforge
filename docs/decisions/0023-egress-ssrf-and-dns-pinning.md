# 23. Outbound HTTP client: SSRF defence and DNS pinning

Date: 2026-11-17

## Status

Accepted

## Context

Destinations are customer-controlled URLs, so the delivery worker is a server-side request forgery
primitive unless it is constrained. Checking a hostname once and then letting the HTTP stack resolve it
again allows DNS rebinding.

## Decision

`internal/egress.Client` is the only component that sends delivery requests.

1. The URL is validated on **every send**, not only when the destination is saved (HTTPS only, no
   credentials in the URL, no fragments, bounded length, no literal blocked IPs).
2. The host is resolved **once**. **All** returned addresses are classified; if any is loopback, private,
   link-local, multicast, unspecified, carrier-grade NAT, documentation or otherwise reserved, the request
   is refused. A mixed answer is refused too, because an attacker could otherwise win a race for the bad one.
3. The connection is made to the validated IP literal. The original host is kept for TLS SNI, certificate
   verification and the `Host` header, so certificates are still verified against the real name.
4. `net.Dialer.Control` checks the final socket address again, as defence in depth.
5. Proxies are never used (environment proxy variables are ignored). Redirects are never followed: a 3xx is
   a final, non-retryable result. Keep-alives are disabled so a pinned address is never reused for another
   resolution. TLS 1.2 is the minimum.
6. Timeouts bound every phase (resolve 5 s, connect 5 s, TLS 5 s, response headers 10 s, total 30 s). The
   response body is read up to 64 KiB and discarded; nothing from it is stored or logged.
7. Errors carry a class (`dns`, `blocked_address`, `connect`, `tls`, `timeout`, ...), never the URL, the
   address or the underlying error text.

## Consequences

- Tests use a controlled resolver and local TLS servers; no test makes a real network call.
- Without keep-alives each attempt pays a TCP and TLS handshake. Deliveries are not latency-critical.
- Destinations on private networks are not supported. This is deliberate; a future opt-in would need an ADR.
