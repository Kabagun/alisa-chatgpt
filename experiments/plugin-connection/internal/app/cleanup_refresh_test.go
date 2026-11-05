package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"smart-home-plugin/internal/oauth"
)

func discoverToolCatalog(h *Handler, token string) ([]map[string]any, error) {
	w := request(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "application/json", token)
	var result struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Result.Tools) != 4 {
		return nil, fmt.Errorf("invalid discovery response status %d", w.Code)
	}
	return result.Result.Tools, nil
}

func cleanupCatalogMatchesOriginal(original, cleanup []map[string]any) bool {
	if len(original) != len(cleanup) {
		return false
	}
	for i, tool := range cleanup {
		description, ok := tool["description"].(string)
		if !ok || !strings.HasPrefix(description, cleanupToolDescriptionPrefix) {
			return false
		}
		unmarked := make(map[string]any, len(tool))
		for key, value := range tool {
			unmarked[key] = value
		}
		unmarked["description"] = strings.TrimPrefix(description, cleanupToolDescriptionPrefix)
		// Compare every field, including names, schemas, scopes, annotations and
		// both security metadata locations. Only the description may differ.
		if !reflect.DeepEqual(original[i], unmarked) {
			return false
		}
	}
	return true
}

func TestCleanupRefreshDescriptionsAreIsolatedFromRealDiscovery(t *testing.T) {
	f := appFixture(t)
	normal := connect(t, f, oauth.ReadScope+" "+oauth.ControlScope)
	original, err := discoverToolCatalog(f.h, normal.tokens.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range original {
		if strings.HasPrefix(tool["description"].(string), cleanupToolDescriptionPrefix) {
			t.Fatal("real discovery was marked as cleanup")
		}
	}
	client, _, _ := begin(t, f, oauth.ReadScope)
	enableCleanup(t, f, client, testRedirect, f.h.now().Add(time.Hour), true)
	w := request(f.h, "GET", "/oauth/authorize?"+cleanupAutoQuery(client).Encode(), "", "", "")
	callback, err := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusSeeOther || err != nil {
		t.Fatal("cleanup activation failed")
	}
	grant := url.Values{"grant_type": {"authorization_code"}, "client_id": {client}, "code": {callback.Query().Get("code")}, "redirect_uri": {testRedirect}, "code_verifier": {testVerifier}, "resource": {testOrigin + "/mcp"}}
	w = request(f.h, "POST", "/oauth/token", grant.Encode(), "application/x-www-form-urlencoded", "")
	var cleanup oauth.Tokens
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &cleanup) != nil || cleanup.AccessToken == "" {
		t.Fatal("cleanup exchange failed")
	}
	marked, err := discoverToolCatalog(f.h, cleanup.AccessToken)
	if err != nil || !cleanupCatalogMatchesOriginal(original, marked) {
		t.Fatal("cleanup discovery changed fields beyond its description prefix")
	}
	// Interleave real and cleanup HTTP requests under the race detector to catch
	// shared catalog mutation or disclosure between identities.
	var wg sync.WaitGroup
	failures := make(chan bool, 8)
	for worker := range 8 {
		wg.Go(func() {
			for range 4 {
				token := normal.tokens.AccessToken
				if worker%2 == 0 {
					token = cleanup.AccessToken
				}
				catalog, err := discoverToolCatalog(f.h, token)
				if err != nil || worker%2 == 0 && !cleanupCatalogMatchesOriginal(original, catalog) || worker%2 != 0 && !reflect.DeepEqual(original, catalog) {
					failures <- true
					return
				}
			}
		})
	}
	wg.Wait()
	close(failures)
	if len(failures) != 0 {
		t.Fatal("concurrent cleanup discovery leaked into real discovery or changed metadata")
	}
	after, err := discoverToolCatalog(f.h, normal.tokens.AccessToken)
	if err != nil || !reflect.DeepEqual(original, after) {
		t.Fatal("real discovery changed after cleanup discovery")
	}
	for _, tool := range []struct{ name, args string }{
		{"home_devices", `{}`},
		{"home_status", `{"device_id":"synthetic-device"}`},
		{"home_rooms", `{"device_id":"synthetic-device"}`},
		{"home_clean_rooms", `{"device_id":"synthetic-device","map_id":"synthetic-map","room_ids":["1"]}`},
	} {
		w = call(f.h, cleanup.AccessToken, tool.name, tool.args)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"isError":true`) || !strings.Contains(w.Body.String(), "provider_unavailable") {
			t.Fatalf("cleanup refresh tool %s did not fail closed", tool.name)
		}
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	if f.p.logins != 1 || f.p.calls != 0 || len(f.p.accounts) != 1 {
		t.Fatal("cleanup discovery or calls reached the provider boundary")
	}
}
