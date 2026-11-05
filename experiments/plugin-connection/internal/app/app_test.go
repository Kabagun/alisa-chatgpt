package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"smart-home-plugin/internal/oauth"
	"smart-home-plugin/internal/provider"
)

const testOrigin = "https://bridge.example.invalid"
const testRedirect = "https://client.example.invalid/callback"
const testVerifier = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGH"

type fakeProvider struct {
	mu             sync.Mutex
	accounts       map[string]string
	calls, logins  int
	block, entered chan struct{}
}

func (f *fakeProvider) ID() string    { return "fake" }
func (f *fakeProvider) Label() string { return "Synthetic provider" }
func (f *fakeProvider) Authenticate(_ context.Context, dir string, c provider.Credentials) error {
	f.mu.Lock()
	f.logins++
	f.mu.Unlock()
	if c.Password == "reject-marker" {
		return errors.New("raw-vendor-secret-reject-marker")
	}
	if c.Password == "panic-marker" {
		panic("raw-vendor-secret-panic-marker")
	}
	if c.Password == "unverified-marker" {
		return provider.Err("account_connection_unverified")
	}
	if c.Region != "" {
		return provider.Err("invalid_region")
	}
	if os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{"session":"synthetic-only"}`), 0600) != nil {
		return provider.Err("provider_unavailable")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[dir] = fmt.Sprintf("device-%d", len(f.accounts)+1)
	return nil
}
func (f *fakeProvider) Execute(ctx context.Context, dir string, op provider.Operation, p provider.Params) (json.RawMessage, error) {
	f.mu.Lock()
	device := f.accounts[dir]
	f.calls++
	f.mu.Unlock()
	if f.block != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, provider.Err("upstream_unavailable")
		}
	}
	if device == "" || (op != provider.Devices && p.DeviceID != device) {
		return nil, provider.Err("device_not_found")
	}
	if op == provider.CleanRooms {
		return nil, provider.Err("room_control_unverified")
	}
	data, _ := json.Marshal(map[string]any{"device_id": device, "operation": op})
	return data, nil
}

type testApp struct {
	h       *Handler
	p       *fakeProvider
	dir     string
	seconds atomic.Int64
}

func appFixture(t *testing.T, onAuthDenial ...func(string)) *testApp {
	t.Helper()
	f := &testApp{p: &fakeProvider{accounts: map[string]string{}}, dir: filepath.Join(t.TempDir(), "private")}
	f.seconds.Store(1793404800)
	config := Config{PublicOrigin: testOrigin, DataDir: f.dir, Providers: []provider.Provider{f.p}, Clock: func() time.Time { return time.Unix(f.seconds.Load(), 0) }, AccessTTL: time.Minute}
	if len(onAuthDenial) == 1 {
		config.OnAuthDenial = onAuthDenial[0]
	}
	h, e := New(config)
	if e != nil {
		t.Fatal(e)
	}
	f.h = h
	t.Cleanup(func() { h.Close() })
	return f
}
func request(h *Handler, method, path, body, content, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, testOrigin+path, strings.NewReader(body))
	r.Host = "spoofed.example.invalid"
	r.Header.Set("X-OpenAI-User-ID", "foreign")
	r.Header.Set("oai-authenticated-user-id", "foreign")
	r.Header.Set("X-Forwarded-Host", "spoofed.example.invalid")
	if content != "" {
		r.Header.Set("Content-Type", content)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type connection struct {
	tokens oauth.Tokens
	client string
}

func begin(t *testing.T, f *testApp, scope string) (string, url.Values, *http.Cookie) {
	t.Helper()
	b := `{"client_name":"<b>untrusted client</b>","redirect_uris":["` + testRedirect + `"],"token_endpoint_auth_method":"none"}`
	w := request(f.h, "POST", "/oauth/register", b, "application/json", "")
	if w.Code != 201 {
		t.Fatalf("register status %d", w.Code)
	}
	var c struct {
		ID string `json:"client_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	q := url.Values{"client_id": {c.ID}, "redirect_uri": {testRedirect}, "response_type": {"code"}, "state": {"opaque-client-state"}, "code_challenge": {oauth.Challenge(testVerifier)}, "code_challenge_method": {"S256"}, "resource": {testOrigin + "/mcp"}, "scope": {scope}}
	w = request(f.h, "GET", "/oauth/authorize?"+q.Encode(), "", "", "")
	if w.Code != 200 {
		t.Fatalf("authorize status %d", w.Code)
	}
	if w.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatal("authorization form policy would hide the same-origin POST origin")
	}
	if strings.Contains(w.Body.String(), "<b>untrusted") || strings.Contains(w.Body.String(), "spoofed.example") || !strings.Contains(w.Body.String(), "&lt;b&gt;") {
		t.Fatal("client markup or incoming host trusted")
	}
	if strings.Contains(w.Body.String(), "name=region") {
		t.Fatal("authorization form asks the user for a cloud region")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Path != "/" || cookies[0].MaxAge != 300 {
		t.Fatal("flow cookie unsafe")
	}
	find := func(name string) string {
		m := regexp.MustCompile(`name=` + name + ` value="([^"]+)"`).FindStringSubmatch(w.Body.String())
		if len(m) != 2 {
			t.Fatal("missing form proof")
		}
		return m[1]
	}
	if cookies[0].Name != flowCookiePrefix+find("flow") {
		t.Fatal("flow cookie is not isolated to this authorization attempt")
	}
	v := url.Values{"flow": {find("flow")}, "csrf": {find("csrf")}, "provider": {find("provider")}, "username": {"synthetic-user"}, "password": {"synthetic-password"}, "confirm": {"yes"}}
	return c.ID, v, cookies[0]
}
func login(f *testApp, v url.Values, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	return loginCookies(f, v, []*http.Cookie{cookie}, origin)
}
func loginCookies(f *testApp, v url.Values, cookies []*http.Cookie, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", testOrigin+"/oauth/authorize", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", origin)
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func TestConcurrentAuthorizationTabsKeepIndependentCookies(t *testing.T) {
	f := appFixture(t)
	_, first, firstCookie := begin(t, f, oauth.ReadScope)
	_, second, secondCookie := begin(t, f, oauth.ReadScope)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, _ := url.Parse(testOrigin + "/oauth/authorize")
	jar.SetCookies(endpoint, []*http.Cookie{firstCookie})
	jar.SetCookies(endpoint, []*http.Cookie{secondCookie})
	for _, form := range []url.Values{first, second} {
		w := loginCookies(f, form, jar.Cookies(endpoint), testOrigin)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("a second authorization tab invalidated the first: status %d", w.Code)
		}
		jar.SetCookies(endpoint, w.Result().Cookies())
	}
	if len(jar.Cookies(endpoint)) != 0 {
		t.Fatal("completed flow cookies were not removed")
	}
}

func connect(t *testing.T, f *testApp, scope string) connection {
	t.Helper()
	client, v, cookie := begin(t, f, scope)
	w := login(f, v, cookie, testOrigin)
	if w.Code != 303 {
		t.Fatalf("provider login status %d", w.Code)
	}
	u, e := url.Parse(w.Header().Get("Location"))
	if e != nil || u.Scheme != "https" || u.Host != "client.example.invalid" || u.Query().Get("state") != "opaque-client-state" {
		t.Fatal("redirect/state binding lost")
	}
	v = url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "code": {u.Query().Get("code")}, "redirect_uri": {testRedirect}, "code_verifier": {testVerifier}, "resource": {testOrigin + "/mcp"}}
	w = request(f.h, "POST", "/oauth/token", v.Encode(), "application/x-www-form-urlencoded", "")
	var tokens oauth.Tokens
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &tokens) != nil || tokens.AccessToken == "" {
		t.Fatalf("token exchange status %d", w.Code)
	}
	return connection{tokens, client}
}
func call(h *Handler, token, name, args string) *httptest.ResponseRecorder {
	return request(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`, "application/json", token)
}
func TestOAuthHTTPConnectionAndMCPAuthBoundary(t *testing.T) {
	f := appFixture(t)
	w := request(f.h, "GET", "/.well-known/oauth-protected-resource", "", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), testOrigin+"/mcp") || strings.Contains(w.Body.String(), "spoofed") {
		t.Fatal("resource metadata unbound")
	}
	w = request(f.h, "POST", "/mcp", `{}`, "application/json", "")
	if w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatal("spoofed identity authenticated")
	}
	c := connect(t, f, oauth.ReadScope)
	w = call(f.h, c.tokens.AccessToken, "home_devices", `{}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "device-1") {
		t.Fatal("authenticated device query failed")
	}
	for _, path := range []string{"/", "/health"} {
		w = request(f.h, "GET", path, "", "", "")
		if strings.Contains(w.Body.String(), "device-1") || strings.Contains(w.Body.String(), "synthetic-user") {
			t.Fatal("public page disclosed an account")
		}
	}
	w = request(f.h, "GET", "/mcp", "", "", c.tokens.AccessToken)
	if w.Code != 405 {
		t.Fatal("authenticated MCP GET accepted")
	}
	f.seconds.Add(61)
	w = call(f.h, c.tokens.AccessToken, "home_devices", `{}`)
	if w.Code != 401 {
		t.Fatal("expired access accepted")
	}
}
func TestScopeAnnotationsAndUnverifiedControl(t *testing.T) {
	f := appFixture(t)
	read := connect(t, f, oauth.ReadScope)
	w := request(f.h, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "application/json", read.tokens.AccessToken)
	var list struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Annotations map[string]bool `json:"annotations"`
				Security    []any           `json:"securitySchemes"`
			} `json:"tools"`
		} `json:"result"`
	}
	if json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Result.Tools) != 4 {
		t.Fatal("tool list invalid")
	}
	for _, tool := range list.Result.Tools {
		if tool.Annotations["readOnlyHint"] != (tool.Name != "home_clean_rooms") || len(tool.Security) != 1 {
			t.Fatal("tool authorization annotation wrong")
		}
	}
	w = call(f.h, read.tokens.AccessToken, "home_clean_rooms", `{"device_id":"device-1","map_id":"map","room_ids":["1"]}`)
	if !strings.Contains(w.Body.String(), "mcp/www_authenticate") || !strings.Contains(w.Body.String(), "insufficient_scope") || !strings.Contains(w.Body.String(), "error_description=") {
		t.Fatal("control scope not enforced")
	}
	if f.p.calls != 0 {
		t.Fatal("read token invoked control")
	}
	control := connect(t, f, oauth.ReadScope+" "+oauth.ControlScope)
	w = call(f.h, control.tokens.AccessToken, "home_clean_rooms", `{"device_id":"device-2","map_id":"map","room_ids":["1"]}`)
	if !strings.Contains(w.Body.String(), "room_control_unverified") || !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("unverified cleaning presented as success")
	}
}
func TestSubjectIsolationAndCallerPathsRejected(t *testing.T) {
	f := appFixture(t)
	a := connect(t, f, oauth.ReadScope)
	b := connect(t, f, oauth.ReadScope)
	for _, tc := range []struct{ token, device, unexpected string }{{a.tokens.AccessToken, "device-1", "device-2"}, {b.tokens.AccessToken, "device-2", "device-1"}} {
		w := call(f.h, tc.token, "home_devices", `{}`)
		if !strings.Contains(w.Body.String(), tc.device) || strings.Contains(w.Body.String(), tc.unexpected) {
			t.Fatal("subject account binding crossed")
		}
	}
	w := call(f.h, a.tokens.AccessToken, "home_status", `{"device_id":"device-2"}`)
	if !strings.Contains(w.Body.String(), "device_not_found") {
		t.Fatal("foreign device accepted")
	}
	before := f.p.calls
	w = call(f.h, a.tokens.AccessToken, "home_devices", `{"privateAccountDir":"/arbitrary","provider":"foreign"}`)
	if !strings.Contains(w.Body.String(), "-32602") || f.p.calls != before {
		t.Fatal("caller account path accepted")
	}
}
func TestCSRFVendorFailureRedactionAndCleanup(t *testing.T) {
	f := appFixture(t)
	_, v, cookie := begin(t, f, oauth.ReadScope)
	if w := login(f, v, cookie, "https://foreign.example.invalid"); w.Code != 403 {
		t.Fatal("foreign origin accepted")
	}
	v.Set("csrf", "wrong")
	if w := login(f, v, cookie, testOrigin); w.Code != 403 {
		t.Fatal("wrong CSRF accepted")
	}
	_, v, cookie = begin(t, f, oauth.ReadScope)
	v.Set("password", "reject-marker")
	w := login(f, v, cookie, testOrigin)
	if w.Code != 400 || strings.Contains(w.Body.String(), "reject-marker") || strings.Contains(w.Body.String(), "raw-vendor") {
		t.Fatal("provider failure exposed secrets")
	}
	entries, e := os.ReadDir(filepath.Join(f.dir, "accounts"))
	if e != nil || len(entries) != 0 {
		t.Fatal("failed login left an account directory")
	}
}

func TestAuthorizationFormOriginPolicyAndLoginBoundary(t *testing.T) {
	f := appFixture(t)
	for _, path := range []string{"/", "/health", "/.well-known/oauth-protected-resource", "/.well-known/oauth-authorization-server", "/mcp"} {
		w := request(f.h, "GET", path, "", "", "")
		if w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("non-authorization response %q exposes referrer information", path)
		}
	}
	for _, tc := range []struct {
		name    string
		origins []string
		status  int
		gate    string
	}{
		{"missing", nil, http.StatusForbidden, "origin_missing"},
		{"null", []string{"null"}, http.StatusForbidden, "origin_null"},
		{"foreign", []string{"https://foreign.example.invalid"}, http.StatusForbidden, "origin_foreign"},
		{"duplicate", []string{testOrigin, testOrigin}, http.StatusForbidden, "origin_duplicate"},
		{"same origin", []string{testOrigin}, http.StatusSeeOther, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gates []string
			f := appFixture(t, func(gate string) { gates = append(gates, gate) })
			_, v, cookie := begin(t, f, oauth.ReadScope)
			r := httptest.NewRequest("POST", testOrigin+"/oauth/authorize", strings.NewReader(v.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for _, origin := range tc.origins {
				r.Header.Add("Origin", origin)
			}
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			f.h.ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Fatalf("login response: status %d, policy %q", w.Code, w.Header().Get("Referrer-Policy"))
			}
			if tc.gate == "" && len(gates) != 0 || tc.gate != "" && (len(gates) != 1 || gates[0] != tc.gate) {
				t.Fatal("origin denial audit did not contain only its fixed gate code")
			}
			f.p.mu.Lock()
			logins := f.p.logins
			f.p.mu.Unlock()
			if tc.status == http.StatusForbidden {
				if logins != 0 || !strings.Contains(w.Body.String(), `"error":"access_denied"`) {
					t.Fatal("untrusted form origin reached provider authentication")
				}
				// A rejected origin must not consume an otherwise valid flow.
				if w := login(f, v, cookie, testOrigin); w.Code != http.StatusSeeOther {
					t.Fatal("origin rejection consumed the valid form proof")
				}
			} else if logins != 1 {
				t.Fatal("valid same-origin form did not authenticate exactly once")
			}
		})
	}
}

func TestAuthorizationFlowDenialsAreSafeAndExplainFreshRetry(t *testing.T) {
	for _, tc := range []struct{ name, gate string }{
		{"missing cookie", "cookie_missing"},
		{"duplicate cookie", "cookie_duplicate"},
		{"another flow cookie", "cookie_missing"},
		{"wrong cookie proof", "flow_proof_or_expiry"},
		{"wrong csrf", "flow_proof_or_expiry"},
		{"cross-flow csrf", "flow_proof_or_expiry"},
		{"expired", "flow_proof_or_expiry"},
		{"consumed", "flow_proof_or_expiry"},
		{"invalid flow", "flow_proof_or_expiry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gates []string
			f := appFixture(t, func(gate string) { gates = append(gates, gate) })
			_, form, cookie := begin(t, f, oauth.ReadScope)
			cookies := []*http.Cookie{cookie}
			expectedLogins := 0
			switch tc.name {
			case "missing cookie":
				cookies = nil
			case "duplicate cookie":
				cookies = append(cookies, cookie)
			case "another flow cookie":
				_, _, secondCookie := begin(t, f, oauth.ReadScope)
				cookies = []*http.Cookie{secondCookie}
			case "wrong cookie proof":
				changed := *cookie
				changed.Value = "synthetic-wrong-cookie-proof"
				cookies = []*http.Cookie{&changed}
			case "wrong csrf":
				form.Set("csrf", "synthetic-wrong-csrf")
			case "cross-flow csrf":
				_, second, secondCookie := begin(t, f, oauth.ReadScope)
				form.Set("csrf", second.Get("csrf"))
				cookies = append(cookies, secondCookie)
			case "expired":
				f.seconds.Add(301)
			case "consumed":
				if w := login(f, form, cookie, testOrigin); w.Code != http.StatusSeeOther {
					t.Fatal("initial authorization failed")
				}
				expectedLogins = 1
			case "invalid flow":
				form.Set("flow", "synthetic-invalid-flow")
			}
			w := loginCookies(f, form, cookies, testOrigin)
			if w.Code != http.StatusForbidden || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(w.Body.String(), "Для новой попытки начните подключение заново в приложении.") {
				t.Fatal("invalid flow did not explain how to start a fresh connection")
			}
			if len(gates) != 1 || gates[0] != tc.gate {
				t.Fatal("flow denial audit did not contain only its fixed gate code")
			}
			for _, private := range []string{form.Get("username"), form.Get("password"), form.Get("flow"), form.Get("csrf"), cookie.Name, cookie.Value, tc.gate} {
				if private != "" && strings.Contains(w.Body.String(), private) {
					t.Fatal("flow denial page reflected request data or an internal gate")
				}
			}
			f.p.mu.Lock()
			logins := f.p.logins
			f.p.mu.Unlock()
			if logins != expectedLogins {
				t.Fatal("invalid flow reached provider authentication")
			}
		})
	}
}

func TestRequestBoundsMalformedRPCAndDuplicateAuth(t *testing.T) {
	f := appFixture(t)
	c := connect(t, f, oauth.ReadScope)
	for _, body := range []string{`{`, `[]`, `{"jsonrpc":"2.0","jsonrpc":"2.0","id":1,"method":"ping"}`, `{"jsonrpc":"2.0","id":{},"method":"ping"}`, `{"jsonrpc":"2.0","id":1,"method":"unknown"}`} {
		w := request(f.h, "POST", "/mcp", body, "application/json", c.tokens.AccessToken)
		if !strings.Contains(w.Body.String(), `"error"`) {
			t.Fatal("malformed/unknown RPC accepted")
		}
	}
	w := request(f.h, "POST", "/mcp", strings.Repeat("x", bodyLimit+1), "application/json", c.tokens.AccessToken)
	if w.Code != 413 {
		t.Fatal("oversized body accepted")
	}
	r := httptest.NewRequest("POST", testOrigin+"/mcp", bytes.NewBufferString(`{}`))
	r.Header.Add("Authorization", "Bearer "+c.tokens.AccessToken)
	r.Header.Add("Authorization", "Bearer "+c.tokens.AccessToken)
	w = httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("duplicated bearer accepted")
	}
	w = request(f.h, "POST", "/mcp", `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"home_devices"}}`, "application/json", c.tokens.AccessToken)
	if w.Code != 204 || f.p.calls != 0 {
		t.Fatal("notification executed a tool")
	}
	w = call(f.h, c.tokens.AccessToken, "home_status", `{"device_id":"device-1","device_id":"device-1"}`)
	if !strings.Contains(w.Body.String(), "-32600") {
		t.Fatal("duplicate argument accepted")
	}
}
func TestConcurrentSubjectOperationIsBounded(t *testing.T) {
	f := appFixture(t)
	c := connect(t, f, oauth.ReadScope)
	f.p.block = make(chan struct{})
	f.p.entered = make(chan struct{}, 1)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call(f.h, c.tokens.AccessToken, "home_devices", `{}`) }()
	<-f.p.entered
	w := call(f.h, c.tokens.AccessToken, "home_devices", `{}`)
	if !strings.Contains(w.Body.String(), "busy") {
		t.Fatal("concurrent account operation admitted")
	}
	close(f.p.block)
	if (<-done).Code != 200 {
		t.Fatal("first operation failed")
	}
}

func TestRegistrationAcceptsOptionalClientMetadata(t *testing.T) {
	for _, metadata := range []string{
		`"client_uri":"https://client.example.invalid/"`,
		`"logo_uri":"https://client.example.invalid/logo.png"`,
		`"client_uri":"https://client.example.invalid/","logo_uri":"https://client.example.invalid/logo.png","example_extension":{"enabled":true}`,
		`"privateAccountDir":"/caller-selected"`,
	} {
		t.Run(metadata, func(t *testing.T) {
			f := appFixture(t)
			body := `{"client_name":"Synthetic client","redirect_uris":["` + testRedirect + `"],` + metadata + `}`
			w := request(f.h, "POST", "/oauth/register", body, "application/json", "")
			var registered map[string]json.RawMessage
			if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &registered) != nil || len(registered["client_id"]) == 0 {
				t.Fatalf("optional metadata rejected: status %d", w.Code)
			}
			if string(registered["client_name"]) != `"Synthetic client"` || string(registered["redirect_uris"]) != `["`+testRedirect+`"]` {
				t.Fatal("supported metadata changed")
			}
			for _, ignored := range []string{"client_uri", "logo_uri", "example_extension", "privateAccountDir"} {
				if _, ok := registered[ignored]; ok {
					t.Fatalf("unsupported metadata %q was registered", ignored)
				}
			}
		})
	}
}

func TestRegistrationRejectsInvalidKnownMetadataAndUnboundedJSON(t *testing.T) {
	for _, tc := range []struct{ name, body, code string }{
		{"missing redirects", `{}`, "invalid_client_metadata"},
		{"null redirects", `{"redirect_uris":null}`, "invalid_client_metadata"},
		{"redirect type", `{"redirect_uris":"` + testRedirect + `"}`, "invalid_client_metadata"},
		{"redirect element type", `{"redirect_uris":[123]}`, "invalid_client_metadata"},
		{"insecure redirect", `{"redirect_uris":["http://public.example.invalid/cb"]}`, "invalid_redirect_uri"},
		{"duplicate redirect key", `{"redirect_uris":["` + testRedirect + `"],"redirect_uris":["` + testRedirect + `"]}`, "invalid_client_metadata"},
		{"escaped duplicate key", `{"redirect_uris":["` + testRedirect + `"],"\u0072edirect_uris":["` + testRedirect + `"]}`, "invalid_client_metadata"},
		{"duplicate unknown key", `{"redirect_uris":["` + testRedirect + `"],"extension":{"x":1,"x":2}}`, "invalid_client_metadata"},
		{"invalid known type with optional metadata", `{"redirect_uris":["` + testRedirect + `"],"client_name":42,"client_uri":"https://client.example.invalid/"}`, "invalid_client_metadata"},
		{"unsupported grant", `{"redirect_uris":["` + testRedirect + `"],"grant_types":["client_credentials"]}`, "invalid_client_metadata"},
		{"unsupported response", `{"redirect_uris":["` + testRedirect + `"],"response_types":["token"]}`, "invalid_client_metadata"},
		{"unsupported auth", `{"redirect_uris":["` + testRedirect + `"],"token_endpoint_auth_method":"client_secret_basic"}`, "invalid_client_metadata"},
		{"unsupported scope", `{"redirect_uris":["` + testRedirect + `"],"scope":"unknown"}`, "invalid_scope"},
		{"trailing object", `{"redirect_uris":["` + testRedirect + `"]}{}`, "invalid_client_metadata"},
		{"deep unknown metadata", `{"redirect_uris":["` + testRedirect + `"],"extension":` + strings.Repeat("[", 13) + `0` + strings.Repeat("]", 13) + `}`, "invalid_client_metadata"},
		{"oversized unknown metadata", `{"redirect_uris":["` + testRedirect + `"],"extension":"` + strings.Repeat("x", bodyLimit) + `"}`, "invalid_client_metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := appFixture(t)
			w := request(f.h, "POST", "/oauth/register", tc.body, "application/json", "")
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"error":"`+tc.code+`"`) {
				t.Fatalf("invalid metadata response: status %d, body %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRussianPagesAndConsentPermissions(t *testing.T) {
	f := appFixture(t)
	const csp = "default-src 'none'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	w := request(f.h, "GET", "/", "", "", "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Security-Policy") != csp {
		t.Fatal("public page or content security policy changed")
	}
	for _, text := range []string{"<html lang=ru>", `name=viewport content="width=device-width, initial-scale=1"`, "Уборка выбранных комнат пока не включена.", "Открытие этой страницы не подключает аккаунт."} {
		if !strings.Contains(w.Body.String(), text) {
			t.Fatalf("public page missing %q", text)
		}
	}
	for _, scope := range []string{oauth.ReadScope, oauth.ControlScope, oauth.ReadScope + " " + oauth.ControlScope} {
		t.Run(scope, func(t *testing.T) {
			client, _, _ := begin(t, f, scope)
			q := url.Values{"client_id": {client}, "redirect_uri": {testRedirect}, "response_type": {"code"}, "code_challenge": {oauth.Challenge(testVerifier)}, "code_challenge_method": {"S256"}, "resource": {testOrigin + "/mcp"}, "scope": {scope}}
			w := request(f.h, "GET", "/oauth/authorize?"+q.Encode(), "", "", "")
			body := w.Body.String()
			if w.Code != http.StatusOK || w.Header().Get("Content-Security-Policy") != csp || !strings.Contains(body, "<html lang=ru>") || !strings.Contains(body, `action="`+testOrigin+`/oauth/authorize"`) {
				t.Fatal("consent page or security policy changed")
			}
			if strings.Contains(body, "Просмотр устройств, их состояния и карты.") != strings.Contains(scope, oauth.ReadScope) || strings.Contains(body, "Запуск уборки выбранных комнат.") != strings.Contains(scope, oauth.ControlScope) {
				t.Fatal("consent descriptions do not match requested permissions")
			}
			if strings.Contains(scope, oauth.ControlScope) && !strings.Contains(body, "Для Dreame эта возможность пока не включена.") {
				t.Fatal("unavailable room cleaning was presented as enabled")
			}
			if strings.Contains(body, oauth.ReadScope) || strings.Contains(body, oauth.ControlScope) {
				t.Fatal("consent wording is unclear")
			}
			if strings.Contains(body, "name=region") || strings.Contains(body, "Регион аккаунта") || strings.Contains(body, "центр обработки данных") {
				t.Fatal("consent page exposes technical cloud routing")
			}
		})
	}
}

func TestProviderChoiceOnlyWithMultipleProviders(t *testing.T) {
	for _, tc := range []struct {
		name      string
		providers []providerOption
		selection bool
	}{
		{"one", []providerOption{{"fake", "<b>Synthetic provider</b>"}}, false},
		{"multiple", []providerOption{{"fake", "<b>Synthetic provider</b>"}, {"another", "Another provider"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rendered strings.Builder
			err := loginTemplate.Execute(&rendered, map[string]any{
				"Client": "Synthetic client", "Permissions": scopePermissions(oauth.ReadScope),
				"Action": testOrigin + "/oauth/authorize", "Flow": "synthetic-flow", "CSRF": "synthetic-csrf", "Providers": tc.providers,
			})
			if err != nil {
				t.Fatal(err)
			}
			body := rendered.String()
			if strings.Contains(body, `<select id=provider name=provider required>`) != tc.selection || strings.Contains(body, `<input type=hidden name=provider value="fake">`) == tc.selection {
				t.Fatal("provider choice does not match the configured providers")
			}
			if !strings.Contains(body, "&lt;b&gt;Synthetic provider&lt;/b&gt;") || strings.Contains(body, "<b>Synthetic provider</b>") {
				t.Fatal("provider label was not safely displayed")
			}
			if tc.selection && (!strings.Contains(body, `<option value="" selected>Выберите производителя</option>`) || !strings.Contains(body, `<option value="another">Another provider</option>`)) {
				t.Fatal("configured provider choices are missing")
			}
		})
	}
}

func TestLoginRejectsCallerRegionBeforeProviderAuthentication(t *testing.T) {
	f := appFixture(t)
	_, v, cookie := begin(t, f, oauth.ReadScope)
	v.Set("region", "eu")
	w := login(f, v, cookie, testOrigin)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"error":"invalid_request"`) {
		t.Fatal("caller-supplied cloud region accepted")
	}
	f.p.mu.Lock()
	logins := f.p.logins
	f.p.mu.Unlock()
	if logins != 0 {
		t.Fatal("caller-supplied cloud region reached provider authentication")
	}
	v.Del("region")
	if w := login(f, v, cookie, testOrigin); w.Code != http.StatusSeeOther {
		t.Fatal("provider-owned automatic region did not authenticate")
	}
}

func TestProviderAuthenticationFailuresHaveSafeRussianHTML(t *testing.T) {
	for _, tc := range []struct{ password, message string }{
		{"reject-marker", "Не удалось подключить аккаунт."},
		{"panic-marker", "Не удалось подключить аккаунт."},
		{"unverified-marker", "Не удалось подтвердить доступ к устройствам. Подключение не сохранено."},
	} {
		t.Run(tc.password, func(t *testing.T) {
			f := appFixture(t)
			_, v, cookie := begin(t, f, oauth.ReadScope)
			v.Set("password", tc.password)
			w := login(f, v, cookie, testOrigin)
			if w.Code != http.StatusBadRequest || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("Referrer-Policy") != "no-referrer" || !strings.Contains(w.Body.String(), tc.message) {
				t.Fatal("provider failure is not explained safely in Russian")
			}
			for _, secret := range []string{tc.password, "synthetic-user", "raw-vendor", "account_connection_unverified"} {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatal("provider failure exposed credentials or internal errors")
				}
			}
			entries, err := os.ReadDir(filepath.Join(f.dir, "accounts"))
			if err != nil || len(entries) != 0 {
				t.Fatal("failed authentication saved an account")
			}
		})
	}
}

func TestAuthorizationGETIgnoresUnsupportedParameters(t *testing.T) {
	f := appFixture(t)
	client, _, _ := begin(t, f, oauth.ReadScope)
	query := func() url.Values {
		return url.Values{
			"client_id": {client}, "redirect_uri": {testRedirect}, "response_type": {"code"},
			"state": {"synthetic-extension-state"}, "code_challenge": {oauth.Challenge(testVerifier)},
			"code_challenge_method": {"S256"}, "resource": {testOrigin + "/mcp"}, "scope": {oauth.ReadScope},
			"ui_locales": {"ru-RU"}, "example_extension": {"ignored-extension-marker"},
		}
	}
	t.Run("consent with locale and another extension", func(t *testing.T) {
		w := request(f.h, "GET", "/oauth/authorize?"+query().Encode(), "", "", "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Запрошенный доступ") || len(w.Result().Cookies()) != 1 {
			t.Fatalf("unsupported authorization metadata prevented consent: status %d, body %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "ignored-extension-marker") {
			t.Fatal("unsupported authorization metadata was rendered")
		}
	})
	for _, key := range []string{"client_id", "redirect_uri", "response_type", "state", "code_challenge", "code_challenge_method", "resource", "scope"} {
		t.Run("duplicate "+key, func(t *testing.T) {
			q := query()
			q.Add(key, "synthetic-override")
			w := request(f.h, "GET", "/oauth/authorize?"+q.Encode(), "", "", "")
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"error":"invalid_request"`) || len(w.Result().Cookies()) != 0 {
				t.Fatal("duplicate recognized authorization parameter accepted")
			}
		})
	}
	for _, tc := range []struct{ key, value, code string }{
		{"client_id", "client_unknown", "invalid_client"},
		{"redirect_uri", "https://foreign.example.invalid/callback", "invalid_redirect_uri"},
		{"response_type", "token", "invalid_request"},
		{"state", "invalid\x00state", "invalid_request"},
		{"code_challenge", "invalid", "invalid_request"},
		{"code_challenge_method", "plain", "invalid_request"},
		{"resource", "https://foreign.example.invalid/mcp", "invalid_target"},
		{"scope", "unsupported", "invalid_scope"},
	} {
		t.Run("invalid "+tc.key, func(t *testing.T) {
			q := query()
			q.Set(tc.key, tc.value)
			w := request(f.h, "GET", "/oauth/authorize?"+q.Encode(), "", "", "")
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"error":"`+tc.code+`"`) || len(w.Result().Cookies()) != 0 {
				t.Fatalf("invalid recognized authorization parameter response: status %d, body %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestStrictRegistrationLoginAndTokenHTTP(t *testing.T) {
	f := appFixture(t)
	for _, body := range []string{`{"redirect_uris":["http://public.example.invalid/cb"]}`, `{"redirect_uris":["` + testRedirect + `"],"redirect_uris":["` + testRedirect + `"]}`, `{"redirect_uris":["` + testRedirect + `"],"client_name":123}`} {
		if w := request(f.h, "POST", "/oauth/register", body, "application/json", ""); w.Code != 400 {
			t.Fatal("unsafe registration accepted")
		}
	}
	_, v, cookie := begin(t, f, oauth.ReadScope)
	v.Set("privateAccountDir", "/caller-selected")
	if w := login(f, v, cookie, testOrigin); w.Code != 400 {
		t.Fatal("unknown login field accepted")
	}
	_, v, cookie = begin(t, f, oauth.ReadScope)
	r := httptest.NewRequest("POST", testOrigin+"/oauth/authorize", strings.NewReader(v.Encode()))
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(cookie)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("duplicate flow cookie accepted")
	}
	c := connect(t, f, oauth.ReadScope)
	rv := url.Values{"grant_type": {"refresh_token"}, "client_id": {c.client}, "refresh_token": {c.tokens.RefreshToken}, "resource": {testOrigin + "/mcp"}}
	w = request(f.h, "POST", "/oauth/token", rv.Encode()+"&client_id=duplicate", "application/x-www-form-urlencoded", "")
	if w.Code != 400 {
		t.Fatal("duplicated token parameter accepted")
	}
	rv = url.Values{"client_id": {c.client}, "token": {c.tokens.RefreshToken}}
	w = request(f.h, "POST", "/oauth/revoke", rv.Encode(), "application/x-www-form-urlencoded", "")
	if w.Code != 200 {
		t.Fatal("revoke failed")
	}
	w = call(f.h, c.tokens.AccessToken, "home_devices", `{}`)
	if w.Code != 401 {
		t.Fatal("revoked HTTP access remained valid")
	}
}
func TestMCPDiscoveryInitializationAndOptionalArguments(t *testing.T) {
	f := appFixture(t)
	c := connect(t, f, oauth.ReadScope)
	for _, body := range []string{`{"jsonrpc":"2.0","id":"discover","method":"server/discover"}`, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{}}}`, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"home_devices"}}`} {
		w := request(f.h, "POST", "/mcp", body, "application/json", c.tokens.AccessToken)
		if w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) {
			t.Fatal("standard MCP metadata or optional arguments rejected")
		}
	}
	w := call(f.h, c.tokens.AccessToken, "home_devices", `null`)
	if !strings.Contains(w.Body.String(), "-32602") {
		t.Fatal("null arguments accepted")
	}
}

func TestProviderPanicIsContainedWithoutSecretOutput(t *testing.T) {
	f := appFixture(t)
	_, v, cookie := begin(t, f, oauth.ReadScope)
	v.Set("password", "panic-marker")
	w := login(f, v, cookie, testOrigin)
	if w.Code != 400 || strings.Contains(w.Body.String(), "panic-marker") || strings.Contains(w.Body.String(), "raw-vendor") {
		t.Fatal("provider panic escaped privacy boundary")
	}
	entries, e := os.ReadDir(filepath.Join(f.dir, "accounts"))
	if e != nil || len(entries) != 0 {
		t.Fatal("panic left unlinked account")
	}
}
