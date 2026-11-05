package cloud

import (
	"alisa-dreame-readonly/internal/bridge"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// Conservative exclusion of special-use ranges, including IANA 2025-10-09
// documentation/translation/protocol blocks. Ordinary public global unicast
// only; rejecting a special-purpose public exception is intentional.
var deniedMapPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func publicMapAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.Zone() != "" || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	if addr.Is6() && !netip.MustParsePrefix("2000::/3").Contains(addr) {
		return false
	}
	for _, p := range deniedMapPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

type mapBlobReader struct {
	hosts   map[string]bool
	resolve func(context.Context, string) ([]netip.Addr, error)
	dial    func(context.Context, string, string) (net.Conn, error)
	// roots is nil in production; package tests use a synthetic TLS CA.
	roots *x509.CertPool
}

func newMapBlobReader(hosts []string) *mapBlobReader {
	approved := map[string]bool{}
	for _, h := range hosts {
		approved[h] = true
	}
	return &mapBlobReader{hosts: approved, resolve: func(ctx context.Context, h string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
	}, dial: (&net.Dialer{Timeout: callTimeout}).DialContext}
}
func (b *mapBlobReader) approved(value string) bool {
	d := inspectSignedMapURL(value)
	return d.Outcome == "metadata_available" && b.hosts[d.Hostname]
}
func (b *mapBlobReader) fetch(parent context.Context, value string, limit int) ([]byte, error) {
	if limit < 1 || limit > 2<<20 || !b.approved(value) {
		return nil, sessionError("map_blob_host_denied")
	}
	u, _ := url.Parse(value)
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	ctx, cancel := context.WithTimeout(parent, callTimeout)
	defer cancel()
	transport := &http.Transport{Proxy: nil, DisableCompression: true, DisableKeepAlives: true, ForceAttemptHTTP2: false, MaxConnsPerHost: 1, MaxResponseHeaderBytes: 16 << 10, TLSHandshakeTimeout: callTimeout, ResponseHeaderTimeout: callTimeout, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host, RootCAs: b.roots}}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		requested, port, err := net.SplitHostPort(address)
		if err != nil || strings.ToLower(strings.TrimSuffix(requested, ".")) != host || port != "443" {
			return nil, sessionError("map_blob_host_denied")
		}
		addresses, err := b.resolve(ctx, host)
		if err != nil || len(addresses) == 0 || len(addresses) > 16 {
			return nil, sessionError("map_blob_dns_unavailable")
		}
		var selected netip.Addr
		for _, addr := range addresses {
			if !publicMapAddress(addr) {
				return nil, sessionError("map_blob_address_denied")
			}
			if !selected.IsValid() || addr.Is4() {
				selected = addr.Unmap()
				if addr.Is4() {
					break
				}
			}
		}
		// Check every answer, even when an IPv4 address was selected first.
		for _, addr := range addresses {
			if !publicMapAddress(addr) {
				return nil, sessionError("map_blob_address_denied")
			}
		}
		return b.dial(ctx, "tcp", net.JoinHostPort(selected.String(), "443"))
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: callTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, value, nil)
	if err != nil {
		return nil, sessionError("map_blob_url_invalid")
	}
	// This is a separate client/request: no account, app, tenant, cookie or
	// authorization header can be inherited from the vendor API transport.
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "")
	resp, err := client.Do(req)
	if err != nil {
		var fault *bridge.Fault
		if errors.As(err, &fault) {
			return nil, fault
		}
		if ctx.Err() != nil {
			return nil, sessionError("map_blob_timeout")
		}
		return nil, sessionError("map_blob_transport_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, sessionError("map_blob_redirect_denied")
	}
	if resp.StatusCode != 200 {
		return nil, sessionError("map_blob_http_failed")
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return nil, sessionError("map_blob_encoding_unsupported")
	}
	if resp.ContentLength > int64(limit) {
		return nil, sessionError("map_blob_response_limit")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, sessionError("map_blob_timeout")
		}
		return nil, sessionError("map_blob_transport_failed")
	}
	if len(body) > limit {
		return nil, sessionError("map_blob_response_limit")
	}
	return body, nil
}
