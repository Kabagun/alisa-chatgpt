package cloud

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"alisa-dreame-readonly/internal/bridge"
)

const deviceList = `{"code":0,"data":{"page":{"records":[{"did":"fixture-vacuum-1","masterUid":"fixture-owner","model":"dreame.vacuum.synthetic","bindDomain":"shard-1.iot.dreame.tech","customName":"Example robot","mac":"private-discarded"}]}}}`
const statusResult = `{"code":0,"data":{"result":[{"siid":2,"piid":1,"code":0,"value":6},{"siid":3,"piid":1,"code":0,"value":82}]}}`

func syntheticSession() Session {
	return Session{Schema: 1, ServerRegion: "eu", ExpiresAt: "2100-01-01T00:00:00Z", AccessToken: "synthetic-access-token", TenantID: "synthetic-tenant", Profile: Profile{UpstreamCommit: UpstreamCommit, SigningKey: "synthetic-key-16!", BasicAuthorization: "c3ludGhldGlj", UserAgent: "Synthetic/Test", Metadata: "cv=a_synthetic"}}
}

func mockClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	c, err := NewClient(syntheticSession())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	c.http.Transport = server.Client().Transport
	c.origin = server.URL
	c.now = func() time.Time { return time.UnixMilli(1700000000123) }
	t.Cleanup(func() { c.Close(); server.Close() })
	return c, server
}

func TestSigningMatchesPinnedPythonOracle(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/signing-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Params    map[string]any `json:"params"`
		Canonical string         `json:"canonical"`
		Signed    map[string]any `json:"signed"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for i, tc := range cases {
		if got := canonical(tc.Params, true); got != tc.Canonical {
			t.Fatalf("canonical case %d", i)
		}
		encoded, _ := json.Marshal(sign(tc.Params, 1700000000123, "synthetic-key-16!"))
		var got map[string]any
		json.Unmarshal(encoded, &got)
		if !reflect.DeepEqual(got, tc.Signed) {
			t.Fatalf("signature case %d", i)
		}
	}
}

func TestRealReadRequestsAgainstTLSMock(t *testing.T) {
	var calls atomic.Int32
	c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.Header.Get("dreame-auth") != "synthetic-access-token" || r.Header.Get("Authorization") != "Basic c3ludGhldGlj" || r.Header.Get("tenant-id") != "synthetic-tenant" || r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("incorrect fixed read headers")
		}
		if r.URL.Path == listPath {
			b, _ := io.ReadAll(r.Body)
			if len(b) != 0 {
				t.Error("list should not have signed parameters")
			}
			io.WriteString(w, deviceList)
			return
		}
		if r.URL.Path != "/dreame-iot-com-shard-1/device/sendCommand" {
			t.Error("unexpected endpoint")
			w.WriteHeader(400)
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid RPC body")
		}
		data, _ := body["data"].(map[string]any)
		if data["method"] != "get_properties" || len(body) != 5 || body["did"] != "fixture-vacuum-1" {
			t.Error("non-read RPC")
		}
		params, _ := data["params"].([]any)
		if len(params) != 6 {
			t.Error("wrong read selectors")
		}
		for _, p := range params {
			m, _ := p.(map[string]any)
			if len(m) != 3 || m["value"] != nil || m["aiid"] != nil {
				t.Error("write selector")
			}
		}
		actualSign := body["sign"]
		delete(body, "sign")
		delete(body, "timestamp")
		if actualSign != sign(body, 1700000000123, "synthetic-key-16!")["sign"] {
			t.Error("incorrect transmitted signature")
		}
		io.WriteString(w, statusResult)
	})
	devices := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "devices"})
	if !devices.OK || devices.Mode != "dreame_cloud" {
		t.Fatalf("devices %+v", devices.Error)
	}
	encoded, _ := json.Marshal(devices)
	if strings.Contains(string(encoded), "private-discarded") || strings.Contains(string(encoded), "synthetic-access-token") {
		t.Fatal("upstream/private fields leaked")
	}
	status := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "status", Device: "fixture-vacuum-1"})
	if !status.OK || status.Data.(bridge.Status).Values["battery_percent"] != 82 || calls.Load() != 3 {
		t.Fatalf("status %+v, calls %d", status.Error, calls.Load())
	}
}

func TestInlineCloudMapMetadata(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/synthetic.json")
	if err != nil {
		t.Fatal(err)
	}
	f, err := bridge.LoadFixture(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	encoded := f.Maps["fixture-vacuum-1"][0].Data
	array, _ := json.Marshal([]string{encoded})
	history, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"list": []any{map[string]any{"value": string(array), "time": 1700000000}}}})
	c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == listPath {
			io.WriteString(w, deviceList)
			return
		}
		if r.URL.Path == "/dreame-iot-com-shard-1/device/sendCommand" {
			io.WriteString(w, `{"code":0,"data":{"result":[{"siid":6,"piid":3,"code":-1},{"siid":6,"piid":8,"code":-1}]}}`)
			return
		}
		if r.URL.Path != historyPath {
			t.Error("non-history request")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["siid"] != "6" || body["piid"] != "1" || body["uid"] != "fixture-owner" || body["limit"] != float64(20) || len(body) != 10 {
			t.Error("nonfixed history selectors")
		}
		w.Write(history)
	})
	r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"})
	if !r.OK {
		t.Fatal(r.Error)
	}
	m := r.Data.(MapMetadata)
	if m.ActiveMapVerified || len(m.Maps) != 1 || m.Maps[0].Active || m.Maps[0].MapVersion != 0 || m.Maps[0].VerifiedForControl || !m.Maps[0].MetadataOnly || len(m.Maps[0].Items) != 2 {
		t.Fatalf("map flags %+v", m)
	}
}

func TestDeniedInputsMakeNoNetworkCalls(t *testing.T) {
	var calls atomic.Int32
	c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) })
	for _, req := range []bridge.Request{{Schema: 1, Op: "clean"}, {Schema: 1, Op: "shell"}, {Schema: 1, Op: "send"}, {Schema: 1, Op: "set_properties"}, {Schema: 1, Op: "status", Device: "https://example.test"}, {Schema: 1, Op: "devices", Device: "arbitrary"}, {Schema: 1, Op: "rooms", Device: "fixture-vacuum-1", MapID: -1}} {
		if c.Execute(context.Background(), req).OK {
			t.Fatal("denied request accepted")
		}
	}
	for _, p := range []string{"/dreame-auth/oauth/token", "/dreame-user-iot/iotuserdata/setDeviceData", "https://example.test", "/dreame-iot-com-a/../device/sendCommand"} {
		if _, err := c.post(context.Background(), p, nil); err == nil {
			t.Fatal("endpoint accepted")
		}
	}
	if _, err := rpcPath("a/../b.iot.dreame.tech"); err == nil {
		t.Fatal("unsafe shard accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("denied input performed network calls")
	}
}

func TestRedirectNeverForwardsCredentials(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(302)
	})
	r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "devices"})
	if r.OK || r.Error.Code != "cloud_redirect_denied" || destinationCalls.Load() != 0 {
		t.Fatal("redirect was not blocked")
	}
}

func TestTLSVerificationCannotBeDisabledBySession(t *testing.T) {
	c, server := mockClient(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, deviceList) })
	secure, err := NewClient(syntheticSession())
	if err != nil {
		t.Fatal(err)
	}
	defer secure.Close()
	secure.origin = server.URL // Test-only destination, deliberately without trusting its certificate.
	if secure.Execute(context.Background(), bridge.Request{Schema: 1, Op: "devices"}).OK {
		t.Fatal("untrusted TLS accepted")
	}
	transport := secure.http.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 || transport.MaxResponseHeaderBytes != 16<<10 {
		t.Fatal("unsafe production transport")
	}
	_ = c
}

func TestHTTPFailuresAreBoundedAndRedacted(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		encoding string
		expected string
	}{
		{"unauthorized", 401, "private-server-error synthetic-access-token", "", "session_reauthentication_required"},
		{"forbidden", 403, "private-server-error", "", "session_reauthentication_required"},
		{"ratelimit", 429, "private-server-error", "", "cloud_rate_limited"},
		{"error", 500, "private-server-error", "", "cloud_http_failed"},
		{"badjson", 200, "private-server-error", "", "cloud_response_invalid"},
		{"apicode", 200, `{"code":401,"detail":"private-server-error"}`, "", "session_reauthentication_required"},
		{"oversize", 200, strings.Repeat("x", maxResponse+1), "", "cloud_response_limit"},
		{"gzip", 200, "private-server-error", "gzip", "cloud_encoding_unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "devices"})
			if r.OK || r.Error.Code != tc.expected {
				t.Fatalf("result %+v", r.Error)
			}
			encoded, _ := json.Marshal(r)
			if strings.Contains(string(encoded), "private-server-error") || strings.Contains(string(encoded), "synthetic-access-token") || strings.Contains(string(encoded), c.origin) {
				t.Fatal("error leaked details")
			}
		})
	}
}

func TestDeadlineAndExpiredSession(t *testing.T) {
	var calls atomic.Int32
	c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r := c.Execute(ctx, bridge.Request{Schema: 1, Op: "devices"})
	if r.OK || r.Error.Code != "cloud_timeout" {
		t.Fatal("deadline ignored")
	}
	c.session.ExpiresAt = "2000-01-01T00:00:00Z"
	before := calls.Load()
	r = c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "devices"})
	if r.OK || r.Error.Code != "session_expired" || calls.Load() != before {
		t.Fatal("expired session transmitted")
	}
}

func TestProtectedSessionContract(t *testing.T) {
	s := syntheticSession()
	encoded, _ := json.Marshal(s)
	if _, err := ParseSession(strings.NewReader(string(encoded)), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Session){
		func(s *Session) { s.ServerRegion = "de" }, func(s *Session) { s.ServerRegion = "example.test" },
		func(s *Session) { s.AccessToken = "token\r\nInjected: true" }, func(s *Session) { s.Profile.UpstreamCommit = "untrusted" },
		func(s *Session) { s.Profile.BasicAuthorization = "@@" }, func(s *Session) { s.Profile.Locale = "@@" },
		func(s *Session) { s.ExpiresAt = "expired" }, func(s *Session) { s.ExpiresAt = "2000-01-01T00:00:00Z" },
	} {
		value := s
		mutate(&value)
		if value.Validate(time.Now()) == nil {
			t.Fatal("invalid session accepted")
		}
	}
	if strings.Contains(s.String(), s.AccessToken) || strings.Contains(s.Profile.String(), s.Profile.SigningKey) {
		t.Fatal("diagnostic session exposure")
	}
	if _, err := ParseSession(strings.NewReader(strings.Repeat(" ", MaxSession+1)), time.Now()); err == nil {
		t.Fatal("session limit ignored")
	}
	if _, err := ParseSession(strings.NewReader(`{"schema":1,"password":"never accepted"}`), time.Now()); err == nil {
		t.Fatal("password import accepted")
	}
	for region, origin := range origins {
		if !strings.HasPrefix(origin, "https://"+region+".iot.dreame.tech:13267") {
			t.Fatal("nonfixed host")
		}
	}
}
