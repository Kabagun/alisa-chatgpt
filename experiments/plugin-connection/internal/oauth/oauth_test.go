package oauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smart-home-plugin/internal/store"
)

const origin = "https://bridge.example.invalid"
const redirect = "https://client.example.invalid/callback"
const verifier = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGH"

type fixture struct {
	o   *Service
	s   *store.Store
	now time.Time
	dir string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{now: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), dir: filepath.Join(t.TempDir(), "private")}
	s, e := store.Open(f.dir)
	if e != nil {
		t.Fatal(e)
	}
	f.s = s
	t.Cleanup(func() { s.Close() })
	o, e := New(s, origin, func() time.Time { return f.now }, time.Minute, 0)
	if e != nil {
		t.Fatal(e)
	}
	f.o = o
	return f
}
func (f *fixture) client(t *testing.T) store.Client {
	t.Helper()
	c, e := f.o.Register("synthetic client", []string{redirect})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func (f *fixture) grant(t *testing.T, c store.Client, scope string) (string, string) {
	t.Helper()
	x, e := f.o.Start(StartRequest{Client: c.ID, Redirect: redirect, Challenge: Challenge(verifier), Method: "S256", Resource: f.o.Resource(), Scope: scope, State: "preserved-state"})
	if e != nil {
		t.Fatal(e)
	}
	flow, e := f.o.TakeFlow(x.ID, x.Cookie, x.CSRF)
	if e != nil {
		t.Fatal(e)
	}
	id, _, e := f.s.NewAccount()
	if e != nil {
		t.Fatal(e)
	}
	code, e := f.o.Complete(flow, id, "test")
	if e != nil {
		t.Fatal(e)
	}
	return code, id
}
func codeRequest(c store.Client, code string) TokenRequest {
	return TokenRequest{GrantType: "authorization_code", Client: c.ID, Code: code, Redirect: redirect, Verifier: verifier, Resource: origin + "/mcp"}
}
func TestPKCEClientRedirectResourceScopeBindingAndReuse(t *testing.T) {
	for _, kind := range []string{"verifier", "client", "redirect", "resource", "scope", "expired", "valid"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			c := f.client(t)
			code, _ := f.grant(t, c, ReadScope)
			r := codeRequest(c, code)
			switch kind {
			case "verifier":
				r.Verifier = strings.Repeat("x", 43)
			case "client":
				r.Client = f.client(t).ID
			case "redirect":
				r.Redirect = "https://other.example.invalid/cb"
			case "resource":
				r.Resource = origin + "/foreign"
			case "scope":
				r.Scope = ControlScope
			case "expired":
				f.now = f.now.Add(61 * time.Second)
			}
			tokens, e := f.o.Exchange(r)
			if kind != "valid" {
				if e == nil {
					t.Fatal("invalid bound grant accepted")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = f.o.Authenticate(tokens.AccessToken); e != nil {
				t.Fatal(e)
			}
			if _, e = f.o.Exchange(r); e == nil {
				t.Fatal("authorization code reused")
			}
		})
	}
}
func TestCSRFAndFlowExpiry(t *testing.T) {
	f := setup(t)
	c := f.client(t)
	x, e := f.o.Start(StartRequest{Client: c.ID, Redirect: redirect, Challenge: Challenge(verifier), Method: "S256", Resource: f.o.Resource()})
	if e != nil {
		t.Fatal(e)
	}
	for _, pair := range [][2]string{{"wrong", x.CSRF}, {x.Cookie, "wrong"}} {
		if _, e := f.o.TakeFlow(x.ID, pair[0], pair[1]); e == nil {
			t.Fatal("unbound CSRF accepted")
		}
	}
	if _, e := f.o.TakeFlow(x.ID, x.Cookie, x.CSRF); e != nil {
		t.Fatal(e)
	}
	if _, e := f.o.TakeFlow(x.ID, x.Cookie, x.CSRF); e == nil {
		t.Fatal("consent flow reused")
	}
	x, e = f.o.Start(StartRequest{Client: c.ID, Redirect: redirect, Challenge: Challenge(verifier), Method: "S256", Resource: f.o.Resource()})
	if e != nil {
		t.Fatal(e)
	}
	f.now = f.now.Add(6 * time.Minute)
	if _, e = f.o.TakeFlow(x.ID, x.Cookie, x.CSRF); e == nil {
		t.Fatal("expired consent accepted")
	}
}
func TestRefreshRotationExpiryAndRevocation(t *testing.T) {
	f := setup(t)
	c := f.client(t)
	code, subject := f.grant(t, c, ReadScope+" "+ControlScope)
	first, e := f.o.Exchange(codeRequest(c, code))
	if e != nil {
		t.Fatal(e)
	}
	r := TokenRequest{GrantType: "refresh_token", Client: c.ID, Refresh: first.RefreshToken, Resource: f.o.Resource()}
	second, e := f.o.Exchange(r)
	if e != nil || second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh did not rotate")
	}
	id, e := f.o.Authenticate(second.AccessToken)
	if e != nil || id.Subject != subject {
		t.Fatal("refresh changed account binding")
	}
	if _, e = f.o.Exchange(r); e == nil {
		t.Fatal("consumed refresh accepted")
	}
	f.now = f.now.Add(61 * time.Second)
	if _, e = f.o.Authenticate(first.AccessToken); e == nil {
		t.Fatal("expired access accepted")
	}
	r.Refresh = second.RefreshToken
	third, e := f.o.Exchange(r)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.o.Revoke(third.RefreshToken, c.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = f.o.Authenticate(third.AccessToken); e == nil {
		t.Fatal("revoked family remained usable")
	}
	r.Refresh = third.RefreshToken
	if _, e = f.o.Exchange(r); e == nil {
		t.Fatal("revoked refresh accepted")
	}
}
func TestTokensPersistOnlyHashesAndRestart(t *testing.T) {
	f := setup(t)
	c := f.client(t)
	code, _ := f.grant(t, c, ReadScope)
	tokens, e := f.o.Exchange(codeRequest(c, code))
	if e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(f.dir, "oauth-state.json"))
	if e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{code, tokens.AccessToken, tokens.RefreshToken} {
		if strings.Contains(string(data), raw) {
			t.Fatal("plaintext OAuth secret persisted")
		}
	}
	second, e := store.Open(f.dir)
	if e != nil {
		t.Fatal(e)
	}
	defer second.Close()
	o, e := New(second, origin, func() time.Time { return f.now }, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = o.Authenticate(tokens.AccessToken); e != nil {
		t.Fatal("persisted token hash lost")
	}
}
func TestCanonicalURLsAndScopes(t *testing.T) {
	for _, bad := range []string{"http://bridge.example.invalid", "https://user:secret@bridge.example.invalid", "https://bridge.example.invalid/path", "https://bridge.example.invalid?query=1", "https://bridge.example.invalid/#fragment"} {
		if _, e := CanonicalOrigin(bad); e == nil {
			t.Fatal("unsafe public origin accepted")
		}
	}
	for _, bad := range []string{"http://public.example.invalid/cb", "https://user:secret@client.example.invalid/cb", "https://client.example.invalid/cb#fragment", "javascript:alert(1)"} {
		if RedirectValid(bad) {
			t.Fatal("unsafe redirect accepted")
		}
	}
	if !RedirectValid("http://127.0.0.1:12345/cb") || !RedirectValid(redirect) {
		t.Fatal("valid callback rejected")
	}
	for _, bad := range []string{"home:admin", "home:read home:read", "home:read  home:control"} {
		if _, e := Scope(bad); e == nil {
			t.Fatal("invalid scope accepted")
		}
	}
}

func TestExpiredAndRevokedAccountRetention(t *testing.T) {
	f := setup(t)
	c := f.client(t)
	code, subject := f.grant(t, c, ReadScope)
	tokens, e := f.o.Exchange(codeRequest(c, code))
	if e != nil {
		t.Fatal(e)
	}
	dir, e := f.s.AccountDir(subject)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.o.Revoke(tokens.RefreshToken, c.ID); e != nil {
		t.Fatal(e)
	}
	f.now = f.now.Add(61 * time.Second)
	if e = f.o.Sweep(); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Lstat(dir); !os.IsNotExist(e) {
		t.Fatal("revoked unlinked private account retained after cleanup grace")
	}
}

func TestRefreshCannotChangeClientResourceOrEscalateScope(t *testing.T) {
	for _, kind := range []string{"client", "resource", "scope"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			c := f.client(t)
			code, _ := f.grant(t, c, ReadScope)
			tokens, e := f.o.Exchange(codeRequest(c, code))
			if e != nil {
				t.Fatal(e)
			}
			r := TokenRequest{GrantType: "refresh_token", Client: c.ID, Refresh: tokens.RefreshToken, Resource: f.o.Resource()}
			bad := r
			switch kind {
			case "client":
				bad.Client = f.client(t).ID
			case "resource":
				bad.Resource = origin + "/other"
			case "scope":
				bad.Scope = ReadScope + " " + ControlScope
			}
			if _, e = f.o.Exchange(bad); e == nil {
				t.Fatal("refresh binding or scope bypassed")
			}
			if _, e = f.o.Exchange(r); e != nil {
				t.Fatal("rejected request consumed valid refresh")
			}
		})
	}
}
