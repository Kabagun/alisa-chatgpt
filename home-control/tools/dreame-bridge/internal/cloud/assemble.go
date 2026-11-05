package cloud

import (
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"alisa-dreame-readonly/internal/bridge"
)

// TokenImport receives only the explicitly supplied issued-token subset. It
// never looks up accounts, passwords, browser storage or credential managers.
type TokenImport struct {
	Schema       int    `json:"schema"`
	ServerRegion string `json:"server_region"`
	Username     string `json:"username"`
	IssuedAt     string `json:"issued_at"`
	Token        struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		TenantID    string `json:"tenant_id"`
		Region      string `json:"region,omitempty"`
		Lang        string `json:"lang,omitempty"`
		Country     string `json:"country,omitempty"`
	} `json:"token"`
	Profile AppProfile `json:"profile"`
}

type AppProfile struct {
	UpstreamCommit     string            `json:"upstream_commit"`
	SigningKey         string            `json:"signing_key"`
	BasicAuthorization string            `json:"basic_authorization"`
	UserAgent          string            `json:"user_agent"`
	AppVersion         string            `json:"app_version"`
	PasswordSalt       string            `json:"password_salt"`
	DefaultTenant      string            `json:"default_tenant"`
	MapStorageHosts    []string          `json:"map_storage_hosts,omitempty"`
	MapModelIVs        map[string]string `json:"map_model_ivs,omitempty"`
	MapModelVersions   map[string]int    `json:"map_model_versions,omitempty"`
}

func (AppProfile) String() string               { return "[redacted pinned app profile]" }
func (AppProfile) GoString() string             { return "[redacted pinned app profile]" }
func (p AppProfile) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte(p.String())) }

func (TokenImport) String() string { return "[redacted issued-token import]" }

func hashText(s string) string { sum := md5.Sum([]byte(s)); return hex.EncodeToString(sum[:]) }

// AssembleIssuedSession follows login/_base_headers in the pinned source, using
// the exact imported app profile and issued response. It performs no HTTP call.
// The username is used for short metadata hashes and is not retained in Session.
func AssembleIssuedSession(r io.Reader, now time.Time) (*Session, error) {
	var input TokenImport
	if bridge.DecodeOne(r, MaxSession, &input, true) != nil {
		return nil, sessionError("token_import_invalid")
	}
	if input.Schema != 1 || len(input.Username) == 0 || len(input.Username) > 320 || strings.ContainsFunc(input.Username, unicode.IsControl) || len(input.Profile.SigningKey) != 16 || !clean(input.Profile.AppVersion, 1, 20) {
		return nil, sessionError("token_import_invalid")
	}
	for _, c := range input.Profile.AppVersion {
		if c < '0' || c > '9' {
			return nil, sessionError("token_import_invalid")
		}
	}
	issued, err := time.Parse(time.RFC3339, input.IssuedAt)
	if err != nil || issued.After(now.Add(5*time.Minute)) || input.Token.ExpiresIn < 1 || input.Token.ExpiresIn > 365*24*60*60 {
		return nil, sessionError("token_import_invalid")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, sessionError("token_import_unavailable")
	}
	visitor := md5.Sum(random[:])
	metadata := "cv=a_" + input.Profile.AppVersion + ";canvasHash=" + hashText(input.Username + "c")[:8] + ";webglHash=" + hashText(input.Username + "w")[:8] + ";visitorIdHash=" + hex.EncodeToString(visitor[:])
	s := &Session{Schema: 1, ServerRegion: input.ServerRegion, ExpiresAt: issued.Add(time.Duration(input.Token.ExpiresIn) * time.Second).UTC().Format(time.RFC3339), AccessToken: input.Token.AccessToken, TenantID: input.Token.TenantID, Profile: Profile{UpstreamCommit: input.Profile.UpstreamCommit, SigningKey: input.Profile.SigningKey, BasicAuthorization: input.Profile.BasicAuthorization, UserAgent: input.Profile.UserAgent, Metadata: metadata}}
	s.LocaleContext = LocaleContext{Region: input.Token.Region, Lang: input.Token.Lang, Country: input.Token.Country}
	// Upstream adds this header only when all three issued locale values exist.
	if input.Token.Region != "" && input.Token.Lang != "" && input.Token.Country != "" {
		if !clean(input.Token.Region, 1, 64) || !clean(input.Token.Lang, 1, 32) || !clean(input.Token.Country, 1, 32) {
			return nil, sessionError("token_import_invalid")
		}
		plain := []byte(input.Token.Region + "|" + input.Token.Lang + "|" + input.Token.Country)
		padding := aes.BlockSize - len(plain)%aes.BlockSize
		for range padding {
			plain = append(plain, byte(padding))
		}
		cipher, err := aes.NewCipher([]byte(input.Profile.SigningKey))
		if err != nil {
			return nil, sessionError("token_import_invalid")
		}
		for offset := 0; offset < len(plain); offset += aes.BlockSize {
			cipher.Encrypt(plain[offset:offset+aes.BlockSize], plain[offset:offset+aes.BlockSize])
		}
		s.Profile.Locale = base64.StdEncoding.EncodeToString(plain)
	}
	if err := s.Validate(now); err != nil {
		return nil, err
	}
	return s, nil
}

func validUsername(s string) bool {
	return len(s) > 0 && len(s) <= 320 && !strings.ContainsFunc(s, unicode.IsControl)
}

func makeMetadata(username, version string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", sessionError("metadata_unavailable")
	}
	visitor := md5.Sum(random[:])
	return "cv=a_" + version + ";canvasHash=" + hashText(username + "c")[:8] + ";webglHash=" + hashText(username + "w")[:8] + ";visitorIdHash=" + hex.EncodeToString(visitor[:]), nil
}

func localeHeader(key string, locale LocaleContext) (string, error) {
	if locale.Region == "" || locale.Lang == "" || locale.Country == "" {
		return "", nil
	}
	for _, value := range []string{locale.Region, locale.Lang, locale.Country} {
		if !clean(value, 1, 64) {
			return "", sessionError("locale_invalid")
		}
	}
	plain := []byte(locale.Region + "|" + locale.Lang + "|" + locale.Country)
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	for range padding {
		plain = append(plain, byte(padding))
	}
	cipher, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", sessionError("locale_invalid")
	}
	for offset := 0; offset < len(plain); offset += aes.BlockSize {
		cipher.Encrypt(plain[offset:offset+aes.BlockSize], plain[offset:offset+aes.BlockSize])
	}
	return base64.StdEncoding.EncodeToString(plain), nil
}
