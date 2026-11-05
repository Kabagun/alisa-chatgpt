package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"strings"
)

const maxSignedURLResponse = 64 << 10
const maxSignedURL = 8192

// A map reference is usable only inside this package and only from the
// authenticated fixed6.8 property response. Formatting never exposes it.
type privateMapReference struct {
	name    string
	invalid bool
}
type privateMapURL struct{ value string }

func (privateMapURL) String() string               { return "[map URL redacted]" }
func (privateMapURL) GoString() string             { return "[map URL redacted]" }
func (u privateMapURL) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte(u.String())) }

func (privateMapReference) String() string               { return "[map reference redacted]" }
func (privateMapReference) GoString() string             { return "[map reference redacted]" }
func (r privateMapReference) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte(r.String())) }

type SavedObjectDiagnostics struct {
	Outcome        string `json:"outcome"`
	Scheme         string `json:"scheme_class"`
	Host           string `json:"host_class"`
	Port           string `json:"port_class"`
	Hostname       string `json:"hostname,omitempty"`
	FetchPerformed bool   `json:"fetch_performed"`
}

func (SavedObjectDiagnostics) String() string   { return "[saved object metadata redacted]" }
func (SavedObjectDiagnostics) GoString() string { return "[saved object metadata redacted]" }
func (s SavedObjectDiagnostics) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(s.String()))
}

func newSavedObjectDiagnostics() SavedObjectDiagnostics {
	return SavedObjectDiagnostics{Outcome: "not_referenced", Scheme: "not_observed", Host: "not_observed", Port: "not_observed"}
}

func parsePrivateMapReference(raw json.RawMessage) privateMapReference {
	if len(raw) == 0 || len(raw) > 16<<10 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return privateMapReference{}
	}
	var text string
	// The pinned MAP_LIST handler requires a JSON object encoded in a string.
	// A direct object may be diagnosed in phase1 but never supplies a filename.
	if json.Unmarshal(raw, &text) != nil {
		return privateMapReference{invalid: true}
	}
	if text == "" {
		return privateMapReference{}
	}
	if !privateMapText(text, 16<<10) {
		return privateMapReference{invalid: true}
	}
	if !uniqueDescriptorKeys(text) {
		return privateMapReference{invalid: true}
	}
	var object struct {
		Object *string `json:"object_name"`
		Alias  *string `json:"obj_name"`
	}
	if json.Unmarshal([]byte(text), &object) != nil {
		return privateMapReference{invalid: true}
	}
	if object.Object != nil && object.Alias != nil && *object.Object != "" && *object.Alias != "" && *object.Object != *object.Alias {
		return privateMapReference{invalid: true}
	}
	name := ""
	if object.Object != nil {
		name = *object.Object
	} else if object.Alias != nil {
		name = *object.Alias
	}
	if name == "" {
		return privateMapReference{}
	}
	if !privateMapText(name, 2048) {
		return privateMapReference{invalid: true}
	}
	// Preserve the opaque reference intact, including any comma. This is
	// getDownloadUrl, not getOss1dDownloadUrl or the OBJECT_NAME suffix decoder.
	return privateMapReference{name: name}
}

func uniqueDescriptorKeys(value string) bool {
	d := json.NewDecoder(strings.NewReader(value))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] || len(seen) >= 32 {
			return false
		}
		seen[key] = true
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return false
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return false
	}
	return d.Decode(new(any)) == io.EOF
}

func (c *Client) readSavedObjectURLMetadata(ctx context.Context, device, model string, reference privateMapReference, d *MapDiagnostics) (privateMapURL, error) {
	locator, metadata, err := c.locateMapObject(ctx, device, model, reference)
	d.SavedObject = metadata
	return locator, err
}
func (c *Client) locateMapObject(ctx context.Context, device, model string, reference privateMapReference) (privateMapURL, SavedObjectDiagnostics, error) {
	d := newSavedObjectDiagnostics()
	if reference.invalid {
		d.Outcome = "reference_invalid"
		return privateMapURL{}, d, nil
	}
	if reference.name == "" {
		return privateMapURL{}, d, nil
	}
	if !privateMapText(model, 128) {
		d.Outcome = "reference_invalid"
		return privateMapURL{}, d, nil
	}
	params := map[string]any{"did": device, "model": model, "filename": reference.name, "region": c.session.ServerRegion}
	raw, err := c.postBounded(ctx, mapDownloadURLPath, params, maxSignedURLResponse)
	reference.name = ""
	delete(params, "filename")
	if err != nil {
		if mapReadAuthenticationError(err) {
			return privateMapURL{}, d, err
		}
		d.Outcome = mapReadOutcome(err)
		return privateMapURL{}, d, nil
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	var signedURL string
	if json.Unmarshal(raw, &envelope) != nil || json.Unmarshal(envelope.Data, &signedURL) != nil || signedURL == "" {
		d.Outcome = "response_invalid"
		return privateMapURL{}, d, nil
	}
	d = inspectSignedMapURL(signedURL)
	if d.Outcome != "metadata_available" {
		return privateMapURL{}, d, nil
	}
	return privateMapURL{value: signedURL}, d, nil
}

// dnsMetadataHost is syntactic validation, not a DNS lookup or proof of public
// reachability/ownership. The exact returned host still requires root review.
func dnsMetadataHost(host string) bool {
	if len(host) < 3 || len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".invalid", ".test", ".example", ".home.arpa", ".onion"} {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	tld := host[strings.LastIndexByte(host, '.')+1:]
	if !strings.HasPrefix(tld, "xn--") {
		for _, r := range tld {
			if r < 'a' || r > 'z' {
				return false
			}
		}
	}
	return true
}

func inspectSignedMapURL(value string) SavedObjectDiagnostics {
	d := newSavedObjectDiagnostics()
	d.Outcome = "url_rejected"
	if len(value) > maxSignedURL {
		d.Outcome = "response_limit"
		return d
	}
	if !privateMapText(value, maxSignedURL) {
		d.Scheme = "invalid"
		d.Host = "invalid"
		d.Port = "invalid"
		return d
	}
	u, err := url.Parse(value)
	if err != nil {
		d.Scheme = "invalid"
		d.Host = "invalid"
		d.Port = "invalid"
		return d
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		d.Scheme = "https"
	case "http":
		d.Scheme = "http"
	case "":
		d.Scheme = "missing"
	default:
		d.Scheme = "other"
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	switch {
	case host == "":
		d.Host = "missing"
	case strings.Contains(host, "%"):
		d.Host = "invalid"
	default:
		if _, err := netip.ParseAddr(host); err == nil {
			d.Host = "ip_literal"
		} else if dnsMetadataHost(host) {
			d.Host = "dns_name"
		} else {
			d.Host = "reserved_or_invalid"
		}
	}
	port := u.Port()
	switch {
	case strings.HasSuffix(u.Host, ":"):
		d.Port = "invalid"
	case port == "" && d.Scheme == "https":
		d.Port = "default_443"
	case port == "443":
		d.Port = "explicit_443"
	default:
		d.Port = "other"
	}
	if d.Scheme != "https" || d.Host != "dns_name" || (d.Port != "default_443" && d.Port != "explicit_443") || u.User != nil || strings.Contains(value, "#") || u.Opaque != "" {
		return d
	}
	d.Outcome = "metadata_available"
	d.Hostname = host
	return d
}
