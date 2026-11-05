// Package app hosts the authenticated smart-home MCP resource and OAuth UI.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"smart-home-plugin/internal/oauth"
	"smart-home-plugin/internal/provider"
	"smart-home-plugin/internal/store"
)

const bodyLimit = 16 << 10
const flowCookiePrefix = "__Host-home_flow_"

type Config struct {
	PublicOrigin, DataDir string
	Providers             []provider.Provider
	Clock                 func() time.Time
	CleanupAuthorization  *CleanupAuthorization
	// OnAuthDenial receives only fixed gate codes, never request or account data.
	// It may be called concurrently and must return promptly.
	OnAuthDenial                                         func(string)
	AccessTTL, RefreshTTL, AuthTimeout, OperationTimeout time.Duration
}
type Handler struct {
	origin                        string
	store                         *store.Store
	oauth                         *oauth.Service
	providers                     map[string]provider.Provider
	labels                        []providerOption
	now                           func() time.Time
	onAuthDenial                  func(string)
	authTimeout, operationTimeout time.Duration
	mu                            sync.Mutex
	rates                         map[string]rate
	operations                    map[string]chan struct{}
	authSlots                     chan struct{}
	lastSweep                     time.Time
	cleanupAuthorization          *CleanupAuthorization
}
type rate struct {
	start time.Time
	count int
}
type providerOption struct{ ID, Label string }

var providerID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
var flowID = regexp.MustCompile(`^[a-f0-9]{48}$`)

func New(c Config) (*Handler, error) {
	origin, e := oauth.CanonicalOrigin(c.PublicOrigin)
	if e != nil {
		return nil, errors.New("invalid_public_origin")
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	cleanupAuthorization, e := prepareCleanupAuthorization(c.CleanupAuthorization, c.Clock())
	if e != nil {
		return nil, e
	}
	if c.AuthTimeout == 0 {
		c.AuthTimeout = 45 * time.Second
	}
	if c.OperationTimeout == 0 {
		c.OperationTimeout = 20 * time.Second
	}
	if c.AuthTimeout < time.Second || c.AuthTimeout > time.Minute || c.OperationTimeout < time.Second || c.OperationTimeout > time.Minute {
		return nil, errors.New("invalid_timeout")
	}
	p := map[string]provider.Provider{}
	var labels []providerOption
	for _, v := range c.Providers {
		if v == nil || !providerID.MatchString(v.ID()) || v.ID() == cleanupProviderID || v.Label() == "" || len(v.Label()) > 100 || p[v.ID()] != nil {
			return nil, errors.New("invalid_provider_registry")
		}
		p[v.ID()] = v
		labels = append(labels, providerOption{v.ID(), v.Label()})
	}
	st, e := store.Open(c.DataDir)
	if e != nil {
		return nil, e
	}
	o, e := oauth.New(st, origin, c.Clock, c.AccessTTL, c.RefreshTTL)
	if e != nil {
		st.Close()
		return nil, e
	}
	return &Handler{origin: origin, store: st, oauth: o, providers: p, labels: labels, now: c.Clock, onAuthDenial: c.OnAuthDenial, authTimeout: c.AuthTimeout, operationTimeout: c.OperationTimeout, rates: map[string]rate{}, operations: map[string]chan struct{}{}, authSlots: make(chan struct{}, 2), cleanupAuthorization: cleanupAuthorization}, nil
}
func (h *Handler) Close() error { return h.store.Close() }
func (h *Handler) limited(key string, max int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.rates[key]
	now := h.now()
	if v.start.IsZero() || now.Sub(v.start) >= time.Minute {
		v = rate{start: now}
	}
	v.count++
	h.rates[key] = v
	return v.count > max
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	h.mu.Lock()
	sweep := h.lastSweep.IsZero() || h.now().Sub(h.lastSweep) >= time.Minute
	if sweep {
		h.lastSweep = h.now()
	}
	h.mu.Unlock()
	if sweep {
		if h.oauth.Sweep() != nil {
			h.failure(w, 503, "temporarily_unavailable")
			return
		}
		live := map[string]bool{}
		_ = h.store.Read(func(st store.State) error {
			for id := range st.Subjects {
				live[id] = true
			}
			return nil
		})
		h.mu.Lock()
		for id, gate := range h.operations {
			if !live[id] && len(gate) == 0 {
				delete(h.operations, id)
			}
		}
		h.mu.Unlock()
	}
	if len(r.URL.RawQuery) > 4096 || len(r.Header.Get("Authorization")) > 4096 {
		h.failure(w, 400, "invalid_request")
		return
	}
	switch r.URL.Path {
	case "/":
		if r.Method != "GET" {
			h.method(w, "GET")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, homePage)
	case "/health":
		if r.Method != "GET" {
			h.method(w, "GET")
			return
		}
		h.json(w, 200, map[string]any{"status": "ready", "authentication": "oauth_required", "providers_configured": len(h.providers) > 0})
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp":
		if r.Method != "GET" {
			h.method(w, "GET")
			return
		}
		h.json(w, 200, map[string]any{"resource": h.oauth.Resource(), "authorization_servers": []string{h.origin}, "scopes_supported": []string{oauth.ReadScope, oauth.ControlScope}, "bearer_methods_supported": []string{"header"}})
	case "/.well-known/oauth-authorization-server":
		if r.Method != "GET" {
			h.method(w, "GET")
			return
		}
		h.json(w, 200, map[string]any{"issuer": h.origin, "authorization_endpoint": h.origin + "/oauth/authorize", "token_endpoint": h.origin + "/oauth/token", "registration_endpoint": h.origin + "/oauth/register", "revocation_endpoint": h.origin + "/oauth/revoke", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{oauth.ReadScope, oauth.ControlScope}})
	case "/oauth/register":
		h.register(w, r)
	case "/oauth/authorize":
		h.authorize(w, r)
	case "/oauth/token":
		h.token(w, r)
	case "/oauth/revoke":
		h.revoke(w, r)
	case "/mcp":
		h.mcp(w, r)
	default:
		h.failure(w, 404, "not_found")
	}
}
func (h *Handler) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (h *Handler) failure(w http.ResponseWriter, status int, code string) {
	h.json(w, status, map[string]string{"error": code})
}
func (h *Handler) method(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	h.failure(w, 405, "method_not_allowed")
}
func contentType(r *http.Request, want string) bool {
	v, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return e == nil && v == want
}
func readBody(r *http.Request) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r.Body, bodyLimit+1))
	if e != nil || len(b) > bodyLimit {
		return nil, errors.New("invalid_request")
	}
	return b, nil
}

// Check duplicate object keys and nesting before decoding known fields.
func jsonValue(d *json.Decoder, depth int) error {
	if depth > 12 {
		return errors.New("depth")
	}
	v, e := d.Token()
	if e != nil {
		return e
	}
	delim, ok := v.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return errors.New("duplicate")
			}
			seen[s] = true
			if e = jsonValue(d, depth+1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return errors.New("object")
		}
	case '[':
		for d.More() {
			if e := jsonValue(d, depth+1); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil || end != json.Delim(']') {
			return errors.New("array")
		}
	default:
		return errors.New("delimiter")
	}
	return nil
}
func decodeJSON(b []byte, dst any, allowUnknownFields bool) error {
	d := json.NewDecoder(strings.NewReader(string(b)))
	if e := jsonValue(d, 0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("extra JSON")
	}
	d = json.NewDecoder(strings.NewReader(string(b)))
	if !allowUnknownFields {
		d.DisallowUnknownFields()
	}
	return d.Decode(dst)
}
func strictJSON(b []byte, dst any) error { return decodeJSON(b, dst, false) }
func form(r *http.Request, keys ...string) (url.Values, error) {
	if r.URL.RawQuery != "" || !contentType(r, "application/x-www-form-urlencoded") {
		return nil, errors.New("invalid_request")
	}
	b, e := readBody(r)
	if e != nil {
		return nil, e
	}
	v, e := url.ParseQuery(string(b))
	if e != nil {
		return nil, e
	}
	return fields(v, keys...)
}
func fields(v url.Values, keys ...string) (url.Values, error) {
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	for k, values := range v {
		if !allowed[k] || len(values) != 1 || len(values[0]) > 4096 {
			return nil, errors.New("invalid_request")
		}
	}
	return v, nil
}
func authorizationFields(v url.Values) (url.Values, error) {
	keys := []string{"client_id", "redirect_uri", "response_type", "state", "code_challenge", "code_challenge_method", "resource", "scope"}
	known := url.Values{}
	// RFC 6749 section 3.1 requires ignoring unrecognized authorization parameters.
	// Keep the recognized values intact so duplicate and length checks still apply.
	for _, key := range keys {
		if values, ok := v[key]; ok {
			known[key] = values
		}
	}
	return fields(known, keys...)
}
func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		h.method(w, "POST")
		return
	}
	if h.limited("registration", 10) {
		h.failure(w, 429, "slow_down")
		return
	}
	if r.URL.RawQuery != "" || !contentType(r, "application/json") {
		h.failure(w, 400, "invalid_request")
		return
	}
	var in struct {
		RedirectURIs  []string `json:"redirect_uris"`
		ClientName    string   `json:"client_name"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
		AuthMethod    string   `json:"token_endpoint_auth_method"`
		Scope         string   `json:"scope"`
	}
	b, e := readBody(r)
	// RFC 7591 section 2 requires ignoring unsupported client metadata.
	// Duplicate keys, nesting, request bounds and known field types remain checked.
	if e != nil || decodeJSON(b, &in, true) != nil {
		h.failure(w, 400, "invalid_client_metadata")
		return
	}
	if in.AuthMethod != "" && in.AuthMethod != "none" {
		h.failure(w, 400, "invalid_client_metadata")
		return
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			h.failure(w, 400, "invalid_client_metadata")
			return
		}
	}
	for _, v := range in.ResponseTypes {
		if v != "code" {
			h.failure(w, 400, "invalid_client_metadata")
			return
		}
	}
	if _, e := oauth.Scope(in.Scope); e != nil {
		h.failure(w, 400, "invalid_scope")
		return
	}
	c, e := h.oauth.Register(in.ClientName, in.RedirectURIs)
	if e != nil {
		h.failure(w, 400, oauth.Code(e))
		return
	}
	h.json(w, 201, map[string]any{"client_id": c.ID, "client_name": c.Name, "redirect_uris": c.Redirects, "client_id_issued_at": h.now().Unix(), "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "scope": oauth.ReadScope + " " + oauth.ControlScope})
}

const homePage = `<!doctype html>
<html lang=ru>
<head>
<meta charset=utf-8>
<meta name=viewport content="width=device-width, initial-scale=1">
<title>Умный дом · Dreame</title>
</head>
<body>
<main>
<h1>Умный дом</h1>
<p>Доступ к устройствам Dreame из вашего приложения.</p>
<section aria-labelledby=features>
<h2 id=features>Что доступно сейчас</h2>
<ul>
<li>Список устройств.</li>
<li>Состояние робота-пылесоса.</li>
<li>Карта и комнаты.</li>
</ul>
<p><strong>Уборка выбранных комнат пока не включена.</strong></p>
</section>
<section aria-labelledby=connection>
<h2 id=connection>Как подключить</h2>
<ol>
<li>Добавьте этот сервис в приложении, из которого хотите обращаться к умному дому.</li>
<li>Введите логин и пароль своего аккаунта Dreame.</li>
<li>Проверьте запрошенный доступ и подтвердите подключение.</li>
</ol>
<p>Открытие этой страницы не подключает аккаунт. Доступ к устройствам появится после успешного входа и вашего подтверждения.</p>
</section>
</main>
</body>
</html>`

var loginTemplate = template.Must(template.New("login").Parse(`<!doctype html>
<html lang=ru>
<head>
<meta charset=utf-8>
<meta name=viewport content="width=device-width, initial-scale=1">
<title>Подключение умного дома</title>
</head>
<body>
<main>
<h1>Подключение умного дома</h1>
<p>Приложение <strong>{{.Client}}</strong> запрашивает доступ к вашему умному дому.</p>
<section aria-labelledby=permissions>
<h2 id=permissions>Запрошенный доступ</h2>
<ul>{{range .Permissions}}
<li>{{.}}</li>{{end}}
</ul>
</section>
{{if .Cleanup}}<p>Тестовое подключение для удаления плагина. Введите любые тестовые данные. Вход в Dreamehome не выполняется.</p>{{else}}<p>Подключение создаётся после успешного входа в аккаунт производителя. Пароль используется только для этого входа и не сохраняется.</p>{{end}}
<form method=post action="{{.Action}}">
<input type=hidden name=flow value="{{.Flow}}">
<input type=hidden name=csrf value="{{.CSRF}}">
{{if .CleanupProof}}<input type=hidden name=cleanup_proof value="{{.CleanupProof}}">{{end}}
<fieldset>
<legend>{{if .Cleanup}}Тестовое подключение{{else}}Аккаунт производителя{{end}}</legend>
{{if eq (len .Providers) 1}}{{range .Providers}}
<p>{{.Label}}</p>
<input type=hidden name=provider value="{{.ID}}">
{{end}}{{else}}
<p><label for=provider>Производитель</label><br>
<select id=provider name=provider required>
<option value="" selected>Выберите производителя</option>{{range .Providers}}
<option value="{{.ID}}">{{.Label}}</option>{{end}}
</select></p>
{{end}}
<p><label for=username>{{if .Cleanup}}Тестовый логин{{else}}Логин аккаунта производителя{{end}}</label><br>
<input id=username name=username type=text maxlength=320 autocomplete={{if .Cleanup}}off{{else}}username{{end}} required></p>
<p><label for=password>{{if .Cleanup}}Тестовое слово{{else}}Пароль аккаунта производителя{{end}}</label><br>
<input id=password name=password type={{if .Cleanup}}text{{else}}password{{end}} maxlength=1024 autocomplete={{if .Cleanup}}off{{else}}current-password{{end}} required></p>
</fieldset>
<p><label><input name=confirm type=checkbox value=yes required> Я разрешаю этому приложению запрошенный доступ для данного подключения.</label></p>
<p><button type=submit>Подключить и разрешить доступ</button></p>
</form>
</main>
</body>
</html>`))

var loginFailureTemplate = template.Must(template.New("login-failure").Parse(`<!doctype html>
<html lang=ru>
<head>
<meta charset=utf-8>
<meta name=viewport content="width=device-width, initial-scale=1">
<title>Не удалось подключить умный дом</title>
</head>
<body>
<main>
<h1>Не удалось подключить умный дом</h1>
<p>{{.}}</p>
<p>Для новой попытки начните подключение заново в приложении.</p>
</main>
</body>
</html>`))

func scopePermissions(scope string) []string {
	var permissions []string
	for _, s := range strings.Fields(scope) {
		switch s {
		case oauth.ReadScope:
			permissions = append(permissions, "Просмотр устройств, их состояния и карты.")
		case oauth.ControlScope:
			permissions = append(permissions, "Запуск уборки выбранных комнат. Для Dreame эта возможность пока не включена.")
		}
	}
	return permissions
}

func (h *Handler) authDenied(w http.ResponseWriter, gate string, retryPage bool) {
	if h.onAuthDenial != nil {
		h.onAuthDenial(gate)
	}
	if !retryPage {
		h.failure(w, http.StatusForbidden, "access_denied")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = loginFailureTemplate.Execute(w, "Не удалось подтвердить эту попытку подключения. Страница могла устареть.")
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		if h.limited("authorize", 30) {
			h.failure(w, 429, "slow_down")
			return
		}
		v, e := url.ParseQuery(r.URL.RawQuery)
		if e == nil {
			v, e = authorizationFields(v)
		}
		if e != nil || v.Get("response_type") != "code" {
			h.failure(w, 400, "invalid_request")
			return
		}
		x, e := h.oauth.Start(oauth.StartRequest{Client: v.Get("client_id"), Redirect: v.Get("redirect_uri"), State: v.Get("state"), Challenge: v.Get("code_challenge"), Method: v.Get("code_challenge_method"), Resource: v.Get("resource"), Scope: v.Get("scope")})
		if e != nil {
			h.failure(w, 400, oauth.Code(e))
			return
		}
		if h.cleanupAutoActivationAllowed(x.Flow) {
			h.authorizeCleanupAutomatically(w, r, x)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: flowCookiePrefix + x.ID, Value: x.Cookie, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 300})
		// Form-navigation POSTs need the same-origin Origin header for the login check.
		// no-referrer makes browsers send Origin: null for this non-CORS request.
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		cleanup := h.cleanupAuthorizationAllowed(x.Flow)
		var cleanupProof string
		if cleanup {
			cleanupProof = x.Cookie
		}
		_ = loginTemplate.Execute(w, map[string]any{"Client": x.ClientName, "Permissions": scopePermissions(x.Flow.Scope), "Action": h.origin + "/oauth/authorize", "Flow": x.ID, "CSRF": x.CSRF, "Providers": h.labels, "Cleanup": cleanup, "CleanupProof": cleanupProof})
		return
	}
	if r.Method != "POST" {
		h.method(w, "GET, POST")
		return
	}
	if h.limited("login", 10) {
		h.failure(w, 429, "slow_down")
		return
	}
	origins := r.Header.Values("Origin")
	var originGate string
	switch {
	case len(origins) == 0 || len(origins) == 1 && origins[0] == "":
		originGate = "origin_missing"
	case len(origins) != 1:
		originGate = "origin_duplicate"
	case origins[0] == "null":
		originGate = "origin_null"
	case origins[0] != h.origin:
		originGate = "origin_foreign"
	}
	if originGate != "" {
		h.authDenied(w, originGate, false)
		return
	}
	v, e := form(r, "flow", "csrf", "provider", "username", "password", "confirm", "cleanup_proof")
	if e != nil {
		h.failure(w, 400, "invalid_request")
		return
	}
	if !flowID.MatchString(v.Get("flow")) {
		h.authDenied(w, "flow_proof_or_expiry", true)
		return
	}
	cookieName := flowCookiePrefix + v.Get("flow")
	var cookie string
	count := 0
	for _, c := range r.Cookies() {
		if c.Name == cookieName {
			cookie = c.Value
			count++
		}
	}
	_, cleanupProofSupplied := v["cleanup_proof"]
	if count == 0 && cleanupProofSupplied {
		// The temporary cleanup form carries the same random, single-use proof.
		// TakeFlow still checks it and CSRF atomically; the returned flow must
		// also pass the exact cleanup guard before any account/provider access.
		cookie = v.Get("cleanup_proof")
	} else if count != 1 {
		gate := "cookie_missing"
		if count > 1 {
			gate = "cookie_duplicate"
		}
		h.authDenied(w, gate, true)
		return
	}
	p := h.providers[v.Get("provider")]
	if p == nil || v.Get("confirm") != "yes" || len(v.Get("username")) == 0 || len(v.Get("username")) > 320 || len(v.Get("password")) == 0 || len(v.Get("password")) > 1024 {
		h.failure(w, 400, "invalid_request")
		return
	}
	f, e := h.oauth.TakeFlow(v.Get("flow"), cookie, v.Get("csrf"))
	if e != nil {
		h.authDenied(w, "flow_proof_or_expiry", true)
		return
	}
	cleanup := h.cleanupAuthorizationAllowed(f)
	if cleanupProofSupplied && !cleanup {
		h.authDenied(w, "cleanup_not_allowed", true)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	select {
	case h.authSlots <- struct{}{}:
		defer func() { <-h.authSlots }()
	default:
		h.failure(w, 429, "temporarily_unavailable")
		return
	}
	id, dir, e := h.store.NewAccount()
	if e != nil {
		h.failure(w, 503, "temporarily_unavailable")
		return
	}
	linked := false
	defer func() {
		if !linked {
			_ = h.store.RemoveUnlinkedAccount(id)
		}
	}()
	ctx, cancel := context.WithTimeout(r.Context(), h.authTimeout)
	defer cancel()
	credentials := provider.Credentials{Username: v.Get("username"), Password: v.Get("password")}
	linkedProvider := p.ID()
	if cleanup {
		linkedProvider = cleanupProviderID
	} else {
		e = authenticateProvider(ctx, p, dir, credentials)
	}
	credentials = provider.Credentials{}
	v.Del("password")
	v.Del("username")
	if e != nil || ctx.Err() != nil {
		message := "Не удалось подключить аккаунт. Проверьте данные для входа и доступность сервиса производителя."
		if ctx.Err() == nil && provider.SafeCode(e) == "account_connection_unverified" {
			message = "Не удалось подтвердить доступ к устройствам. Подключение не сохранено."
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_ = loginFailureTemplate.Execute(w, message)
		return
	}
	code, e := h.oauth.Complete(f, id, linkedProvider)
	if e != nil {
		h.failure(w, 503, "temporarily_unavailable")
		return
	}
	linked = true
	redirectAuthorization(w, r, f, code)
}

func redirectAuthorization(w http.ResponseWriter, r *http.Request, f store.Flow, code string) {
	u, _ := url.Parse(f.Redirect)
	q := u.Query()
	q.Set("code", code)
	q.Del("state")
	if f.State != "" {
		q.Set("state", f.State)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}
func (h *Handler) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		h.method(w, "POST")
		return
	}
	if h.limited("token", 120) {
		h.failure(w, 429, "slow_down")
		return
	}
	if len(r.Header.Values("Authorization")) != 0 {
		h.failure(w, 400, "invalid_request")
		return
	}
	v, e := form(r, "grant_type", "client_id", "code", "redirect_uri", "code_verifier", "refresh_token", "resource", "scope")
	if e != nil {
		h.failure(w, 400, "invalid_request")
		return
	}
	if v.Get("grant_type") == "authorization_code" && v.Get("refresh_token") != "" || v.Get("grant_type") == "refresh_token" && (v.Get("code") != "" || v.Get("code_verifier") != "" || v.Get("redirect_uri") != "") {
		h.failure(w, 400, "invalid_request")
		return
	}
	t, e := h.oauth.Exchange(oauth.TokenRequest{GrantType: v.Get("grant_type"), Client: v.Get("client_id"), Code: v.Get("code"), Redirect: v.Get("redirect_uri"), Verifier: v.Get("code_verifier"), Refresh: v.Get("refresh_token"), Resource: v.Get("resource"), Scope: v.Get("scope")})
	if e != nil {
		h.failure(w, 400, oauth.Code(e))
		return
	}
	h.json(w, 200, t)
}
func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		h.method(w, "POST")
		return
	}
	if h.limited("revoke", 120) {
		h.failure(w, 429, "slow_down")
		return
	}
	if len(r.Header.Values("Authorization")) != 0 {
		h.failure(w, 400, "invalid_request")
		return
	}
	v, e := form(r, "token", "client_id", "token_type_hint")
	if e != nil || v.Get("client_id") == "" || len(v.Get("token")) > 256 {
		h.failure(w, 400, "invalid_request")
		return
	}
	if h.oauth.Revoke(v.Get("token"), v.Get("client_id")) != nil {
		h.failure(w, 503, "temporarily_unavailable")
		return
	}
	h.json(w, 200, map[string]any{})
}
