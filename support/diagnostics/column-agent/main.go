package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	configLimit    = 8 * 1024
	bodyLimit      = 64 * 1024
	requestTimeout = 6 * time.Second
	pageTitle      = "<title>Private Voice tool probe</title>"
	pageStatus     = "Status: test endpoint available."
)

// The report deliberately has no free-form strings, destination or server data.
type safeReport struct {
	DNS               string `json:"dns"`
	TCP               string `json:"tcp"`
	TLS               string `json:"tls"`
	HTTP              string `json:"http"`
	ErrorClass        string `json:"error_class"`
	HTTPStatus        int    `json:"http_status"`
	ExpectedProbePage bool   `json:"expected_probe_page"`
	WAFBlock          bool   `json:"waf_block"`
}

func emptyReport(class string) safeReport {
	return safeReport{DNS: "not_attempted", TCP: "not_attempted", TLS: "not_attempted", HTTP: "not_attempted", ErrorClass: class}
}

type config struct {
	SiteOrigin   string
	ServiceToken string
}

var originPattern = regexp.MustCompile(`^https://(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+chatgpt\.site/?$`)

func validOrigin(value string) bool {
	return originPattern.MatchString(value) && len(strings.TrimSuffix(strings.TrimPrefix(value, "https://"), "/")) <= 253
}

func parseConfig(r io.Reader) (config, error) {
	var cfg config
	data, err := io.ReadAll(io.LimitReader(r, configLimit+1))
	if err != nil || len(data) > configLimit {
		return cfg, errors.New("config")
	}
	defer clear(data)
	d := json.NewDecoder(strings.NewReader(string(data)))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return cfg, errors.New("config")
	}
	seen := make(map[string]bool, 2)
	for d.More() {
		t, err = d.Token()
		key, ok := t.(string)
		if err != nil || !ok || seen[key] || (key != "site_origin" && key != "service_token") {
			return cfg, errors.New("config")
		}
		seen[key] = true
		var value string
		if d.Decode(&value) != nil {
			return cfg, errors.New("config")
		}
		if key == "site_origin" {
			cfg.SiteOrigin = value
		} else {
			cfg.ServiceToken = value
		}
	}
	if t, err = d.Token(); err != nil || t != json.Delim('}') {
		return config{}, errors.New("config")
	}
	if _, err = d.Token(); err != io.EOF {
		return config{}, errors.New("config")
	}
	if len(seen) != 2 || !validOrigin(cfg.SiteOrigin) || len(cfg.ServiceToken) == 0 || len(cfg.ServiceToken) > 6144 {
		return config{}, errors.New("config")
	}
	for _, b := range []byte(cfg.ServiceToken) {
		if b < 33 || b > 126 {
			return config{}, errors.New("config")
		}
	}
	cfg.SiteOrigin = strings.TrimSuffix(cfg.SiteOrigin, "/")
	return cfg, nil
}

// testOptions is internal; production exposes no custom CA, target or timeout flags.
type testOptions struct {
	dial      func(context.Context, string, string) (net.Conn, error)
	tlsConfig *tls.Config
	timeout   time.Duration
}

func probe(ctx context.Context, cfg config, opts testOptions) safeReport {
	r := emptyReport("none")
	var mu sync.Mutex
	set := func(f func()) { mu.Lock(); f(); mu.Unlock() }
	deadline := requestTimeout
	if opts.timeout > 0 {
		deadline = opts.timeout
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	u, err := url.Parse(cfg.SiteOrigin)
	if err != nil || !validOrigin(cfg.SiteOrigin) {
		return emptyReport("config")
	}
	host := u.Hostname()
	dial := (&net.Dialer{Timeout: requestTimeout, KeepAlive: -1}).DialContext
	if opts.dial != nil {
		dial = opts.dial
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host, NextProtos: []string{"http/1.1"}}
	if opts.tlsConfig != nil {
		tlsConfig = opts.tlsConfig.Clone()
		tlsConfig.ServerName = host
		tlsConfig.MinVersion = tls.VersionTLS12
		tlsConfig.NextProtos = []string{"http/1.1"}
	}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != net.JoinHostPort(host, "443") {
				return nil, errors.New("destination")
			}
			return dial(ctx, network, address)
		},
		TLSClientConfig:   tlsConfig,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
		ForceAttemptHTTP2: false, DisableCompression: true, DisableKeepAlives: true,
		MaxConnsPerHost: 1, MaxResponseHeaderBytes: 16 * 1024,
		TLSHandshakeTimeout: requestTimeout, ResponseHeaderTimeout: requestTimeout,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	trace := &httptrace.ClientTrace{
		DNSDone: func(info httptrace.DNSDoneInfo) {
			set(func() {
				if info.Err == nil {
					r.DNS = "ok"
				} else {
					r.DNS = "error"
				}
			})
		},
		ConnectDone: func(_, _ string, err error) {
			set(func() {
				if err == nil {
					r.TCP = "ok"
				} else {
					r.TCP = "error"
				}
			})
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			set(func() {
				if err == nil {
					r.TLS = "ok"
				} else {
					r.TLS = "error"
				}
			})
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, cfg.SiteOrigin+"/", nil)
	if err != nil {
		return emptyReport("config")
	}
	req.Header.Set("OAI-Sites-Authorization", "Bearer "+cfg.ServiceToken)
	req.Header.Set("User-Agent", "column-preflight/1")
	response, err := client.Do(req)
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		r.HTTP = "error"
		r.ErrorClass = "http"
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) || r.DNS == "error" {
			r.DNS = "error"
			r.ErrorClass = "dns"
		} else if r.TLS == "error" {
			r.ErrorClass = "tls"
		} else if r.TCP != "ok" {
			r.TCP = "error"
			r.ErrorClass = "tcp"
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			r.ErrorClass = "timeout"
		}
		return r
	}
	defer response.Body.Close()
	r.HTTPStatus = response.StatusCode
	if response.StatusCode >= 300 && response.StatusCode <= 399 {
		r.HTTP = "redirect_blocked"
		r.ErrorClass = "http"
		return r
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, bodyLimit+1))
	defer clear(data)
	if err != nil {
		r.HTTP = "read_error"
		r.ErrorClass = "http"
		if ctx.Err() != nil {
			r.ErrorClass = "timeout"
		}
		return r
	}
	if len(data) > bodyLimit {
		r.HTTP = "response_limit"
		r.ErrorClass = "http"
		return r
	}
	body := strings.ToLower(string(data))
	r.WAFBlock = strings.EqualFold(response.Header.Get("CF-Mitigated"), "challenge") || ((response.StatusCode == 403 || response.StatusCode == 429) && (strings.Contains(body, "sorry, you have been blocked") || strings.Contains(body, "attention required! | cloudflare") || (strings.Contains(body, "cf-error-details") && strings.Contains(body, "cloudflare"))))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		r.HTTP = "ok"
		r.ExpectedProbePage = strings.Contains(string(data), pageTitle) && strings.Contains(string(data), pageStatus)
	} else {
		r.HTTP = "status_error"
		r.ErrorClass = "http"
	}
	return r
}

func run(input io.Reader, output io.Writer) int {
	cfg, err := parseConfig(input)
	if err != nil {
		_ = json.NewEncoder(output).Encode(emptyReport("config"))
		return 2
	}
	r := probe(context.Background(), cfg, testOptions{})
	_ = json.NewEncoder(output).Encode(r)
	if r.HTTP == "ok" {
		return 0
	}
	return 1
}

func main() {
	if len(os.Args) != 1 || disableCore() != nil {
		_ = json.NewEncoder(os.Stdout).Encode(emptyReport("config"))
		os.Exit(2)
	}
	os.Exit(run(os.Stdin, os.Stdout))
}
