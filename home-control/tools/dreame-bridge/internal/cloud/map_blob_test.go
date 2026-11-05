package cloud

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Synthetic TLS server only. Public-IP fixtures are intercepted by this private
// dial hook before any network connection, and mapped to the local test server.
func syntheticBlobReader(t *testing.T, handler http.HandlerFunc) (*mapBlobReader, *atomic.Int32) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"storage.example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	reader := newMapBlobReader([]string{"storage.example.com"})
	reader.roots = roots
	reader.resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host != "storage.example.com" {
			t.Fatal("nonapproved DNS name")
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	var dials atomic.Int32
	reader.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != "8.8.8.8:443" {
			t.Fatal("DNS-to-dial binding lost")
		}
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	return reader, &dials
}

func TestMapBlobTransportIsExactHostTLSAndCredentialIsolated(t *testing.T) {
	var requests atomic.Int32
	b, _ := syntheticBlobReader(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" || r.Host != "storage.example.com" || r.TLS == nil || r.TLS.ServerName != "storage.example.com" {
			t.Error("storage TLS/host/method changed")
		}
		for _, h := range []string{"Authorization", "Dreame-Auth", "Tenant-Id", "Dreame-Meta", "Dreame-Rlc", "Cookie", "Referer", "User-Agent"} {
			if r.Header.Get(h) != "" {
				t.Error("account/app/cookie header reached blob host")
			}
		}
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("compression enabled")
		}
		io.WriteString(w, "synthetic-blob")
	})
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	raw, err := b.fetch(context.Background(), "https://storage.example.com/list?signature=synthetic", 1024)
	if err != nil || string(raw) != "synthetic-blob" || requests.Load() != 1 {
		t.Fatal("isolated exact-host read failed")
	}
	for _, value := range []string{"https://other.example.com/list", "https://sub.storage.example.com/list", "http://storage.example.com/list", "https://user:private@storage.example.com/list", "https://storage.example.com:8443/list", "https://127.0.0.1/list"} {
		if _, err := b.fetch(context.Background(), value, 1024); err == nil {
			t.Fatal("unapproved blob URL accepted")
		}
	}
	if requests.Load() != 1 {
		t.Fatal("unapproved host contacted")
	}
}

func TestMapBlobPublicAddressAndRebindingBoundary(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.169.254", "192.0.2.1", "198.18.0.1", "203.0.113.1", "240.1.2.3", "::1", "fc00::1", "fe80::1", "2001:db8::1", "64:ff9b::a00:1", "2002:7f00:1::", "3fff::1"} {
		if publicMapAddress(netip.MustParseAddr(value)) {
			t.Fatal("nonpublic address accepted")
		}
	}
	for _, value := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicMapAddress(netip.MustParseAddr(value)) {
			t.Fatal("ordinary public address rejected")
		}
	}
	b, dials := syntheticBlobReader(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	var lookups atomic.Int32
	b.resolve = func(context.Context, string) ([]netip.Addr, error) {
		if lookups.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	if _, err := b.fetch(context.Background(), "https://storage.example.com/list", 1024); err != nil {
		t.Fatal(err)
	}
	if _, err := b.fetch(context.Background(), "https://storage.example.com/list", 1024); err == nil || err.Error() != "map_blob_address_denied" {
		t.Fatal("changed DNS answer was trusted")
	}
	if dials.Load() != 1 {
		t.Fatal("unsafe rebound address dialed")
	}
	b.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, nil
	}
	if _, err := b.fetch(context.Background(), "https://storage.example.com/list", 1024); err == nil || dials.Load() != 1 {
		t.Fatal("mixed DNS answer dialed")
	}
}

func TestMapBlobTLSRedirectEncodingAndBodyLimits(t *testing.T) {
	for _, tc := range []struct {
		name, code     string
		status         int
		encoding, body string
	}{
		{"redirect", "map_blob_redirect_denied", 302, "", ""}, {"http", "map_blob_http_failed", 403, "", "private-body"}, {"encoding", "map_blob_encoding_unsupported", 200, "gzip", "private-body"}, {"limit", "map_blob_response_limit", 200, "", strings.Repeat("x", 1025)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			b, _ := syntheticBlobReader(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "https://untrusted.invalid/never-follow")
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			if _, err := b.fetch(context.Background(), "https://storage.example.com/list?private", 1024); err == nil || err.Error() != tc.code || calls.Load() != 1 {
				t.Fatal("blob restriction bypassed or raw error exposed")
			}
		})
	}
	b, _ := syntheticBlobReader(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	b.roots = nil
	if _, err := b.fetch(context.Background(), "https://storage.example.com/list", 1024); err == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
}
