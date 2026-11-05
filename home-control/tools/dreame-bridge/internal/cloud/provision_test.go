package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func loginFixture() LoginInput {
	return LoginInput{Schema: 1, ServerRegion: "eu", Username: "fixture@example.test", Password: "synthetic-password", Profile: AppProfile{UpstreamCommit: UpstreamCommit, SigningKey: "0123456789abcdef", BasicAuthorization: "c3ludGhldGlj", UserAgent: "Synthetic/Test", AppVersion: "101010101", PasswordSalt: "fedcba9876543210", DefaultTenant: "fixture-tenant"}}
}

func mockProvisioner(t *testing.T, handler http.HandlerFunc) *Provisioner {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	p := NewProvisioner()
	p.http.Transport = server.Client().Transport
	p.originFor = func(string) string { return server.URL }
	p.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { p.Close(); server.Close() })
	return p
}

func TestPasswordLoginAndRefreshUsePinnedFlow(t *testing.T) {
	var calls atomic.Int32
	p := mockProvisioner(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if r.Method != "POST" || r.URL.Path != tokenPath || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Authorization") != "Basic c3ludGhldGlj" || r.Header.Get("dreame-auth") != "" {
			t.Error("auth endpoint/headers not fixed")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("scope") != "all" || r.Form.Get("platform") != "ANDROID" || r.Form.Get("type") != "account" {
			t.Error("unexpected auth profile")
		}
		if call == 1 {
			fixture := loginFixture()
			if r.Form.Get("grant_type") != "password" || r.Form.Get("username") != fixture.Username || r.Form.Get("password") != hashText(fixture.Password+fixture.Profile.PasswordSalt) || len(r.Form) != 6 {
				t.Error("incorrect salted password form")
			}
			if r.Header.Get("tenant-id") != "fixture-tenant" || r.Header.Get("dreame-rlc") != "" || !strings.HasPrefix(r.Header.Get("dreame-meta"), "cv=a_101010101;canvasHash=") {
				t.Error("incorrect initial metadata")
			}
			io.WriteString(w, `{"access_token":"issued-access-1","refresh_token":"issued-refresh-1","expires_in":3600,"tenant_id":"issued-tenant","region":"eu","lang":"en","country":"BY","uid":"discarded-account-field"}`)
		} else {
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "issued-refresh-1" || len(r.Form) != 5 || r.Form.Get("password") != "" {
				t.Error("incorrect refresh form")
			}
			if r.Header.Get("tenant-id") != "issued-tenant" || r.Header.Get("dreame-rlc") == "" {
				t.Error("refresh did not retain issued metadata")
			}
			io.WriteString(w, `{"access_token":"issued-access-2","refresh_token":"issued-refresh-2","expires_in":3600}`)
		}
	})
	first, err := p.PasswordLogin(context.Background(), loginFixture())
	if err != nil {
		t.Fatal(err)
	}
	if first.AccessToken != "issued-access-1" || first.RefreshToken != "issued-refresh-1" || first.TenantID != "issued-tenant" || first.ExpiresAt != "2026-01-01T01:00:00Z" || first.Profile.Locale == "" {
		t.Fatal("issued session mismatch")
	}
	encoded, _ := json.Marshal(first)
	for _, private := range []string{loginFixture().Password, loginFixture().Username, "password_salt", "discarded-account-field"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("password/account field retained")
		}
	}
	// Refresh is allowed after the read token expires and never falls back to a password.
	first.ExpiresAt = "2000-01-01T00:00:00Z"
	second, err := p.Refresh(context.Background(), *first)
	if err != nil {
		t.Fatal(err)
	}
	if second.AccessToken != "issued-access-2" || second.RefreshToken != "issued-refresh-2" || second.Profile.Locale != first.Profile.Locale || calls.Load() != 2 {
		t.Fatal("refresh rotation mismatch")
	}
}

func TestAuthenticationStopsOnChallengesAndRedactsFailures(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, code string
	}{
		{400, `{"error_description":"captcha required: private-url"}`, "authentication_challenge_unsupported"},
		{200, `{"verification_url":"private-url"}`, "authentication_challenge_unsupported"},
		{403, `{"mfa_required":true}`, "authentication_challenge_unsupported"},
		{401, `{"error_description":"invalid credentials: private-detail"}`, "authentication_failed"},
		{429, `private-detail`, "authentication_rate_limited"},
		{503, `private-detail`, "authentication_service_failed"},
		{200, `{"access_token":"partial","expires_in":null}`, "authentication_response_invalid"},
		{200, strings.Repeat("x", maxAuthResponse+1), "authentication_response_limit"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			var calls atomic.Int32
			p := mockProvisioner(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			_, err := p.PasswordLogin(context.Background(), loginFixture())
			if err == nil || err.Error() != tc.code || calls.Load() != 1 || strings.Contains(err.Error(), "private") {
				t.Fatalf("unexpected failure %v", err)
			}
		})
	}
}

func TestProvisioningRedirectAndInvalidInputNeverLeak(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	var sourceCalls atomic.Int32
	p := mockProvisioner(t, func(w http.ResponseWriter, r *http.Request) {
		sourceCalls.Add(1)
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(302)
	})
	if _, err := p.PasswordLogin(context.Background(), loginFixture()); err == nil || err.Error() != "authentication_redirect_denied" || destinationCalls.Load() != 0 {
		t.Fatal("auth redirect followed")
	}
	before := sourceCalls.Load()
	for _, change := range []func(*LoginInput){func(i *LoginInput) { i.ServerRegion = "example.test" }, func(i *LoginInput) { i.Username = "phone-only" }, func(i *LoginInput) { i.Password = "" }, func(i *LoginInput) { i.Profile.UpstreamCommit = "other" }, func(i *LoginInput) { i.Profile.PasswordSalt = "" }} {
		input := loginFixture()
		change(&input)
		if _, err := p.PasswordLogin(context.Background(), input); err == nil {
			t.Fatal("invalid login accepted")
		}
	}
	if sourceCalls.Load() != before {
		t.Fatal("invalid login performed a request")
	}
	if _, err := p.Refresh(context.Background(), syntheticSession()); err == nil || sourceCalls.Load() != before {
		t.Fatal("missing refresh token transmitted")
	}
}

func TestIssuedSessionAssembler(t *testing.T) {
	var input TokenImport
	input.Schema = 1
	input.ServerRegion = "eu"
	input.Username = "fixture@example.test"
	input.IssuedAt = "2026-01-01T00:00:00Z"
	input.Profile = loginFixture().Profile
	input.Token.AccessToken = "synthetic-access"
	input.Token.TenantID = "synthetic-tenant"
	input.Token.ExpiresIn = 3600
	input.Token.Region = "eu"
	input.Token.Lang = "en"
	input.Token.Country = "BY"
	raw, _ := json.Marshal(input)
	s, err := AssembleIssuedSession(strings.NewReader(string(raw)), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if s.ExpiresAt != "2026-01-01T01:00:00Z" || s.Profile.Locale == "" {
		t.Fatal("assembler missing issued metadata")
	}
	decoded, err := base64.StdEncoding.DecodeString(s.Profile.Locale)
	if err != nil || len(decoded)%16 != 0 {
		t.Fatal("locale header is not padded AES blocks")
	}
	// An already expired issued export must not be relabelled as newly issued.
	input.IssuedAt = "2000-01-01T00:00:00Z"
	raw, _ = json.Marshal(input)
	if _, err := AssembleIssuedSession(strings.NewReader(string(raw)), time.Now()); err == nil {
		t.Fatal("expired import accepted")
	}
}

func TestAutomaticConnectionUsesOneEUOriginAndCatalogProof(t *testing.T) {
	for _, region := range []string{"", "auto"} {
		t.Run("automatic="+region, func(t *testing.T) {
			var logins, catalogs atomic.Int32
			p := mockProvisioner(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case tokenPath:
					if logins.Add(1) != 1 {
						t.Error("credentials resubmitted")
					}
					if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "password" {
						t.Error("unexpected auth request")
					}
					io.WriteString(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"tenant_id":"synthetic-tenant","domain":"https://untrusted.invalid","region":"cn","lang":"zh","country":"BY"}`)
				case listPath:
					catalogs.Add(1)
					if r.Method != http.MethodPost || r.Header.Get("dreame-auth") != "synthetic-access" || r.ContentLength != 0 {
						t.Error("catalog not bounded authenticated read")
					}
					io.WriteString(w, `{"code":0,"data":{"page":{"records":[{"did":"synthetic-1","model":"synthetic.vacuum","customName":"Synthetic vacuum"}]}}}`)
				default:
					t.Error("unexpected endpoint")
				}
			})
			origin := p.originFor
			var regions []string
			p.originFor = func(region string) string { regions = append(regions, region); return origin(region) }
			input := loginFixture()
			input.ServerRegion = region
			s, err := p.PasswordLogin(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if s.ServerRegion != "eu" || s.RoutingProof == nil || s.RoutingProof.Method != routingProofMethod || s.RoutingProof.VerifiedAt != p.now().UTC().Format(time.RFC3339) {
				t.Fatal("missing working-origin proof")
			}
			if s.LocaleContext.Region != "cn" || s.LocaleContext.Country != "BY" || s.Profile.Locale == "" {
				t.Fatal("locale metadata lost or inferred as route")
			}
			if logins.Load() != 1 || catalogs.Load() != 1 || strings.Join(regions, ",") != "eu,eu" {
				t.Fatal("automatic origin changed or credentials retried")
			}
		})
	}
}

func TestAutomaticLoginFailureDoesNotTryCatalogOrOtherOrigin(t *testing.T) {
	var calls atomic.Int32
	p := mockProvisioner(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != tokenPath {
			t.Error("catalog requested after failed login")
		}
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error_description":"synthetic failure"}`)
	})
	origin := p.originFor
	p.originFor = func(region string) string {
		if region != "eu" {
			t.Error("alternate origin attempted")
		}
		return origin(region)
	}
	input := loginFixture()
	input.ServerRegion = ""
	if _, err := p.PasswordLogin(context.Background(), input); err == nil || err.Error() != "authentication_failed" || calls.Load() != 1 {
		t.Fatal("failed login retried or exposed")
	}
}

func TestRoutingProofValidationAndExplicitBackwardCompatibility(t *testing.T) {
	s := syntheticSession()
	now := time.Now()
	if err := s.Validate(now); err != nil {
		t.Fatal("legacy explicit session rejected")
	}
	s.RoutingProof = &RoutingProof{Method: routingProofMethod, VerifiedAt: now.UTC().Format(time.RFC3339)}
	if err := s.Validate(now); err != nil {
		t.Fatal("valid working-origin proof rejected")
	}
	for _, change := range []func(*Session){
		func(s *Session) { s.ServerRegion = "cn" },
		func(s *Session) { s.RoutingProof.Method = "geographic_country" },
		func(s *Session) { s.RoutingProof.VerifiedAt = "invalid" },
		func(s *Session) { s.RoutingProof.VerifiedAt = now.Add(time.Hour).UTC().Format(time.RFC3339) },
	} {
		copy := s
		proof := *s.RoutingProof
		copy.RoutingProof = &proof
		change(&copy)
		if err := copy.Validate(now); err == nil {
			t.Fatal("invalid proof accepted")
		}
	}
}

func TestExplicitRegionModeDoesNotInferOrVerifyAnotherOrigin(t *testing.T) {
	for _, region := range []string{"by", "eu", "ru", "us", "cn", "sg", "kr"} {
		t.Run(region, func(t *testing.T) {
			var calls atomic.Int32
			p := mockProvisioner(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != tokenPath {
					t.Error("legacy explicit mode unexpectedly requested catalog")
				}
				io.WriteString(w, `{"access_token":"synthetic-access","expires_in":3600,"tenant_id":"synthetic-tenant","region":"cn","country":"BY","domain":"https://untrusted.invalid"}`)
			})
			origin := p.originFor
			p.originFor = func(selected string) string {
				if selected != region {
					t.Error("explicit origin changed")
				}
				return origin(selected)
			}
			input := loginFixture()
			input.ServerRegion = region
			s, err := p.PasswordLogin(context.Background(), input)
			if err != nil || s.ServerRegion != region || s.RoutingProof != nil || calls.Load() != 1 {
				t.Fatal("legacy explicit origin changed or acquired unverified proof")
			}
		})
	}
}
