package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"smart-home-plugin/internal/oauth"
	"smart-home-plugin/internal/provider"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (h *Handler) challenge(scope string) string {
	s := `Bearer resource_metadata="` + h.origin + `/.well-known/oauth-protected-resource"`
	if scope != "" {
		s += `, error="insufficient_scope", error_description="Authorize the required permissions to continue", scope="` + scope + `"`
	}
	return s
}
func (h *Handler) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", h.challenge(""))
	h.failure(w, 401, "unauthorized")
}
func bearer(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return ""
	}
	v := strings.TrimPrefix(values[0], "Bearer ")
	if len(v) != 64 || strings.ContainsAny(v, " ,\t\r\n") {
		return ""
	}
	return v
}
func (h *Handler) rpc(w http.ResponseWriter, id json.RawMessage, result any) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	h.json(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func (h *Handler) rpcError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	h.json(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}
func rpcIDValid(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	if len(id) > 256 {
		return false
	}
	var v any
	if json.Unmarshal(id, &v) != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}
func emptyParams(p json.RawMessage) bool {
	if len(p) == 0 {
		return true
	}
	var v map[string]json.RawMessage
	if strictJSON(p, &v) != nil || v == nil {
		return false
	}
	for key, value := range v {
		if key != "_meta" {
			return false
		}
		var metadata map[string]json.RawMessage
		if strictJSON(value, &metadata) != nil || metadata == nil {
			return false
		}
	}
	return true
}
func toolError(code string) map[string]any {
	return map[string]any{"content": []any{map[string]string{"type": "text", "text": code}}, "isError": true}
}

const cleanupToolDescriptionPrefix = "Test-only cleanup connection. Device calls are disabled. "

func tools() []any {
	var result []any
	for _, x := range []struct {
		name, description, scope string
		op                       provider.Operation
	}{
		{"home_devices", "List devices belonging to the connected provider account.", oauth.ReadScope, provider.Devices},
		{"home_status", "Read status for a device from the connected account's verified catalog.", oauth.ReadScope, provider.Status},
		{"home_rooms", "Read map rooms and a fresh single-room cleaning preview for a device from the connected account.", oauth.ReadScope, provider.Rooms},
		{"home_clean_rooms", "Start one room from a recent server-held cleaning preview after rechecking the device, active map and settings. Report accepted, started or unknown; never infer completion.", oauth.ControlScope, provider.CleanRooms},
	} {
		properties := map[string]any{}
		required := []string{}
		if x.op != provider.Devices {
			properties["device_id"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
			required = append(required, "device_id")
		}
		if x.op == provider.CleanRooms {
			properties["map_id"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
			properties["room_ids"] = map[string]any{"type": "array", "minItems": 1, "maxItems": 1, "uniqueItems": true, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}}
			required = append(required, "map_id", "room_ids")
		}
		security := []any{map[string]any{"type": "oauth2", "scopes": []string{x.scope}}}
		result = append(result, map[string]any{"name": x.name, "description": x.description, "inputSchema": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}, "annotations": map[string]any{"readOnlyHint": x.op != provider.CleanRooms, "destructiveHint": x.op == provider.CleanRooms, "idempotentHint": x.op != provider.CleanRooms, "openWorldHint": true}, "securitySchemes": security, "_meta": map[string]any{"securitySchemes": security}})
	}
	return result
}
func (h *Handler) mcp(w http.ResponseWriter, r *http.Request) {
	rawToken := bearer(r)
	identity, e := h.oauth.Authenticate(rawToken)
	if e != nil {
		h.unauthorized(w)
		return
	}
	if r.Method != "POST" {
		h.method(w, "POST")
		return
	}
	if r.URL.RawQuery != "" || !contentType(r, "application/json") {
		h.failure(w, 400, "invalid_request")
		return
	}
	b, e := readBody(r)
	if e != nil {
		h.failure(w, 413, "request_too_large")
		return
	}
	var req rpcRequest
	if strictJSON(b, &req) != nil {
		if json.Valid(b) {
			h.rpcError(w, nil, -32600, "Invalid Request")
		} else {
			h.rpcError(w, nil, -32700, "Parse error")
		}
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" || len(req.Method) > 64 || !rpcIDValid(req.ID) {
		h.rpcError(w, nil, -32600, "Invalid Request")
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch req.Method {
	case "server/discover":
		if !emptyParams(req.Params) {
			h.rpcError(w, req.ID, -32602, "Invalid params")
			return
		}
		h.rpc(w, req.ID, map[string]any{"supportedVersions": []string{"2026-07-28", "2025-11-25"}, "capabilities": map[string]any{"tools": map[string]any{}}})
	case "initialize":
		var p struct {
			ProtocolVersion string          `json:"protocolVersion"`
			Capabilities    json.RawMessage `json:"capabilities"`
			ClientInfo      json.RawMessage `json:"clientInfo"`
			Meta            json.RawMessage `json:"_meta,omitempty"`
		}
		if len(req.Params) == 0 || strictJSON(req.Params, &p) != nil || p.ProtocolVersion == "" {
			h.rpcError(w, req.ID, -32602, "Invalid params")
			return
		}
		version := p.ProtocolVersion
		if version != "2026-07-28" && version != "2025-11-25" {
			version = "2025-11-25"
		}
		h.rpc(w, req.ID, map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "smart-home-bridge", "version": "1.0.0"}})
	case "ping":
		if !emptyParams(req.Params) {
			h.rpcError(w, req.ID, -32602, "Invalid params")
			return
		}
		h.rpc(w, req.ID, map[string]any{})
	case "tools/list":
		if !emptyParams(req.Params) {
			h.rpcError(w, req.ID, -32602, "Invalid params")
			return
		}
		catalog := tools()
		if identity.Provider == cleanupProviderID {
			// tools returns fresh maps for this request; real discovery is unchanged.
			for _, entry := range catalog {
				tool := entry.(map[string]any)
				tool["description"] = cleanupToolDescriptionPrefix + tool["description"].(string)
			}
		}
		h.rpc(w, req.ID, map[string]any{"tools": catalog})
	case "tools/call":
		h.call(w, r, req, identity, rawToken)
	default:
		h.rpcError(w, req.ID, -32601, "Method not found")
	}
}
func (h *Handler) call(w http.ResponseWriter, r *http.Request, req rpcRequest, identity oauth.Identity, rawToken string) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
		Meta      json.RawMessage `json:"_meta,omitempty"`
	}
	if len(req.Params) == 0 || strictJSON(req.Params, &p) != nil {
		h.rpcError(w, req.ID, -32602, "Invalid params")
		return
	}
	var op provider.Operation
	scope := oauth.ReadScope
	switch p.Name {
	case "home_devices":
		op = provider.Devices
	case "home_status":
		op = provider.Status
	case "home_rooms":
		op = provider.Rooms
	case "home_clean_rooms":
		op = provider.CleanRooms
		scope = oauth.ControlScope
	default:
		h.rpcError(w, req.ID, -32602, "Unknown tool")
		return
	}
	if !oauth.HasScope(identity.Scope, scope) {
		result := toolError("insufficient_scope")
		result["_meta"] = map[string]any{"mcp/www_authenticate": []string{h.challenge(scope)}}
		h.rpc(w, req.ID, result)
		return
	}
	if len(p.Arguments) == 0 {
		p.Arguments = json.RawMessage("{}")
	}
	var params provider.Params
	if strings.TrimSpace(string(p.Arguments)) == "null" || strictJSON(p.Arguments, &params) != nil || params.Validate(op) != nil {
		h.rpcError(w, req.ID, -32602, "Invalid tool arguments")
		return
	}
	if identity.Provider == cleanupProviderID {
		h.rpc(w, req.ID, toolError("provider_unavailable"))
		return
	}
	pr := h.providers[identity.Provider]
	if pr == nil {
		h.rpc(w, req.ID, toolError("provider_unavailable"))
		return
	}
	h.mu.Lock()
	gate := h.operations[identity.Subject]
	if gate == nil {
		gate = make(chan struct{}, 1)
		h.operations[identity.Subject] = gate
	}
	h.mu.Unlock()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		h.rpc(w, req.ID, toolError("busy"))
		return
	}
	// Revalidate after admission; a revocation cannot leave a queued call usable.
	current, e := h.oauth.Authenticate(rawToken)
	if e != nil || current.Subject != identity.Subject {
		h.unauthorized(w)
		return
	}
	dir, e := h.store.AccountDir(identity.Subject)
	if e != nil {
		h.rpc(w, req.ID, toolError("provider_unavailable"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.operationTimeout)
	defer cancel()
	data, e := executeProvider(ctx, pr, dir, op, params)
	if e != nil || ctx.Err() != nil {
		h.rpc(w, req.ID, toolError(provider.SafeCode(e)))
		return
	}
	if len(data) == 0 || len(data) > provider.MaxResultBytes || !json.Valid(data) {
		h.rpc(w, req.ID, toolError("provider_unavailable"))
		return
	}
	h.rpc(w, req.ID, map[string]any{"content": []any{map[string]string{"type": "text", "text": string(data)}}, "isError": false})
}
