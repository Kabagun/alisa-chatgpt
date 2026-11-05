package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"alisa-dreame-readonly/internal/bridge"
)

const tokenPath = "/dreame-auth/oauth/token"
const maxAuthResponse = 64 << 10

type LoginInput struct {
	Schema       int        `json:"schema"`
	ServerRegion string     `json:"server_region"`
	Username     string     `json:"username"`
	Password     string     `json:"password"`
	Profile      AppProfile `json:"profile"`
}

func (i LoginInput) automatic() bool { return i.ServerRegion == "" || i.ServerRegion == "auto" }

func (LoginInput) String() string { return "[redacted login input]" }

func ParseLoginInput(r io.Reader) (*LoginInput, error) {
	var input LoginInput
	if bridge.DecodeOne(r, MaxSession, &input, true) != nil {
		return nil, sessionError("login_input_invalid")
	}
	if err := input.validate(); err != nil {
		return nil, err
	}
	return &input, nil
}

func (i LoginInput) validate() error {
	p := i.Profile
	if i.Schema != 1 || (!i.automatic() && origins[i.ServerRegion] == "") || !validUsername(i.Username) || !strings.Contains(i.Username, "@") || len(i.Password) == 0 || len(i.Password) > 1024 || !utf8.ValidString(i.Password) {
		return sessionError("login_input_invalid")
	}
	if p.UpstreamCommit != UpstreamCommit || len(p.SigningKey) != 16 || len(p.PasswordSalt) != 16 || !clean(p.UserAgent, 1, 256) || !clean(p.DefaultTenant, 1, 128) || !clean(p.AppVersion, 1, 20) || !clean(p.BasicAuthorization, 1, 2048) {
		return sessionError("app_profile_invalid")
	}
	for _, c := range p.AppVersion {
		if c < '0' || c > '9' {
			return sessionError("app_profile_invalid")
		}
	}
	if decoded, err := base64.StdEncoding.DecodeString(p.BasicAuthorization); err != nil || len(decoded) == 0 {
		return sessionError("app_profile_invalid")
	}
	if !validMapProfile(MapReadProfile{StorageHosts: p.MapStorageHosts, ModelIVs: p.MapModelIVs, ModelVersions: p.MapModelVersions}) {
		return sessionError("app_profile_invalid")
	}
	return nil
}

type Provisioner struct {
	http *http.Client
	now  func() time.Time
	// originFor is fixed to the checked region map in production; tests replace
	// it only inside this package to reach a synthetic TLS server.
	originFor func(string) string
}

func NewProvisioner() *Provisioner {
	return &Provisioner{http: newHTTPClient(), now: time.Now, originFor: func(region string) string { return origins[region] }}
}
func (p *Provisioner) Close() { p.http.CloseIdleConnections() }

func (p *Provisioner) PasswordLogin(ctx context.Context, input LoginInput) (*Session, error) {
	if err := input.validate(); err != nil {
		return nil, err
	}
	// Automatic connection is one supported EU candidate, not geographic
	// discovery. Neither locale/domain hints nor failed credentials select a
	// second origin. Tokens remain in memory until the same-origin catalog is
	// authenticated, parsed and nonempty.
	automatic := input.automatic()
	if automatic {
		input.ServerRegion = "eu"
	}
	metadata, err := makeMetadata(input.Username, input.Profile.AppVersion)
	if err != nil {
		return nil, err
	}
	prior := Session{Schema: 1, ServerRegion: input.ServerRegion, TenantID: input.Profile.DefaultTenant, Profile: Profile{UpstreamCommit: input.Profile.UpstreamCommit, SigningKey: input.Profile.SigningKey, BasicAuthorization: input.Profile.BasicAuthorization, UserAgent: input.Profile.UserAgent, Metadata: metadata}}
	// This salted MD5 construction is required by the pinned app protocol. It
	// travels only inside verified TLS; the password and digest are not stored.
	form := "grant_type=password&scope=all&platform=ANDROID&type=account&username=" + url.QueryEscape(input.Username) + "&password=" + hashText(input.Password+input.Profile.PasswordSalt)
	raw, err := p.authenticate(ctx, prior, form)
	if err != nil {
		return nil, err
	}
	next, err := p.issuedSession(raw, prior)
	if err != nil || !automatic {
		return next, err
	}
	client := &Client{session: *next, origin: p.originFor(next.ServerRegion), http: p.http, now: p.now}
	devices, _, err := client.devices(ctx)
	if err != nil || len(devices) == 0 {
		return nil, sessionError("authentication_connection_unverified")
	}
	next.RoutingProof = &RoutingProof{Method: routingProofMethod, VerifiedAt: p.now().UTC().Format(time.RFC3339)}
	if err := next.Validate(p.now()); err != nil {
		return nil, sessionError("authentication_connection_unverified")
	}
	return next, nil
}

// PasswordLoginAndSave persists only a completely accepted login. In automatic
// mode that includes the same-origin nonempty catalog check. The CLI holds the
// account's session lock before calling this method.
func (p *Provisioner) PasswordLoginAndSave(ctx context.Context, input LoginInput, path string) (*Session, error) {
	next, err := p.PasswordLogin(ctx, input)
	if err != nil {
		return nil, err
	}
	if err := SaveSessionFile(path, *next, p.now()); err != nil {
		return nil, err
	}
	return next, nil
}

func (p *Provisioner) Refresh(ctx context.Context, prior Session) (*Session, error) {
	if err := prior.validate(p.now(), true); err != nil {
		return nil, err
	}
	form := "grant_type=refresh_token&scope=all&platform=ANDROID&type=account&refresh_token=" + url.QueryEscape(prior.RefreshToken)
	raw, err := p.authenticate(ctx, prior, form)
	if err != nil {
		var f *bridge.Fault
		if errors.As(err, &f) && f.Code == "authentication_failed" {
			return nil, sessionError("session_reauthentication_required")
		}
		return nil, err
	}
	return p.issuedSession(raw, prior)
}

func (p *Provisioner) authenticate(ctx context.Context, prior Session, form string) ([]byte, error) {
	if origins[prior.ServerRegion] == "" || len(form) > 16<<10 {
		return nil, sessionError("login_input_invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.originFor(prior.ServerRegion)+tokenPath, strings.NewReader(form))
	if err != nil {
		return nil, sessionError("authentication_transport_failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", prior.Profile.UserAgent)
	req.Header.Set("dreame-meta", prior.Profile.Metadata)
	req.Header.Set("Authorization", "Basic "+prior.Profile.BasicAuthorization)
	req.Header.Set("tenant-id", prior.TenantID)
	if prior.Profile.Locale != "" {
		req.Header.Set("dreame-rlc", prior.Profile.Locale)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, sessionError("authentication_timeout")
		}
		return nil, sessionError("authentication_transport_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, sessionError("authentication_redirect_denied")
	}
	if resp.StatusCode == 429 {
		return nil, sessionError("authentication_rate_limited")
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return nil, sessionError("authentication_encoding_unsupported")
	}
	if resp.ContentLength > maxAuthResponse {
		return nil, sessionError("authentication_response_limit")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthResponse+1))
	if err != nil || len(raw) > maxAuthResponse {
		return nil, sessionError("authentication_response_limit")
	}
	if challengeUnsupported(raw) {
		return nil, sessionError("authentication_challenge_unsupported")
	}
	if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, sessionError("authentication_failed")
	}
	if resp.StatusCode != 200 {
		return nil, sessionError("authentication_service_failed")
	}
	return raw, nil
}

func challengeUnsupported(raw []byte) bool {
	return challengeObject(raw, 0)
}

func challengeObject(raw []byte, depth int) bool {
	if depth > 8 {
		return true
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return false
	}
	for key, value := range object {
		key = strings.ToLower(key)
		for _, marker := range []string{"captcha", "verification", "challenge", "mfa", "2fa", "two_factor"} {
			if strings.Contains(key, marker) && string(value) != "null" && string(value) != "false" && string(value) != `""` {
				return true
			}
		}
		if key == "error_description" || key == "error" || key == "message" {
			var text string
			if json.Unmarshal(value, &text) == nil {
				text = strings.ToLower(text)
				for _, marker := range []string{"captcha", "verification", "challenge", "mfa", "2fa", "two-factor"} {
					if strings.Contains(text, marker) {
						return true
					}
				}
			}
		}
		if len(value) > 0 && value[0] == '{' && challengeObject(value, depth+1) {
			return true
		}
	}
	return false
}

func (p *Provisioner) issuedSession(raw []byte, prior Session) (*Session, error) {
	var token struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    *int64  `json:"expires_in"`
		Tenant       *string `json:"tenant_id"`
		Region       *string `json:"region"`
		Lang         *string `json:"lang"`
		Country      *string `json:"country"`
	}
	if json.Unmarshal(raw, &token) != nil || token.ExpiresIn == nil || *token.ExpiresIn < 31 || *token.ExpiresIn > 365*24*60*60 || token.AccessToken == "" {
		return nil, sessionError("authentication_response_invalid")
	}
	next := prior
	next.AccessToken = token.AccessToken
	next.RefreshToken = token.RefreshToken
	next.ExpiresAt = p.now().Add(time.Duration(*token.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	if token.Tenant != nil {
		next.TenantID = *token.Tenant
	}
	if token.Region != nil {
		next.LocaleContext.Region = *token.Region
	}
	if token.Lang != nil {
		next.LocaleContext.Lang = *token.Lang
	}
	if token.Country != nil {
		next.LocaleContext.Country = *token.Country
	}
	var err error
	next.Profile.Locale, err = localeHeader(next.Profile.SigningKey, next.LocaleContext)
	if err != nil {
		return nil, sessionError("authentication_response_invalid")
	}
	if err := next.Validate(p.now()); err != nil {
		return nil, sessionError("authentication_response_invalid")
	}
	return &next, nil
}
