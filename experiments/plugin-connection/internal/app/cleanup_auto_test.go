package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"smart-home-plugin/internal/oauth"
	"smart-home-plugin/internal/store"
)

func cleanupAutoQuery(client string) url.Values {
	return url.Values{"client_id": {client}, "redirect_uri": {testRedirect}, "response_type": {"code"}, "state": {"opaque-client-state"}, "code_challenge": {oauth.Challenge(testVerifier)}, "code_challenge_method": {"S256"}, "resource": {testOrigin + "/mcp"}, "scope": {oauth.ReadScope + " " + oauth.ControlScope}}
}

func TestCleanupAutoActivationCompletesNormalOAuthWithOnlyInertAccess(t *testing.T) {
	f := appFixture(t)
	client, _, _ := begin(t, f, oauth.ReadScope)
	enableCleanup(t, f, client, testRedirect, f.h.now().Add(time.Hour), true)
	w := request(f.h, "GET", "/oauth/authorize?"+cleanupAutoQuery(client).Encode(), "", "", "")
	if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), "<form") {
		t.Fatalf("exact cleanup did not autoactivate without credentials/form/cookie: %d", w.Code)
	}
	callback, err := url.Parse(w.Header().Get("Location"))
	if err != nil || callback.Scheme+"://"+callback.Host+callback.Path != testRedirect || callback.Query().Get("state") != "opaque-client-state" || callback.Query().Get("code") == "" {
		t.Fatal("autoactivation lost registered callback, state or code")
	}
	if err := f.h.store.Read(func(st store.State) error {
		if len(st.Flows) != 1 || len(st.Subjects) != 1 || len(st.Codes) != 1 {
			t.Fatal("autoactivation did not consume its fresh flow exactly once")
		}
		for _, sub := range st.Subjects {
			if sub.Provider != cleanupProviderID {
				t.Fatal("autoactivation created a non-cleanup subject")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Failed exchanges must preserve the code for its exact public client.
	other, err := f.h.oauth.Register("Synthetic other client", []string{testRedirect})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"client_id", "redirect_uri", "resource", "scope"} {
		grant := url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "code": {callback.Query().Get("code")}, "redirect_uri": {testRedirect}, "code_verifier": {testVerifier}, "resource": {testOrigin + "/mcp"}}
		switch field {
		case "client_id":
			grant.Set(field, other.ID)
		case "redirect_uri":
			grant.Set(field, testRedirect+"/other")
		case "resource":
			grant.Set(field, "https://other.example.invalid/mcp")
		case "scope":
			grant.Set(field, oauth.ReadScope)
		}
		w = request(f.h, "POST", "/oauth/token", grant.Encode(), "application/x-www-form-urlencoded", "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("autoactivation token lost %s binding", field)
		}
	}
	assertCleanupOAuthGrant(t, f, client, callback)
}

func TestCleanupAutoActivationNeverAppliesToUnscopedOrDefaultMode(t *testing.T) {
	for _, mismatch := range []string{"client", "callback", "expired", "disabled", "default-false"} {
		t.Run(mismatch, func(t *testing.T) {
			f := appFixture(t)
			client, _, _ := begin(t, f, oauth.ReadScope)
			allowedClient, redirect, expires := client, testRedirect, f.h.now().Add(time.Hour)
			switch mismatch {
			case "client":
				allowedClient = "client_" + strings.Repeat("a", 48)
			case "callback":
				redirect += "/other"
			case "expired":
				expires = f.h.now().Add(-time.Second)
			}
			if mismatch != "disabled" {
				enableCleanup(t, f, allowedClient, redirect, expires, mismatch != "default-false")
			}
			w := request(f.h, "GET", "/oauth/authorize?"+cleanupAutoQuery(client).Encode(), "", "", "")
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<form") || w.Header().Get("Location") != "" {
				t.Fatalf("unscoped/default request autoactivated: %d", w.Code)
			}
			assertNoCleanupAccountOrProvider(t, f)
		})
	}
}

func TestCleanupAutoActivationPreservesAuthorizationValidation(t *testing.T) {
	for _, invalid := range []string{"redirect_uri", "resource", "scope", "code_challenge", "code_challenge_method", "duplicate-client", "response_type"} {
		t.Run(invalid, func(t *testing.T) {
			f := appFixture(t)
			client, _, _ := begin(t, f, oauth.ReadScope)
			enableCleanup(t, f, client, testRedirect, f.h.now().Add(time.Hour), true)
			q := cleanupAutoQuery(client)
			if invalid == "duplicate-client" {
				q.Add("client_id", client)
			} else if invalid == "redirect_uri" {
				q.Set(invalid, testRedirect+"/unregistered")
			} else {
				q.Set(invalid, "invalid")
			}
			w := request(f.h, "GET", "/oauth/authorize?"+q.Encode(), "", "", "")
			if w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" {
				t.Fatalf("autoactivation bypassed %s validation: %d", invalid, w.Code)
			}
			assertNoCleanupAccountOrProvider(t, f)
		})
	}
}

func TestCleanupAutoActivationRemovesAccountOnCompletionError(t *testing.T) {
	f := appFixture(t)
	client, _, _ := begin(t, f, oauth.ReadScope)
	enableCleanup(t, f, client, testRedirect, f.h.now().Add(time.Hour), true)
	if err := f.h.store.Update(func(st *store.State) error {
		for i := range 256 {
			st.Codes[fmt.Sprintf("%064x", i)] = store.Code{Expires: f.h.now().Add(time.Hour).Unix()}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := request(f.h, "GET", "/oauth/authorize?"+cleanupAutoQuery(client).Encode(), "", "", "")
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Location") != "" {
		t.Fatal("autoactivation did not fail closed on code capacity")
	}
	assertNoCleanupAccountOrProvider(t, f)
}

func TestCleanupAutoActivationRespectsAccountSlots(t *testing.T) {
	f := appFixture(t)
	client, _, _ := begin(t, f, oauth.ReadScope)
	enableCleanup(t, f, client, testRedirect, f.h.now().Add(time.Hour), true)
	for range cap(f.h.authSlots) {
		f.h.authSlots <- struct{}{}
	}
	w := request(f.h, "GET", "/oauth/authorize?"+cleanupAutoQuery(client).Encode(), "", "", "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatal("autoactivation bypassed account concurrency protection")
	}
	assertNoCleanupAccountOrProvider(t, f)
}

func TestCleanupAutoActivationJSONDefaultsFalse(t *testing.T) {
	var c CleanupAuthorization
	if json.Unmarshal([]byte(`{}`), &c) != nil || c.AutoActivate {
		t.Fatal("autoactivation was enabled by default")
	}
	if json.Unmarshal([]byte(`{"auto_activate":true}`), &c) != nil || !c.AutoActivate {
		t.Fatal("explicit autoactivation option was not decoded")
	}
}
