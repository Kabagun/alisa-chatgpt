package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"alisa-dreame-readonly/internal/bridge"
)

func TestMapReferenceParsingIsBoundedOpaqueAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		raw, name string
		invalid   bool
	}{
		{`"{\"object_name\":\"synthetic-reference,synthetic-key\",\"md5\":\"unused\"}"`, "synthetic-reference,synthetic-key", false},
		{`"{\"obj_name\":\"synthetic-reference\"}"`, "synthetic-reference", false},
		{`"{\"object_name\":null,\"obj_name\":\"synthetic-reference\"}"`, "synthetic-reference", false},
		{`"{\"object_name\":\"\",\"obj_name\":\"synthetic-reference\"}"`, "", false},
		{`"{\"object_name\":\"first\",\"obj_name\":\"second\"}"`, "", true},
		{`"{\"object_name\":\"first\",\"object_name\":\"second\"}"`, "", true},
		{`{"object_name":"direct-object-not-accepted"}`, "", true},
		{`"not-json"`, "", true},
		{`""`, "", false},
	} {
		got := parsePrivateMapReference(json.RawMessage(tc.raw))
		if got.name != tc.name || got.invalid != tc.invalid {
			t.Fatal("reference parsing changed")
		}
		for _, formatted := range []string{fmt.Sprint(got), fmt.Sprintf("%+v", got), fmt.Sprintf("%#v", got)} {
			if strings.Contains(formatted, "synthetic-reference") || strings.Contains(formatted, "synthetic-key") {
				t.Fatal("reference leaked through formatting")
			}
		}
	}
}

func TestSignedMapURLMetadataContainsNoLocatorSecret(t *testing.T) {
	for _, tc := range []struct{ input, outcome, scheme, host, port, hostname string }{
		{"https://STORAGE.EXAMPLE.COM./private-MUST_NOT_ESCAPE?signature=MUST_NOT_ESCAPE", "metadata_available", "https", "dns_name", "default_443", "storage.example.com"},
		{"https://storage.example.com:443/private?key=MUST_NOT_ESCAPE", "metadata_available", "https", "dns_name", "explicit_443", "storage.example.com"},
		{"https://user:MUST_NOT_ESCAPE@storage.example.com/private", "url_rejected", "https", "dns_name", "default_443", ""},
		{"https://storage.example.com/private#MUST_NOT_ESCAPE", "url_rejected", "https", "dns_name", "default_443", ""},
		{"https://storage.example.com/private#", "url_rejected", "https", "dns_name", "default_443", ""},
		{"http://storage.example.com/private", "url_rejected", "http", "dns_name", "other", ""},
		{"https://storage.example.com:8443/private", "url_rejected", "https", "dns_name", "other", ""},
		{"https://storage.example.com:/private", "url_rejected", "https", "dns_name", "invalid", ""},
		{"https://127.0.0.1/private", "url_rejected", "https", "ip_literal", "default_443", ""},
		{"https://[::1]/private", "url_rejected", "https", "ip_literal", "default_443", ""},
		{"https://127.000.0.1/private", "url_rejected", "https", "reserved_or_invalid", "default_443", ""},
		{"https://localhost/private", "url_rejected", "https", "reserved_or_invalid", "default_443", ""},
		{"https://metadata.internal/private", "url_rejected", "https", "reserved_or_invalid", "default_443", ""},
		{"/private?signature=MUST_NOT_ESCAPE", "url_rejected", "missing", "missing", "other", ""},
		{"https://storage.example.com/\nMUST_NOT_ESCAPE", "url_rejected", "invalid", "invalid", "invalid", ""},
	} {
		got := inspectSignedMapURL(tc.input)
		if got.Outcome != tc.outcome || got.Scheme != tc.scheme || got.Host != tc.host || got.Port != tc.port || got.Hostname != tc.hostname || got.FetchPerformed {
			t.Fatal("locator classification changed")
		}
		raw, _ := json.Marshal(got)
		if strings.Contains(string(raw), "MUST_NOT_ESCAPE") || strings.Contains(string(raw), "private?") {
			t.Fatal("signed locator leaked")
		}
		if strings.Contains(fmt.Sprintf("%+v %#v", got, got), "storage.example.com") {
			t.Fatal("private hostname leaked through formatting")
		}
	}
	if got := inspectSignedMapURL("https://storage.example.com/" + strings.Repeat("x", maxSignedURL)); got.Outcome != "response_limit" || got.Hostname != "" {
		t.Fatal("unbounded signed URL")
	}
}

type vendorOnlyMapTransport struct {
	base     http.RoundTripper
	host     string
	t        *testing.T
	external *atomic.Int32
}

func (v vendorOnlyMapTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != v.host {
		v.external.Add(1)
		v.t.Error("returned URL was requested")
		return nil, errors.New("forbidden_test_destination")
	}
	return v.base.RoundTrip(r)
}

func TestSavedMapURLUsesOneFixedSignedVendorReadWithoutBlobRequest(t *testing.T) {
	const reference = "synthetic-list-reference,synthetic-key-MUST_NOT_ESCAPE"
	var signedCalls, external atomic.Int32
	c, server := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case listPath:
			io.WriteString(w, deviceList)
		case historyPath:
			io.WriteString(w, syntheticHistory())
		case "/dreame-iot-com-shard-1/device/sendCommand":
			assertFixedMapDiagnosticRPC(t, r)
			descriptor, _ := json.Marshal(map[string]string{"object_name": reference, "md5": strings.Repeat("a", 32)})
			io.WriteString(w, syntheticMapProperties("", string(descriptor)))
		case mapDownloadURLPath:
			signedCalls.Add(1)
			if r.Method != "POST" || r.Header.Get("dreame-auth") != "synthetic-access-token" {
				t.Error("signed vendor request lost authenticated origin")
			}
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 6 || body["did"] != "fixture-vacuum-1" || body["model"] != "dreame.vacuum.synthetic" || body["filename"] != reference || body["region"] != "eu" || body["uid"] != nil {
				t.Fatal("wrong filename/model/region/endpoint contract")
			}
			actualSign := body["sign"]
			delete(body, "sign")
			delete(body, "timestamp")
			if actualSign != sign(body, 1700000000123, "synthetic-key-16!")["sign"] {
				t.Fatal("signed locator request differs from pinned flow")
			}
			io.WriteString(w, `{"code":0,"data":"https://storage.example.com/private-MUST_NOT_ESCAPE?signature=MUST_NOT_ESCAPE"}`)
		default:
			t.Error("unexpected endpoint or storage request")
		}
	})
	u, _ := url.Parse(server.URL)
	c.http.Transport = vendorOnlyMapTransport{base: c.http.Transport, host: u.Host, t: t, external: &external}
	r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"})
	if !r.OK {
		t.Fatal(r.Error)
	}
	m := r.Data.(MapMetadata)
	s := m.Diagnostics.SavedObject
	if signedCalls.Load() != 1 || external.Load() != 0 || s.Outcome != "metadata_available" || s.Hostname != "storage.example.com" || s.FetchPerformed || m.ActiveMapVerified || len(m.Maps) != 0 {
		t.Fatal("locator read scope or unverified flags changed")
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "MUST_NOT_ESCAPE") || strings.Contains(string(raw), "synthetic-list-reference") || strings.Contains(string(raw), "synthetic-access-token") {
		t.Fatal("locator/reference/token leaked")
	}
}

func TestSavedMapURLFailuresAreBoundedAndDoNotRetryOrHideAuth(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		body, outcome string
		auth          bool
	}{
		{"wrong_data_shape", 200, `{"code":0,"data":{"url":"private"}}`, "response_invalid", false},
		{"limit", 200, strings.Repeat("x", maxSignedURLResponse+1), "response_limit", false},
		{"redirect", 302, `private`, "request_unavailable", false},
		{"service", 503, `private`, "request_unavailable", false},
		{"unauthenticated", 401, `private`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case listPath:
					io.WriteString(w, deviceList)
				case historyPath:
					io.WriteString(w, syntheticHistory())
				case "/dreame-iot-com-shard-1/device/sendCommand":
					io.WriteString(w, syntheticMapProperties("", `{"object_name":"synthetic-private-reference"}`))
				case mapDownloadURLPath:
					calls.Add(1)
					w.Header().Set("Location", "https://untrusted.invalid/never-follow")
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.body)
				default:
					t.Error("unexpected destination or retry")
				}
			})
			r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"})
			if calls.Load() != 1 {
				t.Fatal("signed URL request retried")
			}
			if tc.auth {
				if r.OK || r.Error == nil || r.Error.Code != "session_reauthentication_required" {
					t.Fatal("locator auth failure hidden")
				}
				return
			}
			if !r.OK || r.Data.(MapMetadata).Diagnostics.SavedObject.Outcome != tc.outcome {
				t.Fatal("locator failure not safely classified")
			}
		})
	}
}

func TestAmbiguousSavedMapReferenceMakesNoLocatorRequest(t *testing.T) {
	c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case listPath:
			io.WriteString(w, deviceList)
		case historyPath:
			io.WriteString(w, syntheticHistory())
		case "/dreame-iot-com-shard-1/device/sendCommand":
			io.WriteString(w, syntheticMapProperties("", `{"object_name":"first","obj_name":"second"}`))
		default:
			t.Error("ambiguous reference used for request")
		}
	})
	r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"})
	if !r.OK || r.Data.(MapMetadata).Diagnostics.SavedObject.Outcome != "reference_invalid" {
		t.Fatal("ambiguous reference accepted")
	}
}
