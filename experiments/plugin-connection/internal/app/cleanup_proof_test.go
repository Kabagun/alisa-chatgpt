package app

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"smart-home-plugin/internal/oauth"
)

// Fetch after enabling cleanup so the proof must come from the scoped form.
func cleanupProofForm(t *testing.T, f *testApp, client string) (url.Values, *http.Cookie) {
	t.Helper()
	q := url.Values{"client_id": {client}, "redirect_uri": {testRedirect}, "response_type": {"code"}, "state": {"opaque-client-state"}, "code_challenge": {oauth.Challenge(testVerifier)}, "code_challenge_method": {"S256"}, "resource": {testOrigin + "/mcp"}, "scope": {oauth.ReadScope + " " + oauth.ControlScope}}
	w := request(f.h, "GET", "/oauth/authorize?"+q.Encode(), "", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("cleanup form status %d", w.Code)
	}
	v := url.Values{"username": {"synthetic-user"}, "password": {"reject-marker"}, "confirm": {"yes"}}
	for _, name := range []string{"flow", "csrf", "provider", "cleanup_proof"} {
		m := regexp.MustCompile(`name=` + name + ` value="([^"]+)"`).FindStringSubmatch(w.Body.String())
		if len(m) != 2 {
			t.Fatalf("cleanup form missing %s", name)
		}
		v.Set(name, m[1])
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || v.Get("cleanup_proof") != cookies[0].Value {
		t.Fatal("cleanup form did not carry the existing flow proof")
	}
	return v, cookies[0]
}

func assertNoCleanupAccountOrProvider(t *testing.T, f *testApp) {
	t.Helper()
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if f.p.logins != 0 || f.p.calls != 0 || len(f.p.accounts) != 0 {
		t.Fatal("invalid fallback reached provider")
	}
	entries, err := os.ReadDir(filepath.Join(f.dir, "accounts"))
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid fallback created an account")
	}
}

func TestCleanupProofFallbackRejectsInvalidProofs(t *testing.T) {
	for _, invalid := range []string{"absent", "empty", "wrong", "other-flow", "duplicate", "oversize", "body-limit", "csrf", "origin", "consent", "duplicate-cookie", "wrong-cookie", "expired-flow", "expired-cleanup"} {
		t.Run(invalid, func(t *testing.T) {
			f := appFixture(t)
			client, _, _ := begin(t, f, oauth.ReadScope)
			enableCleanup(t, f, client, testRedirect, f.h.now().Add(time.Minute))
			v, cookie := cleanupProofForm(t, f, client)
			origin, want := testOrigin, http.StatusForbidden
			var cookies []*http.Cookie
			switch invalid {
			case "absent":
				v.Del("cleanup_proof")
			case "empty":
				v.Set("cleanup_proof", "")
			case "wrong":
				v.Set("cleanup_proof", strings.Repeat("a", 64))
			case "other-flow":
				other, _ := cleanupProofForm(t, f, client)
				v.Set("cleanup_proof", other.Get("cleanup_proof"))
			case "duplicate":
				v.Add("cleanup_proof", v.Get("cleanup_proof"))
				want = http.StatusBadRequest
			case "oversize":
				v.Set("cleanup_proof", strings.Repeat("a", 4097))
				want = http.StatusBadRequest
			case "body-limit":
				v.Set("cleanup_proof", strings.Repeat("a", bodyLimit+1))
				want = http.StatusBadRequest
			case "csrf":
				v.Set("csrf", "wrong-proof")
			case "origin":
				origin = "https://foreign.example.invalid"
			case "consent":
				v.Set("confirm", "no")
				want = http.StatusBadRequest
			case "duplicate-cookie":
				cookies = []*http.Cookie{cookie, cookie}
			case "wrong-cookie":
				cookies = []*http.Cookie{{Name: cookie.Name, Value: "wrong-proof"}}
			case "expired-flow":
				f.seconds.Add(301)
			case "expired-cleanup":
				f.seconds.Add(60)
			}
			w := loginCookies(f, v, cookies, origin)
			if w.Code != want || w.Header().Get("Location") != "" {
				t.Fatalf("invalid %s status %d", invalid, w.Code)
			}
			assertNoCleanupAccountOrProvider(t, f)
		})
	}
}

func TestCleanupProofFormOmitsProofForEveryUnscopedBinding(t *testing.T) {
	for _, mismatch := range []string{"client", "callback", "expired", "disabled"} {
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
				enableCleanup(t, f, allowedClient, redirect, expires)
			}
			q := url.Values{"client_id": {client}, "redirect_uri": {testRedirect}, "response_type": {"code"}, "code_challenge": {oauth.Challenge(testVerifier)}, "code_challenge_method": {"S256"}, "resource": {testOrigin + "/mcp"}}
			w := request(f.h, "GET", "/oauth/authorize?"+q.Encode(), "", "", "")
			if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "cleanup_proof") || !strings.Contains(w.Body.String(), "id=password name=password type=password") {
				t.Fatal("unscoped form exposed cleanup proof or synthetic inputs")
			}
		})
	}
}

func TestCleanupProofFallbackRejectsUnscopedFlowsEvenWithValidCookie(t *testing.T) {
	for _, mismatch := range []string{"client", "callback", "expired", "disabled"} {
		for _, withCookie := range []bool{false, true} {
			t.Run(mismatch+map[bool]string{false: "/no-cookie", true: "/cookie"}[withCookie], func(t *testing.T) {
				f := appFixture(t)
				client, v, cookie := begin(t, f, oauth.ReadScope)
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
					enableCleanup(t, f, allowedClient, redirect, expires)
				}
				// Even knowledge of the exact cookie proof must never grant fallback
				// authority to an ordinary, cross-client or expired flow.
				v.Set("cleanup_proof", cookie.Value)
				var cookies []*http.Cookie
				if withCookie {
					cookies = []*http.Cookie{cookie}
				}
				w := loginCookies(f, v, cookies, testOrigin)
				if w.Code != http.StatusForbidden || w.Header().Get("Location") != "" {
					t.Fatalf("unscoped fallback status %d", w.Code)
				}
				assertNoCleanupAccountOrProvider(t, f)
			})
		}
	}
}

func TestCleanupProofFallbackIsAtomicSingleUse(t *testing.T) {
	f := appFixture(t)
	client, _, _ := begin(t, f, oauth.ReadScope)
	enableCleanup(t, f, client, testRedirect, f.h.now().Add(time.Hour))
	v, _ := cleanupProofForm(t, f, client)
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for range 2 {
		wg.Go(func() { statuses <- loginCookies(f, v, nil, testOrigin).Code })
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[http.StatusSeeOther] != 1 || counts[http.StatusForbidden] != 1 {
		t.Fatal("concurrent fallback did not consume the flow exactly once")
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if f.p.logins != 0 || f.p.calls != 0 {
		t.Fatal("concurrent cleanup reached provider")
	}
}
