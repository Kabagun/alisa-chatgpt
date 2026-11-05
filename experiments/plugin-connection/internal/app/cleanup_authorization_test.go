package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smart-home-plugin/internal/oauth"
	"smart-home-plugin/internal/provider"
)

func enableCleanup(t *testing.T, f *testApp, client, redirect string, expires time.Time, autoActivate ...bool) {
	t.Helper()
	if err := f.h.Close(); err != nil {
		t.Fatal(err)
	}
	c := &CleanupAuthorization{ClientID: client, RedirectURI: redirect, ExpiresAt: expires}
	if len(autoActivate) == 1 {
		c.AutoActivate = autoActivate[0]
	}
	h, err := New(Config{PublicOrigin: testOrigin, DataDir: f.dir, Providers: []provider.Provider{f.p}, Clock: func() time.Time { return time.Unix(f.seconds.Load(), 0) }, AccessTTL: time.Minute, CleanupAuthorization: c})
	if err != nil {
		t.Fatal(err)
	}
	f.h = h
	t.Cleanup(func() { h.Close() })
}

func TestCleanupAuthorizationCompletesOAuthWithoutProviderAccess(t *testing.T) {
	testCleanupOAuthWithoutProviderAccess(t, false)
}

func TestCleanupProofFallbackCompletesOAuthWithoutProviderAccess(t *testing.T) {
	testCleanupOAuthWithoutProviderAccess(t, true)
}

func testCleanupOAuthWithoutProviderAccess(t *testing.T, fallback bool) {
	t.Helper()
	f := appFixture(t)
	client, form, cookie := begin(t, f, oauth.ReadScope+" "+oauth.ControlScope)
	enableCleanup(t, f, client, testRedirect, f.h.now().Add(time.Hour))
	if fallback {
		form, cookie = cleanupProofForm(t, f, client)
	}
	form.Set("username", "arbitrary-test-user")
	form.Set("password", "reject-marker") // The ordinary provider rejects this.
	cookies := []*http.Cookie{cookie}
	if fallback {
		cookies = nil
	}
	w := loginCookies(f, form, cookies, testOrigin)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("cleanup authorization status %d", w.Code)
	}
	callback, err := url.Parse(w.Header().Get("Location"))
	if err != nil || callback.Scheme != "https" || callback.Host != "client.example.invalid" || callback.Query().Get("state") != "opaque-client-state" {
		t.Fatal("cleanup callback lost the registered redirect or state")
	}
	if fallback && loginCookies(f, form, nil, testOrigin).Code != http.StatusForbidden {
		t.Fatal("cleanup form proof was replayable")
	}
	assertCleanupOAuthGrant(t, f, client, callback)
}

func assertCleanupOAuthGrant(t *testing.T, f *testApp, client string, callback *url.URL) {
	t.Helper()
	grant := url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "code": {callback.Query().Get("code")}, "redirect_uri": {testRedirect}, "code_verifier": {strings.Repeat("x", 43)}, "resource": {testOrigin + "/mcp"}}
	w := request(f.h, "POST", "/oauth/token", grant.Encode(), "application/x-www-form-urlencoded", "")
	if w.Code != http.StatusBadRequest {
		t.Fatal("cleanup flow bypassed PKCE")
	}
	grant.Set("code_verifier", testVerifier)
	w = request(f.h, "POST", "/oauth/token", grant.Encode(), "application/x-www-form-urlencoded", "")
	var tokens oauth.Tokens
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &tokens) != nil || tokens.AccessToken == "" {
		t.Fatalf("cleanup token exchange status %d", w.Code)
	}
	w = request(f.h, "POST", "/oauth/token", grant.Encode(), "application/x-www-form-urlencoded", "")
	if w.Code != http.StatusBadRequest {
		t.Fatal("cleanup authorization code was replayable")
	}
	w = request(f.h, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "application/json", tokens.AccessToken)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"home_devices"`) {
		t.Fatal("cleanup connection cannot discover its existing MCP tools")
	}
	for _, tool := range []struct{ name, args string }{
		{"home_devices", `{}`},
		{"home_status", `{"device_id":"synthetic-device"}`},
		{"home_rooms", `{"device_id":"synthetic-device"}`},
		{"home_clean_rooms", `{"device_id":"synthetic-device","map_id":"synthetic-map","room_ids":["1"]}`},
	} {
		w = call(f.h, tokens.AccessToken, tool.name, tool.args)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"isError":true`) || !strings.Contains(w.Body.String(), "provider_unavailable") {
			t.Fatalf("cleanup tool %s did not fail closed", tool.name)
		}
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if f.p.logins != 0 || f.p.calls != 0 || len(f.p.accounts) != 0 {
		t.Fatal("cleanup identity reached the real provider boundary")
	}
}

func TestCleanupAuthorizationOnlyMatchesExactClientCallbackAndExpiry(t *testing.T) {
	for _, mismatch := range []string{"client", "callback", "expired", "disabled"} {
		t.Run(mismatch, func(t *testing.T) {
			f := appFixture(t)
			client, form, cookie := begin(t, f, oauth.ReadScope)
			allowedClient, allowedRedirect, expires := client, testRedirect, f.h.now().Add(time.Hour)
			switch mismatch {
			case "client":
				allowedClient = "client_" + strings.Repeat("a", 48)
			case "callback":
				allowedRedirect += "/other"
			case "expired":
				expires = f.h.now().Add(-time.Second)
			}
			if mismatch != "disabled" {
				enableCleanup(t, f, allowedClient, allowedRedirect, expires)
			}
			form.Set("password", "reject-marker")
			w := login(f, form, cookie, testOrigin)
			if w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" {
				t.Fatalf("unmatched cleanup authorization accepted: %d", w.Code)
			}
			f.p.mu.Lock()
			defer f.p.mu.Unlock()
			if f.p.logins != 1 || len(f.p.accounts) != 0 {
				t.Fatal("ordinary provider authentication was skipped or retained an invalid login")
			}
		})
	}
}

func TestCleanupAuthorizationPreservesConsentOriginCookieAndCSRF(t *testing.T) {
	for _, proof := range []string{"origin", "cookie", "csrf", "consent"} {
		t.Run(proof, func(t *testing.T) {
			f := appFixture(t)
			client, form, cookie := begin(t, f, oauth.ReadScope)
			enableCleanup(t, f, client, testRedirect, f.h.now().Add(time.Hour))
			origin := testOrigin
			want := http.StatusForbidden
			switch proof {
			case "origin":
				origin = "https://foreign.example.invalid"
			case "cookie":
				cookie = &http.Cookie{Name: cookie.Name, Value: "wrong-proof"}
			case "csrf":
				form.Set("csrf", "wrong-proof")
			case "consent":
				form.Set("confirm", "no")
				want = http.StatusBadRequest
			}
			w := login(f, form, cookie, origin)
			if w.Code != want || w.Header().Get("Location") != "" {
				t.Fatalf("cleanup %s proof was bypassed: %d", proof, w.Code)
			}
			f.p.mu.Lock()
			defer f.p.mu.Unlock()
			if f.p.logins != 0 {
				t.Fatal("invalid cleanup proof reached provider")
			}
		})
	}
}

func TestCleanupAuthorizationFormIdentifiesOnlyTheScopedTestConnection(t *testing.T) {
	f := appFixture(t)
	client, _, _ := begin(t, f, oauth.ReadScope)
	enableCleanup(t, f, client, testRedirect, f.h.now().Add(time.Hour))
	q := url.Values{"client_id": {client}, "redirect_uri": {testRedirect}, "response_type": {"code"}, "state": {"synthetic"}, "code_challenge": {oauth.Challenge(testVerifier)}, "code_challenge_method": {"S256"}, "resource": {testOrigin + "/mcp"}, "scope": {oauth.ReadScope}}
	w := request(f.h, "GET", "/oauth/authorize?"+q.Encode(), "", "", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Тестовое подключение для удаления плагина") || !strings.Contains(w.Body.String(), "autocomplete=off") || strings.Contains(w.Body.String(), "autocomplete=current-password") {
		t.Fatal("scoped cleanup login did not clearly request synthetic credentials")
	}
	if !strings.Contains(w.Body.String(), "name=cleanup_proof") || !strings.Contains(w.Body.String(), "Тестовый логин") || !strings.Contains(w.Body.String(), "Тестовое слово") || !strings.Contains(w.Body.String(), "id=password name=password type=text") || strings.Contains(w.Body.String(), "Пароль аккаунта производителя") || strings.Contains(w.Body.String(), "Логин аккаунта производителя") || strings.Contains(w.Body.String(), "type=password") {
		t.Fatal("cleanup form presented real account/password inputs or omitted its scoped proof")
	}
	f.h.cleanupAuthorization = nil
	w = request(f.h, "GET", "/oauth/authorize?"+q.Encode(), "", "", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "Тестовое подключение для удаления плагина") || !strings.Contains(w.Body.String(), "autocomplete=current-password") {
		t.Fatal("ordinary provider login presentation changed")
	}
	if strings.Contains(w.Body.String(), "name=cleanup_proof") || !strings.Contains(w.Body.String(), "id=password name=password type=password") || !strings.Contains(w.Body.String(), "Пароль аккаунта производителя") || !strings.Contains(w.Body.String(), "Логин аккаунта производителя") {
		t.Fatal("ordinary form exposed fallback proof or changed its password input")
	}
}

func TestCleanupAuthorizationConfigurationFailsClosed(t *testing.T) {
	now := time.Unix(1793404800, 0)
	for _, invalid := range []string{"client", "redirect", "query", "expiry", "too-long"} {
		t.Run(invalid, func(t *testing.T) {
			c := &CleanupAuthorization{ClientID: "client_" + strings.Repeat("b", 48), RedirectURI: testRedirect, ExpiresAt: now.Add(time.Hour)}
			switch invalid {
			case "client":
				c.ClientID = "*"
			case "redirect":
				c.RedirectURI = "http://client.example.invalid/callback"
			case "query":
				c.RedirectURI += "?other=client"
			case "expiry":
				c.ExpiresAt = time.Time{}
			case "too-long":
				c.ExpiresAt = now.Add(3 * time.Hour)
			}
			h, err := New(Config{PublicOrigin: testOrigin, DataDir: filepath.Join(t.TempDir(), "private"), Providers: []provider.Provider{&fakeProvider{accounts: map[string]string{}}}, Clock: func() time.Time { return now }, CleanupAuthorization: c})
			if err == nil {
				h.Close()
				t.Fatal("invalid cleanup configuration accepted")
			}
		})
	}
}
