package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const secret = "SYNTHETIC_PRIVATE_TOKEN_123"

func validConfig() config {
	return config{SiteOrigin: "https://fixture.chatgpt.site", ServiceToken: secret}
}
func configJSON(origin, token string) string {
	b, _ := json.Marshal(map[string]string{"site_origin": origin, "service_token": token})
	return string(b)
}

func TestConfigValidation(t *testing.T) {
	for _, origin := range []string{"https://a.chatgpt.site", "https://probe.owner.chatgpt.site", "https://fixture.chatgpt.site/", "https://a-b.chatgpt.site", "https://" + strings.Repeat("a", 63) + ".chatgpt.site"} {
		if _, err := parseConfig(strings.NewReader(configJSON(origin, secret))); err != nil {
			t.Fatal("valid rejected")
		}
	}
	bad := []string{
		`{}`, `[]`, `null`, `{"site_origin":"https://fixture.chatgpt.site","service_token":"a","extra":true}`,
		`{"site_origin":"https://fixture.chatgpt.site","service_token":"a","service_token":"b"}`,
		`{"site_origin":"https://fixture.chatgpt.site","service_token":1}`, configJSON("https://fixture.chatgpt.site", ""),
		configJSON("https://fixture.chatgpt.site", "space token"), configJSON("https://fixture.chatgpt.site", "new\nline"),
		configJSON("https://fixture.chatgpt.site", strings.Repeat("a", 6145)), strings.Repeat(" ", configLimit+1),
		configJSON("https://fixture.chatgpt.site", secret) + `{}`,
	}
	for _, origin := range []string{"http://fixture.chatgpt.site", "https://evil.test", "https://chatgpt.site", "https://sub..fixture.chatgpt.site", "https://a.-b.chatgpt.site", "https://" + strings.Repeat("aaaa.", 49) + "chatgpt.site", "https://fixture.chatgpt.site:443", "https://user@fixture.chatgpt.site", "https://fixture.chatgpt.site/x", "https://fixture.chatgpt.site/?x=1", "https://fixture.chatgpt.site/#a", "https://fixture.chatgpt.site.", "https://-a.chatgpt.site", "https://a-.chatgpt.site", "https://" + strings.Repeat("a", 64) + ".chatgpt.site", "https://FIXTURE.chatgpt.site", "https://fixture.chatgpt.site%2f"} {
		bad = append(bad, configJSON(origin, secret))
	}
	for i, input := range bad {
		if _, err := parseConfig(strings.NewReader(input)); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func fixture(t *testing.T, handler http.HandlerFunc) (*httptest.Server, testOptions) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal("key")
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture.chatgpt.site"}, DNSNames: []string{"fixture.chatgpt.site"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal("cert")
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		t.Fatal("pair")
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	s := httptest.NewUnstartedServer(handler)
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	s.StartTLS()
	t.Cleanup(s.Close)
	address := s.Listener.Addr().String()
	return s, testOptions{tlsConfig: &tls.Config{RootCAs: pool}, dial: func(ctx context.Context, network, addressRequested string) (net.Conn, error) {
		if addressRequested != "fixture.chatgpt.site:443" {
			t.Error("destination escaped")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
}

func assertPrivate(t *testing.T, r safeReport) {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal("json")
	}
	for _, s := range []string{secret, "fixture.chatgpt.site", "Authorization", "PRIVATE_SERVER_BODY", "127.0.0.1"} {
		if bytes.Contains(b, []byte(s)) {
			t.Fatal("private report")
		}
	}
	if len(b) > 512 {
		t.Fatal("report not bounded")
	}
}

func TestSuccessfulFixedRequestAndPrivacy(t *testing.T) {
	var calls atomic.Int32
	_, opts := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.String() != "/" || r.Host != "fixture.chatgpt.site" || r.Header.Get("OAI-Sites-Authorization") != "Bearer "+secret || r.Header.Get("Authorization") != "" || r.Header.Get("Accept-Encoding") != "" {
			t.Error("request mismatch")
		}
		w.Header().Set("Server", "cloudflare")
		w.Header().Set("CF-Ray", "SYNTHETIC_PRIVATE")
		io.WriteString(w, pageTitle+pageStatus+" PRIVATE_SERVER_BODY")
	})
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	r := probe(context.Background(), validConfig(), opts)
	if r.HTTP != "ok" || r.TLS != "ok" || r.TCP != "ok" || r.HTTPStatus != 200 || !r.ExpectedProbePage || r.WAFBlock || r.ErrorClass != "none" || calls.Load() != 1 {
		t.Fatalf("bad safe result: %+v", r)
	}
	assertPrivate(t, r)
}

func TestRedirectNeverFollowedOrAuthorized(t *testing.T) {
	var contacted atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted.Store(true) }))
	defer other.Close()
	_, opts := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/private", http.StatusFound)
	})
	r := probe(context.Background(), validConfig(), opts)
	if r.HTTP != "redirect_blocked" || r.HTTPStatus != 302 || contacted.Load() {
		t.Fatal("redirect followed")
	}
	assertPrivate(t, r)
}

func TestTLSVerified(t *testing.T) {
	var reached atomic.Bool
	_, opts := fixture(t, func(w http.ResponseWriter, r *http.Request) { reached.Store(true) })
	opts.tlsConfig = &tls.Config{RootCAs: x509.NewCertPool()}
	r := probe(context.Background(), validConfig(), opts)
	if r.ErrorClass != "tls" || r.TLS != "error" || reached.Load() {
		t.Fatalf("TLS guard: %+v", r)
	}
	assertPrivate(t, r)
}

func TestTimeoutAndResponseCaps(t *testing.T) {
	t.Run("header timeout", func(t *testing.T) {
		_, opts := fixture(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		opts.timeout = 40 * time.Millisecond
		r := probe(context.Background(), validConfig(), opts)
		if r.ErrorClass != "timeout" {
			t.Fatal("timeout absent")
		}
		assertPrivate(t, r)
	})
	t.Run("body timeout", func(t *testing.T) {
		_, opts := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})
		opts.timeout = 40 * time.Millisecond
		r := probe(context.Background(), validConfig(), opts)
		if r.ErrorClass != "timeout" || r.HTTP != "read_error" {
			t.Fatal("body timeout absent")
		}
		assertPrivate(t, r)
	})
	t.Run("body cap", func(t *testing.T) {
		_, opts := fixture(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat("x", bodyLimit+1)) })
		r := probe(context.Background(), validConfig(), opts)
		if r.HTTP != "response_limit" {
			t.Fatal("body cap absent")
		}
		assertPrivate(t, r)
	})
	t.Run("header cap", func(t *testing.T) {
		_, opts := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Synthetic", strings.Repeat("x", 32*1024))
			w.WriteHeader(200)
		})
		r := probe(context.Background(), validConfig(), opts)
		if r.HTTP != "error" || r.ErrorClass != "http" {
			t.Fatal("header cap absent")
		}
		assertPrivate(t, r)
	})
}

func TestClassificationAndUntrustedBodies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		class string
	}{{"dns", &net.DNSError{Err: "PRIVATE_SERVER_BODY", Name: "private.test"}, "dns"}, {"tcp", errors.New("PRIVATE_SERVER_BODY"), "tcp"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := probe(context.Background(), validConfig(), testOptions{dial: func(context.Context, string, string) (net.Conn, error) { return nil, tc.err }})
			if r.ErrorClass != tc.class {
				t.Fatal("classification")
			}
			assertPrivate(t, r)
		})
	}
	for _, tc := range []struct {
		code   int
		body   string
		header string
		waf    bool
	}{{403, "Sorry, you have been blocked", "", true}, {429, "Attention Required! | Cloudflare", "", true}, {403, "PRIVATE_SERVER_BODY", "challenge", true}, {403, "PRIVATE_SERVER_BODY", "", false}, {200, "PRIVATE_SERVER_BODY", "", false}} {
		_, opts := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("CF-Mitigated", tc.header)
			w.WriteHeader(tc.code)
			io.WriteString(w, tc.body)
		})
		r := probe(context.Background(), validConfig(), opts)
		if r.WAFBlock != tc.waf || r.ExpectedProbePage {
			t.Fatal("body classification")
		}
		assertPrivate(t, r)
	}
}

func TestMalformedInputNeverRequestsOrLeaks(t *testing.T) {
	var output bytes.Buffer
	if run(strings.NewReader(`{"site_origin":"https://evil.test","service_token":"`+secret+`"}`), &output) != 2 {
		t.Fatal("exit")
	}
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), "evil.test") {
		t.Fatal("disclosure")
	}
	var r safeReport
	if json.Unmarshal(output.Bytes(), &r) != nil || r.ErrorClass != "config" || r.TCP != "not_attempted" {
		t.Fatal("not refused")
	}
}
