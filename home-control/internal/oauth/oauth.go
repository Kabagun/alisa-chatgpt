// Package oauth implements public-client authorization code + PKCE, opaque tokens
// and refresh rotation. It never accepts caller-supplied subjects/account paths.
package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"smart-home-plugin/internal/store"
)

const ReadScope = "home:read"
const ControlScope = "home:control"

type Error string

func (e Error) Error() string { return string(e) }
func Code(err error) string {
	var e Error
	if errors.As(err, &e) {
		return string(e)
	}
	return "temporarily_unavailable"
}

type Service struct {
	store                 *store.Store
	origin                string
	now                   func() time.Time
	accessTTL, refreshTTL time.Duration
}
type Identity struct{ Subject, Provider, Client, Scope string }
type StartRequest struct{ Client, Redirect, State, Challenge, Method, Resource, Scope string }
type Started struct {
	ID, Cookie, CSRF string
	Flow             store.Flow
	ClientName       string
}
type TokenRequest struct{ GrantType, Client, Code, Redirect, Verifier, Refresh, Resource, Scope string }
type Tokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func CanonicalOrigin(raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.Opaque != "" || strings.ContainsAny(raw, "\r\n\x00") {
		return "", Error("invalid_request")
	}
	if u.Host != strings.ToLower(u.Host) || u.Hostname() == "" {
		return "", Error("invalid_request")
	}
	u.Path = ""
	return u.String(), nil
}
func New(st *store.Store, origin string, clock func() time.Time, accessTTL, refreshTTL time.Duration) (*Service, error) {
	origin, e := CanonicalOrigin(origin)
	if e != nil {
		return nil, e
	}
	if clock == nil {
		clock = time.Now
	}
	if accessTTL == 0 {
		accessTTL = 5 * time.Minute
	}
	if refreshTTL == 0 {
		refreshTTL = 30 * 24 * time.Hour
	}
	if accessTTL < time.Second || accessTTL > 15*time.Minute || refreshTTL < accessTTL || refreshTTL > 30*24*time.Hour {
		return nil, Error("invalid_request")
	}
	return &Service{st, origin, clock, accessTTL, refreshTTL}, nil
}
func (s *Service) Resource() string { return s.origin + "/mcp" }
func RedirectValid(raw string) bool {
	if len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\x00") {
		return false
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || !strings.HasPrefix(u.Path, "/") {
		return false
	}
	if u.Scheme == "https" {
		return u.Host == strings.ToLower(u.Host)
	}
	if u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	ip := net.ParseIP(h)
	return h == "localhost" || (ip != nil && ip.IsLoopback())
}
func Scope(raw string) (string, error) {
	if raw == "" {
		return ReadScope, nil
	}
	seen := map[string]bool{}
	for _, v := range strings.Split(raw, " ") {
		if (v != ReadScope && v != ControlScope) || seen[v] {
			return "", Error("invalid_scope")
		}
		seen[v] = true
	}
	var values []string
	for _, v := range []string{ReadScope, ControlScope} {
		if seen[v] {
			values = append(values, v)
		}
	}
	return strings.Join(values, " "), nil
}
func HasScope(scope, want string) bool {
	for _, v := range strings.Split(scope, " ") {
		if v == want {
			return true
		}
	}
	return false
}
func subset(want, have string) bool {
	for _, v := range strings.Split(want, " ") {
		if !HasScope(have, v) {
			return false
		}
	}
	return true
}
func prune(st *store.State, now int64) {
	for k, v := range st.Clients {
		if v.Expires <= now {
			delete(st.Clients, k)
		}
	}
	for k, v := range st.Flows {
		if v.Expires <= now {
			delete(st.Flows, k)
		}
	}
	for k, v := range st.Codes {
		if v.Expires <= now {
			delete(st.Codes, k)
		}
	}
	for k, v := range st.Access {
		if v.Expires <= now {
			delete(st.Access, k)
		}
	}
	for k, v := range st.Refresh {
		if v.Expires <= now {
			delete(st.Refresh, k)
		}
	}
	for k, v := range st.Subjects {
		if v.RetainUntil > 0 && v.RetainUntil+60 <= now {
			delete(st.Subjects, k)
		}
	}
}

// Sweep provides bounded, opportunistic expiry/cleanup on request traffic.
func (s *Service) Sweep() error {
	return s.store.Update(func(st *store.State) error { prune(st, s.now().Unix()); return nil })
}
func (s *Service) Register(name string, redirects []string) (store.Client, error) {
	if len(name) > 100 || strings.ContainsAny(name, "\r\n\x00") || len(redirects) == 0 || len(redirects) > 8 {
		return store.Client{}, Error("invalid_client_metadata")
	}
	seen := map[string]bool{}
	for _, r := range redirects {
		if !RedirectValid(r) || seen[r] {
			return store.Client{}, Error("invalid_redirect_uri")
		}
		seen[r] = true
	}
	if name == "" {
		name = "MCP client"
	}
	c := store.Client{ID: "client_" + store.Random(24), Name: name, Redirects: append([]string(nil), redirects...), Expires: s.now().Add(30 * 24 * time.Hour).Unix()}
	if len(c.ID) != 55 {
		return store.Client{}, Error("temporarily_unavailable")
	}
	err := s.store.Update(func(st *store.State) error {
		prune(st, s.now().Unix())
		if len(st.Clients) >= 128 {
			return Error("temporarily_unavailable")
		}
		st.Clients[c.ID] = c
		return nil
	})
	return c, err
}
func challengeValid(c string) bool {
	b, e := base64.RawURLEncoding.DecodeString(c)
	return e == nil && len(c) == 43 && len(b) == 32
}
func (s *Service) Start(r StartRequest) (Started, error) {
	if r.Resource != s.Resource() {
		return Started{}, Error("invalid_target")
	}
	scope, e := Scope(r.Scope)
	if e != nil {
		return Started{}, e
	}
	if r.Method != "S256" || !challengeValid(r.Challenge) || len(r.State) > 1024 || strings.ContainsAny(r.State, "\x00\r\n") {
		return Started{}, Error("invalid_request")
	}
	x := Started{ID: store.Random(24), Cookie: store.Random(32), CSRF: store.Random(32)}
	if len(x.ID) != 48 || len(x.Cookie) != 64 || len(x.CSRF) != 64 {
		return Started{}, Error("temporarily_unavailable")
	}
	x.Flow = store.Flow{Client: r.Client, Redirect: r.Redirect, State: r.State, Challenge: r.Challenge, Resource: r.Resource, Scope: scope, CookieHash: store.Hash(x.Cookie), CSRFHash: store.Hash(x.CSRF), Expires: s.now().Add(5 * time.Minute).Unix()}
	err := s.store.Update(func(st *store.State) error {
		prune(st, s.now().Unix())
		c, ok := st.Clients[r.Client]
		if !ok {
			return Error("invalid_client")
		}
		found := false
		for _, v := range c.Redirects {
			if v == r.Redirect {
				found = true
			}
		}
		if !found {
			return Error("invalid_redirect_uri")
		}
		if len(st.Flows) >= 128 {
			return Error("temporarily_unavailable")
		}
		x.ClientName = c.Name
		st.Flows[x.ID] = x.Flow
		return nil
	})
	return x, err
}
func equalHash(raw, hash string) bool {
	return len(raw) <= 256 && subtle.ConstantTimeCompare([]byte(store.Hash(raw)), []byte(hash)) == 1
}
func (s *Service) TakeFlow(id, cookie, csrf string) (store.Flow, error) {
	var f store.Flow
	err := s.store.Update(func(st *store.State) error {
		prune(st, s.now().Unix())
		v, ok := st.Flows[id]
		if !ok || !equalHash(cookie, v.CookieHash) || !equalHash(csrf, v.CSRFHash) {
			return Error("access_denied")
		}
		if _, ok := st.Clients[v.Client]; !ok {
			return Error("invalid_client")
		}
		f = v
		delete(st.Flows, id)
		return nil
	})
	return f, err
}
func (s *Service) Complete(f store.Flow, subject, provider string) (string, error) {
	if !store.UUID(subject) || provider == "" || f.Expires <= s.now().Unix() {
		return "", Error("access_denied")
	}
	code := store.Random(32)
	if len(code) != 64 {
		return "", Error("temporarily_unavailable")
	}
	err := s.store.Update(func(st *store.State) error {
		prune(st, s.now().Unix())
		if len(st.Subjects) >= 256 || len(st.Codes) >= 256 {
			return Error("temporarily_unavailable")
		}
		if _, ok := st.Subjects[subject]; ok {
			return Error("access_denied")
		}
		if _, ok := st.Clients[f.Client]; !ok {
			return Error("invalid_client")
		}
		st.Subjects[subject] = store.Subject{ID: subject, Provider: provider, Created: s.now().Unix(), RetainUntil: s.now().Add(time.Minute).Unix()}
		st.Codes[store.Hash(code)] = store.Code{Client: f.Client, Redirect: f.Redirect, Challenge: f.Challenge, Resource: f.Resource, Scope: f.Scope, Subject: subject, Expires: s.now().Add(time.Minute).Unix()}
		return nil
	})
	return code, err
}
func verifierValid(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", c)) {
			return false
		}
	}
	return true
}
func Challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
func (s *Service) Exchange(r TokenRequest) (Tokens, error) {
	if r.Resource != s.Resource() {
		return Tokens{}, Error("invalid_target")
	}
	a, refresh := store.Random(32), store.Random(32)
	if len(a) != 64 || len(refresh) != 64 {
		return Tokens{}, Error("temporarily_unavailable")
	}
	var granted string
	err := s.store.Update(func(st *store.State) error {
		now := s.now().Unix()
		prune(st, now)
		if _, ok := st.Clients[r.Client]; !ok {
			return Error("invalid_client")
		}
		var token store.Token
		switch r.GrantType {
		case "authorization_code":
			c, ok := st.Codes[store.Hash(r.Code)]
			if !ok || c.Client != r.Client || c.Redirect != r.Redirect || c.Resource != r.Resource || !verifierValid(r.Verifier) || Challenge(r.Verifier) != c.Challenge {
				return Error("invalid_grant")
			}
			if r.Scope != "" {
				scope, e := Scope(r.Scope)
				if e != nil || scope != c.Scope {
					return Error("invalid_scope")
				}
			}
			token = store.Token{Client: c.Client, Resource: c.Resource, Scope: c.Scope, Subject: c.Subject}
			delete(st.Codes, store.Hash(r.Code))
		case "refresh_token":
			t, ok := st.Refresh[store.Hash(r.Refresh)]
			if !ok || t.Used || t.Client != r.Client || t.Resource != r.Resource {
				return Error("invalid_grant")
			}
			token = t
			if r.Scope != "" {
				scope, e := Scope(r.Scope)
				if e != nil || !subset(scope, t.Scope) {
					return Error("invalid_scope")
				}
				token.Scope = scope
			}
			t.Used = true
			st.Refresh[store.Hash(r.Refresh)] = t
		default:
			return Error("unsupported_grant_type")
		}
		subject, ok := st.Subjects[token.Subject]
		if !ok || subject.Revoked {
			return Error("invalid_grant")
		}
		if len(st.Access) >= 1024 || len(st.Refresh) >= 1024 {
			return Error("temporarily_unavailable")
		}
		subject.RetainUntil = now + int64(s.refreshTTL/time.Second)
		st.Subjects[token.Subject] = subject
		granted = token.Scope
		client := st.Clients[r.Client]
		client.Expires = now + int64(s.refreshTTL/time.Second)
		st.Clients[r.Client] = client
		token.Used = false
		token.Expires = now + int64(s.accessTTL/time.Second)
		st.Access[store.Hash(a)] = token
		token.Expires = now + int64(s.refreshTTL/time.Second)
		st.Refresh[store.Hash(refresh)] = token
		return nil
	})
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{a, "Bearer", int64(s.accessTTL / time.Second), refresh, granted}, nil
}
func (s *Service) Authenticate(raw string) (Identity, error) {
	if len(raw) != 64 {
		return Identity{}, Error("invalid_token")
	}
	var result Identity
	err := s.store.Read(func(st store.State) error {
		t, ok := st.Access[store.Hash(raw)]
		if !ok || t.Expires <= s.now().Unix() || t.Resource != s.Resource() {
			return Error("invalid_token")
		}
		sub, ok := st.Subjects[t.Subject]
		if !ok || sub.Revoked {
			return Error("invalid_token")
		}
		result = Identity{sub.ID, sub.Provider, t.Client, t.Scope}
		return nil
	})
	return result, err
}
func (s *Service) Revoke(raw, client string) error {
	return s.store.Update(func(st *store.State) error {
		prune(st, s.now().Unix())
		hash := store.Hash(raw)
		if t, ok := st.Refresh[hash]; ok && t.Client == client {
			for k, v := range st.Refresh {
				if v.Subject == t.Subject {
					delete(st.Refresh, k)
				}
			}
			for k, v := range st.Access {
				if v.Subject == t.Subject {
					delete(st.Access, k)
				}
			}
			sub := st.Subjects[t.Subject]
			sub.Revoked = true
			sub.RetainUntil = s.now().Unix()
			st.Subjects[t.Subject] = sub
		}
		if t, ok := st.Access[hash]; ok && t.Client == client {
			delete(st.Access, hash)
		}
		return nil
	})
}
