// Package dreame adapts the bounded, read-only Dreame CLI protocol to accounts.
package dreame

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"smart-home-plugin/internal/provider"
)

const upstreamCommit = "f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585"

// Config is trusted deployment configuration, never request input. Both CLI
// paths are fixed executables. ProfilePath is an operator-supplied public app
// signing profile; no profile constants are embedded in this package.
type Config struct {
	ReaderPath, ProvisionerPath, ProfilePath string
	Timeout                                  time.Duration
}

type Adapter struct {
	reader, provisioner string
	profile             json.RawMessage
	timeout             time.Duration
}

var _ provider.Provider = (*Adapter)(nil)

func (*Adapter) ID() string       { return "dreame" }
func (*Adapter) Label() string    { return "Dreamehome" }
func (*Adapter) String() string   { return "[Dreame adapter]" }
func (*Adapter) GoString() string { return "[Dreame adapter]" }

func New(c Config) (*Adapter, error) {
	for _, path := range []string{c.ReaderPath, c.ProvisionerPath, c.ProfilePath} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || !trustedFile(path) {
			return nil, provider.Err("provider_unavailable")
		}
	}
	if !executable(c.ReaderPath) || !executable(c.ProvisionerPath) {
		return nil, provider.Err("provider_unavailable")
	}
	f, err := os.Open(c.ProfilePath)
	if err != nil {
		return nil, provider.Err("provider_unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 || !validProfile(raw) {
		return nil, provider.Err("provider_unavailable")
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.Timeout < time.Millisecond || c.Timeout > 30*time.Second {
		return nil, provider.Err("provider_unavailable")
	}
	return &Adapter{reader: c.ReaderPath, provisioner: c.ProvisionerPath, profile: raw, timeout: c.Timeout}, nil
}

func validProfile(raw []byte) bool {
	var p struct {
		Commit      string            `json:"upstream_commit"`
		Key         string            `json:"signing_key"`
		Basic       string            `json:"basic_authorization"`
		Agent       string            `json:"user_agent"`
		Version     string            `json:"app_version"`
		Salt        string            `json:"password_salt"`
		Tenant      string            `json:"default_tenant"`
		MapHosts    []string          `json:"map_storage_hosts,omitempty"`
		MapIVs      map[string]string `json:"map_model_ivs,omitempty"`
		MapVersions map[string]int    `json:"map_model_versions,omitempty"`
	}
	if decode(raw, &p, true) != nil || p.Commit != upstreamCommit || len(p.Key) != 16 || len(p.Salt) != 16 || !text(p.Agent, 256, false) || !text(p.Tenant, 128, false) || !text(p.Version, 20, false) || !text(p.Basic, 2048, false) {
		return false
	}
	for _, r := range p.Version {
		if r < '0' || r > '9' {
			return false
		}
	}
	b, err := base64.StdEncoding.DecodeString(p.Basic)
	if err != nil || len(b) == 0 || len(p.MapHosts) > 4 || len(p.MapIVs) > 16 || len(p.MapVersions) > 16 {
		return false
	}
	seen := map[string]bool{}
	for _, host := range p.MapHosts {
		if !privateMetadataHostname(host) || seen[host] {
			return false
		}
		seen[host] = true
	}
	for model, iv := range p.MapIVs {
		if !profileModel(model) || len(iv) != 16 || !text(iv, 16, false) {
			return false
		}
	}
	for model, v := range p.MapVersions {
		if !profileModel(model) || v < 0 || v > 3 {
			return false
		}
	}
	return true
}
func profileModel(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func regionOK(r string) bool {
	switch r {
	case "by", "eu", "ru", "us", "cn", "sg", "kr":
		return true
	}
	return false
}

func text(s string, max int, empty bool) bool {
	return (empty || s != "") && len(s) <= max && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

func deviceID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func decode(raw []byte, dst any, strict bool) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if strict {
		d.DisallowUnknownFields()
	}
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return provider.Err("provider_unavailable")
	}
	return nil
}

func (a *Adapter) Authenticate(ctx context.Context, dir string, c provider.Credentials) error {
	automatic := c.Region == "" || c.Region == "auto"
	expectedRegion := c.Region
	if automatic {
		expectedRegion = "eu"
	}
	if !automatic && !regionOK(c.Region) {
		return provider.Err("invalid_region")
	}
	if !text(c.Username, 320, false) || !strings.Contains(c.Username, "@") || len(c.Password) == 0 || len(c.Password) > 1024 || !utf8.ValidString(c.Password) {
		return provider.Err("invalid_credentials")
	}
	session, err := accountSession(dir)
	if err != nil {
		return err
	}
	input := struct {
		Schema   int             `json:"schema"`
		Region   string          `json:"server_region"`
		Username string          `json:"username"`
		Password string          `json:"password"`
		Profile  json.RawMessage `json:"profile"`
	}{1, c.Region, c.Username, c.Password, a.profile}
	payload, err := json.Marshal(input)
	input.Username = ""
	input.Password = ""
	c = provider.Credentials{}
	if err != nil || len(payload) > 24<<10 {
		return provider.Err("invalid_credentials")
	}
	defer clear(payload)
	raw, err := a.run(ctx, a.provisioner, []string{"--session", session}, payload)
	if err != nil {
		return err
	}
	if err = provisionResult(raw, automatic, expectedRegion); err != nil {
		return err
	}
	if !privateSession(session) {
		return provider.Err("provider_unavailable")
	}
	return nil
}

func provisionResult(raw []byte, requireVerified bool, expectedRegion string) error {
	data, err := envelope(raw, "dreame_provision")
	if err != nil {
		return safeFault(err)
	}
	var result struct {
		Saved    *bool  `json:"session_saved"`
		Refresh  *bool  `json:"refresh_available"`
		Password *bool  `json:"password_stored"`
		Region   string `json:"resolved_server_region"`
		Verified *bool  `json:"routing_verified"`
	}
	if decode(data, &result, true) != nil || result.Saved == nil || !*result.Saved || result.Refresh == nil || result.Password == nil || *result.Password {
		return provider.Err("provider_unavailable")
	}
	if result.Region != "" && !regionOK(result.Region) || result.Verified != nil && *result.Verified && result.Region != "eu" {
		return provider.Err("provider_unavailable")
	}
	if result.Region != "" && expectedRegion != "" && result.Region != expectedRegion {
		return provider.Err("provider_unavailable")
	}
	if requireVerified && (result.Region != "eu" || result.Verified == nil || !*result.Verified) {
		return provider.Err("account_connection_unverified")
	}
	return nil
}

func (a *Adapter) Execute(ctx context.Context, dir string, op provider.Operation, p provider.Params) (json.RawMessage, error) {
	if err := p.Validate(op); err != nil {
		return nil, err
	}
	if op != provider.Devices && !deviceID(p.DeviceID) {
		return nil, provider.Err("invalid_params")
	}
	if op == provider.CleanRooms && !validControlParams(p) {
		return nil, provider.Err("invalid_params")
	}
	session, err := accountSession(dir)
	if err != nil {
		return nil, err
	}
	if !privateSession(session) {
		return nil, provider.Err("invalid_credentials")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		data, e := a.read(ctx, session, "devices", "")
		var result json.RawMessage
		if e == nil {
			var owned []device
			result, owned, e = devices(data)
			if e == nil && op != provider.Devices {
				found := false
				for _, d := range owned {
					if d.ID == p.DeviceID {
						found = true
						break
					}
				}
				if !found {
					return nil, provider.Err("device_not_found")
				}
				if op == provider.CleanRooms {
					return a.control(ctx, session, p)
				}
				data, e = a.read(ctx, session, string(op), p.DeviceID)
				if e == nil {
					if op == provider.Status {
						result, e = status(data, p.DeviceID)
					} else {
						result, e = rooms(data, p.DeviceID)
					}
				}
			}
		}
		if e == nil {
			return result, nil
		}
		if attempt == 0 && refreshable(e) {
			raw, re := a.run(ctx, a.provisioner, []string{"--refresh", "--session", session}, nil)
			if re != nil {
				return nil, re
			}
			if re = provisionResult(raw, false, ""); re != nil {
				return nil, re
			}
			if !privateSession(session) {
				return nil, provider.Err("provider_unavailable")
			}
			continue
		}
		return nil, safeFault(e)
	}
	return nil, provider.Err("invalid_credentials")
}

func (a *Adapter) read(ctx context.Context, session, op, id string) (data json.RawMessage, err error) {
	request := struct {
		Schema int    `json:"schema"`
		Op     string `json:"op"`
		Device string `json:"device_id,omitempty"`
	}{1, op, id}
	payload, _ := json.Marshal(request)
	args := []string{"--session", session}
	if op == "rooms" {
		path, remove, stageErr := stageMapProfile(filepath.Dir(session), a.profile)
		if stageErr != nil {
			return nil, stageErr
		}
		defer func() {
			if remove() != nil {
				data = nil
				err = provider.Err("provider_unavailable")
			}
		}()
		args = append(args, "--map-profile", path)
	}
	raw, err := a.run(ctx, a.reader, args, payload)
	if err != nil {
		return nil, err
	}
	return envelope(raw, "dreame_cloud")
}

// Internal faults contain a code only. Vendor strings never become public errors.
type cliFault string

func (cliFault) Error() string { return "provider_unavailable" }
func refreshable(err error) bool {
	f, ok := err.(cliFault)
	return ok && (f == "session_expired" || f == "session_reauthentication_required")
}
func safeFault(err error) error {
	f, ok := err.(cliFault)
	if !ok {
		return provider.Err(provider.SafeCode(err))
	}
	switch f {
	case "authentication_connection_unverified":
		return provider.Err("account_connection_unverified")
	case "session_expired", "session_reauthentication_required", "authentication_failed", "login_input_invalid", "session_unavailable":
		return provider.Err("invalid_credentials")
	case "session_busy":
		return provider.Err("busy")
	case "device_not_found":
		return provider.Err("device_not_found")
	case "invalid_device_id":
		return provider.Err("invalid_params")
	case "authentication_challenge_unsupported", "inline_room_metadata_unavailable", "map_unavailable", "operation_denied", "device_routing_unavailable":
		return provider.Err("operation_unavailable")
	case "authentication_transport_failed", "authentication_timeout", "authentication_redirect_denied", "authentication_rate_limited", "authentication_encoding_unsupported", "authentication_response_limit", "authentication_service_failed", "authentication_response_invalid", "cloud_timeout", "cloud_transport_failed", "cloud_redirect_denied", "cloud_http_failed", "cloud_encoding_unsupported", "cloud_response_limit", "cloud_response_invalid", "cloud_api_failed", "cloud_rate_limited":
		return provider.Err("upstream_unavailable")
	default:
		return provider.Err("provider_unavailable")
	}
}

func envelope(raw []byte, mode string) (json.RawMessage, error) {
	var r struct {
		Schema   int             `json:"schema"`
		Mode     string          `json:"mode"`
		ReadOnly bool            `json:"read_only"`
		OK       *bool           `json:"ok"`
		Data     json.RawMessage `json:"data"`
		Error    *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if decode(raw, &r, true) != nil || r.Schema != 1 || r.Mode != mode || !r.ReadOnly || r.OK == nil {
		return nil, provider.Err("provider_unavailable")
	}
	if !*r.OK {
		if r.Error == nil || len(r.Error.Code) > 80 || r.Data != nil {
			return nil, provider.Err("provider_unavailable")
		}
		return nil, cliFault(r.Error.Code)
	}
	if r.Error != nil || len(r.Data) == 0 || bytes.Equal(r.Data, []byte("null")) {
		return nil, provider.Err("provider_unavailable")
	}
	return r.Data, nil
}
