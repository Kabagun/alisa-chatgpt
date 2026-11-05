// Package cloud implements bounded reads and separate session provisioning. It does not log
// requests, tokens, raw responses, transport errors, or account identifiers.
package cloud

import (
	"encoding/base64"
	"io"
	"strings"
	"time"
	"unicode"

	"alisa-dreame-readonly/internal/bridge"
)

const UpstreamCommit = "f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585"
const MaxSession = 24 << 10

// Session is an issued session, not an email/password login request. Profile
// values are imported from the pinned upstream app profile; none are guessed.
// Never print or serialize this type in diagnostics.
type Session struct {
	Schema        int           `json:"schema"`
	ServerRegion  string        `json:"server_region"`
	ExpiresAt     string        `json:"expires_at"`
	AccessToken   string        `json:"access_token"`
	RefreshToken  string        `json:"refresh_token,omitempty"`
	TenantID      string        `json:"tenant_id"`
	Profile       Profile       `json:"profile"`
	LocaleContext LocaleContext `json:"locale_context,omitempty"`
	RoutingProof  *RoutingProof `json:"routing_proof,omitempty"`
}

const routingProofMethod = "authenticated_nonempty_catalog"

// RoutingProof records a previously verified working EU connection. It never
// asserts registered country, account geography, model or room capability.
type RoutingProof struct {
	Method     string `json:"method"`
	VerifiedAt string `json:"verified_at"`
}

type LocaleContext struct {
	Region  string `json:"region,omitempty"`
	Lang    string `json:"lang,omitempty"`
	Country string `json:"country,omitempty"`
}

type Profile struct {
	UpstreamCommit     string `json:"upstream_commit"`
	SigningKey         string `json:"signing_key"`
	BasicAuthorization string `json:"basic_authorization"`
	UserAgent          string `json:"user_agent"`
	Metadata           string `json:"dreame_meta"`
	Locale             string `json:"dreame_rlc,omitempty"`
}

func (Session) String() string { return "[redacted Dreame session]" }
func (Profile) String() string { return "[redacted Dreame app profile]" }

// These are the Dreamehome server choices in the pinned config_flow.py. Xiaomi
// region codes (such as de) and arbitrary hostnames are not accepted.
var origins = map[string]string{
	"eu": "https://eu.iot.dreame.tech:13267", "cn": "https://cn.iot.dreame.tech:13267",
	"us": "https://us.iot.dreame.tech:13267", "ru": "https://ru.iot.dreame.tech:13267",
	"sg": "https://sg.iot.dreame.tech:13267", "kr": "https://kr.iot.dreame.tech:13267",
	"by": "https://by.iot.dreame.tech:13267",
}

func sessionError(code string) error { return &bridge.Fault{Code: code} }

func clean(s string, min, max int) bool {
	return len(s) >= min && len(s) <= max && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || r > 126 })
}

func ParseSession(r io.Reader, now time.Time) (*Session, error) {
	return parseSession(r, now, false)
}

func parseSession(r io.Reader, now time.Time, refresh bool) (*Session, error) {
	var s Session
	if bridge.DecodeOne(r, MaxSession, &s, true) != nil {
		return nil, sessionError("session_invalid")
	}
	if err := s.validate(now, refresh); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s Session) Validate(now time.Time) error {
	return s.validate(now, false)
}

func (s Session) validate(now time.Time, refresh bool) error {
	if s.Schema != 1 || origins[s.ServerRegion] == "" || s.Profile.UpstreamCommit != UpstreamCommit {
		return sessionError("session_invalid")
	}
	if s.RoutingProof != nil {
		verified, err := time.Parse(time.RFC3339, s.RoutingProof.VerifiedAt)
		if err != nil || s.RoutingProof.Method != routingProofMethod || s.ServerRegion != "eu" || verified.Location() != time.UTC || verified.After(now.Add(5*time.Minute)) {
			return sessionError("session_invalid")
		}
	}
	if !clean(s.AccessToken, 1, 8192) || !clean(s.TenantID, 1, 128) || !clean(s.Profile.SigningKey, 1, 256) || !clean(s.Profile.UserAgent, 1, 256) || !clean(s.Profile.Metadata, 1, 1024) {
		return sessionError("session_invalid")
	}
	if s.RefreshToken != "" && !clean(s.RefreshToken, 1, 8192) {
		return sessionError("session_invalid")
	}
	if refresh && s.RefreshToken == "" {
		return sessionError("session_reauthentication_required")
	}
	if refresh && s.Profile.Locale != "" && (s.LocaleContext.Region == "" || s.LocaleContext.Lang == "" || s.LocaleContext.Country == "") {
		return sessionError("session_reauthentication_required")
	}
	for _, locale := range []string{s.LocaleContext.Region, s.LocaleContext.Lang, s.LocaleContext.Country} {
		if locale != "" && !clean(locale, 1, 64) {
			return sessionError("session_invalid")
		}
	}
	if !clean(s.Profile.BasicAuthorization, 1, 2048) {
		return sessionError("session_invalid")
	}
	decoded, err := base64.StdEncoding.DecodeString(s.Profile.BasicAuthorization)
	if err != nil || len(decoded) == 0 {
		return sessionError("session_invalid")
	}
	if s.Profile.Locale != "" {
		if !clean(s.Profile.Locale, 1, 512) {
			return sessionError("session_invalid")
		}
		if _, err := base64.StdEncoding.DecodeString(s.Profile.Locale); err != nil {
			return sessionError("session_invalid")
		}
	}
	expires, err := time.Parse(time.RFC3339, s.ExpiresAt)
	if err != nil {
		return sessionError("session_invalid")
	}
	if !refresh && !expires.After(now.Add(30*time.Second)) {
		return sessionError("session_expired")
	}
	return nil
}
