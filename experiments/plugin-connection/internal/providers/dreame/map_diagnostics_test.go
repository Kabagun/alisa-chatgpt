package dreame

import (
	"encoding/json"
	"strings"
	"testing"
)

const mapDiagnosticFixture = `{"device_id":"fixture-1","source":"cloud_history_6.1_metadata_only","active_map_verified":false,"maps":[],"unsupported_frames":1,"diagnostics":{"schema":1,"reason":"encrypted_inline_unsupported","history":{"outcome":"ok","records":1,"value_shapes":{"json_array_string":1,"array":0,"object":0,"string_other":0,"null_or_missing":0,"scalar_other":0},"inline_candidates":1,"encrypted_suffix_frames":1,"full_frames":0,"incremental_frames":0,"other_frames":0,"invalid_frames":0,"decode_failures":0,"decoded_maps":0},"properties":{"outcome":"ok","object_name":{"state":"present","value_shape":"string","object_reference_present":true,"md5_present":false},"map_list":{"state":"present","value_shape":"json_object_string","object_reference_present":true,"md5_present":true}}}}`

func TestMapDiagnosticSchemaAllowsUnverifiedEmptyMaps(t *testing.T) {
	raw, err := rooms([]byte(mapDiagnosticFixture), "fixture-1")
	if err != nil || !json.Valid(raw) || !strings.Contains(string(raw), `"maps":[]`) || !strings.Contains(string(raw), `"active_map_verified":false`) {
		t.Fatal("safe diagnostics-only result rejected/promoted")
	}
	for _, unsafe := range []string{
		strings.Replace(mapDiagnosticFixture, `"schema":1`, `"schema":1,"signed_url":"MUST_NOT_ESCAPE"`, 1),
		strings.Replace(mapDiagnosticFixture, `"state":"present"`, `"state":"present","object_name":"MUST_NOT_ESCAPE"`, 1),
		strings.Replace(mapDiagnosticFixture, `"records":1`, `"records":21`, 1),
		strings.Replace(mapDiagnosticFixture, `"records":1`, `"records":-1`, 1),
		strings.Replace(mapDiagnosticFixture, `"encrypted_suffix_frames":1`, `"encrypted_suffix_frames":2`, 1),
		strings.Replace(mapDiagnosticFixture, `"unsupported_frames":1`, `"unsupported_frames":0`, 1),
		strings.Replace(mapDiagnosticFixture, `"reason":"encrypted_inline_unsupported"`, `"reason":"saved_object_required"`, 1),
		strings.Replace(mapDiagnosticFixture, `"outcome":"ok"`, `"outcome":"MUST_NOT_ESCAPE"`, 1),
		strings.Replace(mapDiagnosticFixture, `,"full_frames":0`, "", 1),
		strings.Replace(mapDiagnosticFixture, `"active_map_verified":false`, `"active_map_verified":true`, 1),
		strings.Replace(mapDiagnosticFixture, `"reason":"encrypted_inline_unsupported"`, `"reason":"`+strings.Repeat("x", 5000)+`"`, 1),
	} {
		if raw, err := rooms([]byte(unsafe), "fixture-1"); err == nil || raw != nil {
			t.Fatal("unsafe/unknown/inconsistent diagnostics accepted")
		}
	}
}

func TestDiagnosticEmptyMapsRequireProvenanceSchema(t *testing.T) {
	for _, raw := range []string{
		`{"device_id":"fixture-1","source":"cloud_history_6.1_metadata_only","active_map_verified":false,"maps":[],"unsupported_frames":0}`,
		strings.Replace(mapDiagnosticFixture, `"maps":[]`, `"maps":null`, 1),
	} {
		if _, err := rooms([]byte(raw), "fixture-1"); err == nil {
			t.Fatal("empty maps without valid diagnostics accepted")
		}
	}
}

func mapLocatorFixture(t *testing.T, saved map[string]any) string {
	t.Helper()
	var value map[string]any
	if json.Unmarshal([]byte(mapDiagnosticFixture), &value) != nil {
		t.Fatal("bad fixture")
	}
	d := value["diagnostics"].(map[string]any)
	d["schema"] = 2
	d["saved_object"] = saved
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
func validSavedObjectFixture() map[string]any {
	return map[string]any{"outcome": "metadata_available", "scheme_class": "https", "host_class": "dns_name", "port_class": "default_443", "hostname": "storage.example.com", "fetch_performed": false}
}

func TestMapLocatorMetadataHasStrictPrivateHostnameBoundary(t *testing.T) {
	valid := mapLocatorFixture(t, validSavedObjectFixture())
	raw, err := rooms([]byte(valid), "fixture-1")
	if err != nil || !strings.Contains(string(raw), `"hostname":"storage.example.com"`) {
		t.Fatal("private runtime locator metadata rejected")
	}
	for _, mutation := range []func(map[string]any){
		func(s map[string]any) { s["fetch_performed"] = true },
		func(s map[string]any) { s["scheme_class"] = "http" },
		func(s map[string]any) { s["port_class"] = "other" },
		func(s map[string]any) {
			s["hostname"] = "https://storage.example.com/private?signature=MUST_NOT_ESCAPE"
		},
		func(s map[string]any) { s["hostname"] = "127.0.0.1" },
		func(s map[string]any) { s["hostname"] = "metadata.internal" },
		func(s map[string]any) { s["hostname"] = "UPPER.EXAMPLE.COM" },
		func(s map[string]any) { s["hostname"] = strings.Repeat("x", 254) },
		func(s map[string]any) { s["query"] = "MUST_NOT_ESCAPE" },
		func(s map[string]any) { s["signed_url"] = "MUST_NOT_ESCAPE" },
		func(s map[string]any) { delete(s, "fetch_performed") },
		func(s map[string]any) { delete(s, "hostname") },
		func(s map[string]any) { s["outcome"] = "url_rejected" },
	} {
		saved := validSavedObjectFixture()
		mutation(saved)
		if output, err := rooms([]byte(mapLocatorFixture(t, saved)), "fixture-1"); err == nil || output != nil {
			t.Fatal("unsafe/unknown locator metadata accepted")
		}
	}
	for _, outcome := range []string{"not_referenced", "reference_invalid", "request_unavailable", "response_invalid", "response_limit"} {
		saved := map[string]any{"outcome": outcome, "scheme_class": "not_observed", "host_class": "not_observed", "port_class": "not_observed", "fetch_performed": false}
		if _, err := rooms([]byte(mapLocatorFixture(t, saved)), "fixture-1"); err != nil {
			t.Fatal("safe locator failure rejected")
		}
	}
	for _, unsafe := range []string{
		strings.Replace(valid, `"schema":2`, `"schema":3`, 1),
		strings.Replace(valid, `"schema":2`, `"schema":1`, 1),
		strings.Replace(valid, `"value_shape":"json_object_string"`, `"value_shape":"object"`, 1),
	} {
		if _, err := rooms([]byte(unsafe), "fixture-1"); err == nil {
			t.Fatal("locator schema/provenance mismatch accepted")
		}
	}
}
