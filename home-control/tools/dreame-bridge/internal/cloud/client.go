package cloud

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"alisa-dreame-readonly/internal/bridge"
)

const maxResponse = 2 << 20
const listPath = "/dreame-user-iot/iotuserbind/device/listV2"
const historyPath = "/dreame-user-iot/iotstatus/history"
const mapDownloadURLPath = "/dreame-user-iot/iotfile/getDownloadUrl"
const callTimeout = 6 * time.Second
const operationTimeout = 20 * time.Second

type Client struct {
	session          Session
	origin           string
	http             *http.Client
	now              func() time.Time
	mapProfile       MapReadProfile
	mapBlobs         *mapBlobReader
	mapPollWait      func(context.Context, time.Duration) error
	controlStore     controlStore
	controlPreparing bool
}

func NewClient(s Session) (*Client, error) {
	if err := s.Validate(time.Now()); err != nil {
		return nil, err
	}
	return &Client{session: s, origin: origins[s.ServerRegion], now: time.Now, http: newHTTPClient()}, nil
}

func newHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy: nil, DialContext: (&net.Dialer{Timeout: callTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: callTimeout,
		ResponseHeaderTimeout: callTimeout, MaxResponseHeaderBytes: 16 << 10,
		DisableCompression: true, MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1,
		IdleConnTimeout: 10 * time.Second, ForceAttemptHTTP2: false,
	}
	return &http.Client{
		Transport: transport, Timeout: callTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

// canonical reproduces _spliced from the pinned source for the small fixed set
// of request types generated below. Lists inside nested maps are omitted by the
// upstream signing algorithm; they remain present in the transmitted JSON body.
func canonical(obj map[string]any, top bool) string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := obj[k]
		switch x := v.(type) {
		case map[string]any:
			inner := canonical(x, false)
			if inner == "" {
				parts = append(parts, k+"=]")
			} else {
				parts = append(parts, k+"=["+inner+"]")
			}
		case []any, []map[string]any:
			if top {
				parts = append(parts, k+"="+compactJSON(v))
			}
		case nil:
			parts = append(parts, k+"=null")
		default:
			if top {
				if text, ok := v.(string); ok {
					parts = append(parts, k+"="+text)
				} else {
					parts = append(parts, k+"="+compactJSON(v))
				}
			} else {
				parts = append(parts, k+"="+compactJSON(v))
			}
		}
	}
	return strings.Join(parts, "&")
}

func compactJSON(value any) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(value)
	return strings.TrimSuffix(b.String(), "\n")
}

func sign(params map[string]any, timestamp int64, key string) map[string]any {
	base := canonical(params, true) + strconv.FormatInt(timestamp, 10) + key
	h := md5.Sum([]byte(base))
	out := make(map[string]any, len(params)+2)
	for k, v := range params {
		out[k] = v
	}
	out["sign"] = hex.EncodeToString(h[:])
	out["timestamp"] = timestamp
	return out
}

func shardOK(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return s[0] != '-' && s[len(s)-1] != '-'
}

func rpcPath(domain string) (string, error) {
	if domain == "" {
		return "/dreame-iot-com/device/sendCommand", nil
	}
	shard := strings.SplitN(domain, ".", 2)[0]
	if !shardOK(shard) {
		return "", sessionError("device_routing_unavailable")
	}
	return "/dreame-iot-com-" + shard + "/device/sendCommand", nil
}

func allowedPath(path string) bool {
	if path == listPath || path == historyPath || path == mapDownloadURLPath || path == "/dreame-iot-com/device/sendCommand" {
		return true
	}
	if !strings.HasPrefix(path, "/dreame-iot-com-") || !strings.HasSuffix(path, "/device/sendCommand") {
		return false
	}
	return shardOK(strings.TrimSuffix(strings.TrimPrefix(path, "/dreame-iot-com-"), "/device/sendCommand"))
}

func (c *Client) post(ctx context.Context, path string, params map[string]any) ([]byte, error) {
	return c.postBounded(ctx, path, params, maxResponse)
}

func (c *Client) postBounded(ctx context.Context, path string, params map[string]any, responseLimit int) ([]byte, error) {
	if !allowedPath(path) || responseLimit < 1 || responseLimit > maxResponse {
		return nil, sessionError("endpoint_denied")
	}
	if err := c.session.Validate(c.now()); err != nil {
		return nil, err
	}
	var body io.Reader
	if params != nil {
		payload := compactJSON(sign(params, c.now().UnixMilli(), c.session.Profile.SigningKey))
		if len(payload) > 16<<10 {
			return nil, sessionError("request_limit")
		}
		body = strings.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+path, body)
	if err != nil {
		return nil, sessionError("cloud_request_failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", c.session.Profile.UserAgent)
	req.Header.Set("dreame-meta", c.session.Profile.Metadata)
	req.Header.Set("Authorization", "Basic "+c.session.Profile.BasicAuthorization)
	req.Header.Set("tenant-id", c.session.TenantID)
	req.Header.Set("dreame-auth", c.session.AccessToken)
	if c.session.Profile.Locale != "" {
		req.Header.Set("dreame-rlc", c.session.Profile.Locale)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, sessionError("cloud_timeout")
		}
		return nil, sessionError("cloud_transport_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, sessionError("cloud_redirect_denied")
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, sessionError("session_reauthentication_required")
	}
	if resp.StatusCode == 429 {
		return nil, sessionError("cloud_rate_limited")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, sessionError("cloud_http_failed")
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return nil, sessionError("cloud_encoding_unsupported")
	}
	if resp.ContentLength > int64(responseLimit) {
		return nil, sessionError("cloud_response_limit")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(responseLimit)+1))
	if err != nil || len(raw) > responseLimit {
		return nil, sessionError("cloud_response_limit")
	}
	var envelope struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Code == nil {
		return nil, sessionError("cloud_response_invalid")
	}
	if *envelope.Code == 401 || *envelope.Code == 403 {
		return nil, sessionError("session_reauthentication_required")
	}
	if *envelope.Code != 0 {
		return nil, sessionError("cloud_api_failed")
	}
	return raw, nil
}

type deviceRecord struct {
	DID    json.RawMessage `json:"did"`
	UID    json.RawMessage `json:"masterUid"`
	Domain string          `json:"bindDomain"`
	Model  string          `json:"model"`
}

func idString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var n json.Number
		if json.Unmarshal(raw, &n) != nil {
			return ""
		}
		s = string(n)
	}
	if len(s) == 0 || len(s) > 64 {
		return ""
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-' || r == '_' || r == '.') {
			return ""
		}
	}
	return s
}

func (c *Client) devices(ctx context.Context) ([]bridge.Device, []deviceRecord, error) {
	raw, err := c.post(ctx, listPath, nil)
	if err != nil {
		return nil, nil, err
	}
	public, err := bridge.ParseDevices(raw)
	if err != nil {
		return nil, nil, sessionError("cloud_devices_invalid")
	}
	var env struct {
		Data struct {
			Page struct {
				Records []deviceRecord `json:"records"`
			} `json:"page"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return nil, nil, sessionError("cloud_devices_invalid")
	}
	return public, env.Data.Page.Records, nil
}

func validateRequest(req bridge.Request) error {
	if req.Schema != 1 {
		return sessionError("unsupported_schema")
	}
	if req.Op != "capabilities" && req.Op != "devices" && req.Op != "status" && req.Op != "rooms" {
		return sessionError("operation_denied")
	}
	if (req.Op == "devices" || req.Op == "capabilities") && (req.Device != "" || req.MapID != 0) {
		return sessionError("unexpected_selector")
	}
	if req.Op != "rooms" && req.MapID != 0 {
		return sessionError("unexpected_selector")
	}
	if (req.Op == "status" || req.Op == "rooms") && idString(json.RawMessage(strconv.Quote(req.Device))) == "" {
		return sessionError("invalid_device_id")
	}
	if req.MapID < 0 || req.MapID > 32767 {
		return sessionError("invalid_map_id")
	}
	return nil
}

func (c *Client) Execute(ctx context.Context, req bridge.Request) bridge.Response {
	r := bridge.Response{Schema: 1, Mode: "dreame_cloud", ReadOnly: true}
	data, err := c.execute(ctx, req)
	if err != nil {
		var f *bridge.Fault
		if errors.As(err, &f) {
			r.Error = f
		} else {
			r.Error = &bridge.Fault{Code: "cloud_read_failed"}
		}
	} else {
		r.OK = true
		r.Data = data
	}
	return r
}

func (c *Client) execute(ctx context.Context, req bridge.Request) (any, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	if req.Op == "capabilities" {
		return map[string]any{"operations": []string{"devices", "status", "rooms"}, "network_enabled": true, "robot_commands": false, "credential_mode": "issued_token_only", "automatic_renewal": false, "room_source": "inline_complete_history_frames_only"}, nil
	}
	timeout := operationTimeout
	if req.Op == "rooms" && c.mapProfile.enabled() {
		timeout = 24 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	catalogAt := c.now().UnixMilli()
	devices, records, err := c.devices(ctx)
	if err != nil {
		return nil, err
	}
	if req.Op == "devices" {
		return devices, nil
	}
	var selected *deviceRecord
	matches := 0
	for i := range records {
		if idString(records[i].DID) == req.Device {
			selected = &records[i]
			matches++
		}
	}
	if selected == nil {
		return nil, sessionError("device_not_found")
	}
	if req.Op == "rooms" {
		return c.rooms(ctx, req.Device, *selected, req.MapID, catalogAt, matches)
	}
	path, err := rpcPath(selected.Domain)
	if err != nil {
		return nil, err
	}
	var random [2]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, sessionError("request_id_unavailable")
	}
	id := int(binary.LittleEndian.Uint16(random[:])%100) + 2
	params := map[string]any{"did": req.Device, "id": id, "data": map[string]any{"did": req.Device, "id": id, "method": "get_properties", "params": bridge.StatusReadParams()}}
	raw, err := c.post(ctx, path, params)
	if err != nil {
		return nil, err
	}
	status, err := bridge.ParseStatus(req.Device, c.now().UTC().Format(time.RFC3339), raw)
	if err != nil {
		return nil, sessionError("cloud_status_invalid")
	}
	return status, nil
}

type MapMetadata struct {
	DeviceID           string             `json:"device_id"`
	Source             string             `json:"source"`
	ActiveMapVerified  bool               `json:"active_map_verified"`
	Maps               []bridge.Rooms     `json:"maps"`
	UnsupportedFrames  int                `json:"unsupported_frames"`
	Diagnostics        *MapDiagnostics    `json:"diagnostics,omitempty"`
	SelectedSavedMapID string             `json:"selected_saved_map_id,omitempty"`
	CleaningReadiness  *CleaningReadiness `json:"cleaning_readiness,omitempty"`
	currentProof       *privateCurrentProof
	savedFrameHash     [32]byte
	controlDrafts      privateDrafts
	ownedRecord        deviceRecord
}

func (c *Client) rooms(ctx context.Context, device string, record deviceRecord, mapID int, catalogAt int64, matches int) (any, error) {
	result := MapMetadata{DeviceID: device, Source: "cloud_history_6.1_metadata_only", Maps: []bridge.Rooms{}, Diagnostics: newMapDiagnostics(), ownedRecord: record}
	if err := c.readMapHistory(ctx, device, record, mapID, &result); err != nil {
		return nil, err
	}
	reference, err := c.readMapDiagnosticProperties(ctx, device, record.Domain, result.Diagnostics)
	if err != nil {
		return nil, err
	}
	if c.mapProfile.enabled() {
		c.readMapVersionProbe(ctx, device, record.Domain, result.Diagnostics)
	}
	locator, err := c.readSavedObjectURLMetadata(ctx, device, record.Model, reference, result.Diagnostics)
	if err != nil {
		return nil, err
	}
	if c.mapProfile.enabled() {
		result.Source = "cloud_saved_map_metadata_only"
		result.Maps = []bridge.Rooms{}
		result.Diagnostics.Schema = 3
		if err := c.readSavedMaps(ctx, device, record.Model, locator, &result); err != nil {
			return nil, err
		}
		c.refreshCurrentMap(ctx, device, record, &result)
		c.readCleaningReadiness(ctx, device, record, catalogAt, matches, &result)
		if !c.controlPreparing {
			c.saveControlPreview(device, &result)
		}
		result.Diagnostics.Schema = 4
		result.Diagnostics.SavedObject.Hostname = ""
		if len(result.Maps) > 0 {
			result.Diagnostics.Reason = "saved_metadata_available"
		} else {
			result.Diagnostics.Reason = "saved_metadata_unavailable"
		}
	} else {
		result.Diagnostics.Reason = mapDiagnosticReason(result.Diagnostics)
	}
	sort.Slice(result.Maps, func(i, j int) bool { return result.Maps[i].MapID < result.Maps[j].MapID })
	return result, nil
}
