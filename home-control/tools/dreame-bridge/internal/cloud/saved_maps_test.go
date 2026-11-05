package cloud

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"alisa-dreame-readonly/internal/bridge"
)

func savedReadFrame(t *testing.T, id int, key string) string {
	t.Helper()
	raw := make([]byte, 31)
	binary.LittleEndian.PutUint16(raw, uint16(id))
	raw[4] = 'I'
	binary.LittleEndian.PutUint16(raw[19:], 2)
	binary.LittleEndian.PutUint16(raw[21:], 2)
	raw[27] = 3
	raw[28] = 3
	name := base64.StdEncoding.EncodeToString([]byte("Synthetic kitchen"))
	raw = append(raw, []byte(`{"seg_inf":{"3":{"type":4,"name":"`+name+`","roomID":"synthetic-room"}}}`)...)
	var out bytes.Buffer
	z := zlib.NewWriter(&out)
	_, _ = z.Write(raw)
	_ = z.Close()
	data := out.Bytes()
	if key != "" {
		for len(data)%16 != 0 {
			data = append(data, 0xa5)
		}
		sum := sha256.Sum256([]byte(key))
		aesKey := []byte(hex.EncodeToString(sum[:])[:32])
		block, err := aes.NewCipher(aesKey)
		if err != nil {
			t.Fatal(err)
		}
		cipher.NewCBCEncrypter(block, []byte("synthetic-IV-123")).CryptBlocks(data, data)
		return base64.URLEncoding.EncodeToString(data) + "," + key
	}
	return base64.StdEncoding.EncodeToString(data)
}
func savedReadProperties(t *testing.T) string {
	t.Helper()
	descriptor := `{"object_name":"synthetic-list-reference","md5":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"result": []any{map[string]any{"siid": 6, "piid": 3, "code": -1}, map[string]any{"siid": 6, "piid": 8, "code": 0, "value": descriptor}}}})
	return string(raw)
}
func writeSavedReadProperties(t *testing.T, w http.ResponseWriter, r *http.Request, nonnull bool) {
	t.Helper()
	var body map[string]any
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		t.Fatal("invalid read")
	}
	data, _ := body["data"].(map[string]any)
	coreRaw, _ := json.Marshal(bridge.MapDiagnosticReadParams())
	probeRaw, _ := json.Marshal(bridge.MapSavingProbeReadParams())
	var core, probe any
	_ = json.Unmarshal(coreRaw, &core)
	_ = json.Unmarshal(probeRaw, &probe)
	if data["method"] != "get_properties" {
		t.Fatal("non-read map method")
	}
	if reflect.DeepEqual(data["params"], core) {
		io.WriteString(w, savedReadProperties(t))
		return
	}
	if !reflect.DeepEqual(data["params"], probe) {
		t.Fatal("combined/nonfixed map selectors")
	}
	result := map[string]any{"siid": 13, "piid": 1, "code": -4001}
	if nonnull {
		result["code"] = 0
		result["value"] = 0
	}
	raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"result": []any{result}}})
	w.Write(raw)
}

func TestSavedMapReadSelectsDecodedCurrentIDWithoutFloorFallback(t *testing.T) {
	for _, nonnull := range []bool{false, true} {
		t.Run(map[bool]string{false: "pinned_default", true: "nonnull_override"}[nonnull], func(t *testing.T) {
			first := savedReadFrame(t, 11, "")
			target := savedReadFrame(t, 42, "syntheticKey")
			list, _ := json.Marshal(map[string]any{"curr_id": 42, "server": 1, "mapstr": []any{map[string]any{"map": first}, map[string]any{"rismobj": "opaque-MUST_NOT_ESCAPE,reference-intact"}, map[string]any{"rismobj": "must-not-fetch-third-floor"}}})
			var blobs, signed atomic.Int32
			b, _ := syntheticBlobReader(t, func(w http.ResponseWriter, r *http.Request) {
				blobs.Add(1)
				switch r.URL.Path {
				case "/list":
					w.Write(list)
				case "/frame":
					io.WriteString(w, target)
				default:
					t.Error("unrelated floor or arbitrary object fetched")
				}
			})
			c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case listPath:
					io.WriteString(w, deviceList)
				case historyPath:
					io.WriteString(w, syntheticHistory())
				case "/dreame-iot-com-shard-1/device/sendCommand":
					writeSavedReadProperties(t, w, r, nonnull)
				case mapDownloadURLPath:
					signed.Add(1)
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["model"] != "dreame.vacuum.synthetic" || body["region"] != "eu" {
						t.Error("model/region not bound to owned catalog")
					}
					path := ""
					switch body["filename"] {
					case "synthetic-list-reference":
						path = "/list"
					case "opaque-MUST_NOT_ESCAPE,reference-intact":
						path = "/frame"
					default:
						t.Error("reference normalized or unrelated floor requested")
					}
					raw, _ := json.Marshal(map[string]any{"code": 0, "data": "https://storage.example.com" + path + "?signature=MUST_NOT_ESCAPE"})
					w.Write(raw)
				default:
					t.Error("unexpected API path or write")
				}
			})
			if err := c.SetMapReadProfile(syntheticMapProfile()); err != nil {
				t.Fatal(err)
			}
			c.mapBlobs = b
			r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"})
			if !r.OK {
				t.Fatal(r.Error)
			}
			m := r.Data.(MapMetadata)
			d := m.Diagnostics.SavedMaps
			if len(m.Maps) != 1 || m.Maps[0].MapID != 42 || m.SelectedSavedMapID != "42" || m.ActiveMapVerified || m.Maps[0].Active || m.Maps[0].VerifiedForControl || !m.Maps[0].MetadataOnly || len(m.Maps[0].Items) != 1 || m.Maps[0].Items[0].SegmentID != 3 {
				t.Fatal("saved selection/control flags wrong")
			}
			if signed.Load() != 2 || blobs.Load() != 2 || d == nil || d.Outcome != "selected_map_available" || !d.SelectedMatched || d.FramesExamined != 2 || d.FramesDecoded != 2 || d.BlobGetAttempts != 2 || d.BlobDownloads != 2 || d.ListEntries != 3 || !m.Diagnostics.SavedObject.FetchPerformed {
				t.Fatal("read budget/selection diagnostics wrong")
			}
			if nonnull {
				if d.VersionHint != 0 || d.VersionSource != "map_saving_nonnull" || d.MapSavingProbe != "value_present" {
					t.Fatal("nonnull source override lost")
				}
			} else if d.VersionHint != 1 || d.VersionSource != "model_default" || d.MapSavingProbe != "property_unavailable" {
				t.Fatal("fallback provenance missing")
			}
			raw, _ := json.Marshal(r)
			for _, private := range []string{"MUST_NOT_ESCAPE", "syntheticKey", "synthetic-IV-123", "synthetic-list-reference", "must-not-fetch-third-floor"} {
				if strings.Contains(string(raw), private) {
					t.Fatal("raw map locator/key/reference leaked")
				}
			}
		})
	}
}

func TestSavedMapMissingVersionDoesNotGuessOrDownload(t *testing.T) {
	var blobs atomic.Int32
	b, _ := syntheticBlobReader(t, func(w http.ResponseWriter, r *http.Request) { blobs.Add(1) })
	c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case listPath:
			io.WriteString(w, deviceList)
		case historyPath:
			io.WriteString(w, syntheticHistory())
		case "/dreame-iot-com-shard-1/device/sendCommand":
			writeSavedReadProperties(t, w, r, true)
		case mapDownloadURLPath:
			io.WriteString(w, `{"code":0,"data":"https://storage.example.com/list?private"}`)
		default:
			t.Error("unexpected path")
		}
	})
	p := syntheticMapProfile()
	p.ModelVersions = nil
	if err := c.SetMapReadProfile(p); err != nil {
		t.Fatal(err)
	}
	c.mapBlobs = b
	r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"})
	if !r.OK {
		t.Fatal(r.Error)
	}
	m := r.Data.(MapMetadata)
	if blobs.Load() != 0 || m.Diagnostics.SavedMaps.Outcome != "model_version_unavailable" || len(m.Maps) != 0 || m.SelectedSavedMapID != "" {
		t.Fatal("cross-model version guessed or empty selection fabricated")
	}
}

func TestSavedMapReferenceBudgetStopsWithoutFirstMapFallback(t *testing.T) {
	var records []any
	for i := 0; i < 6; i++ {
		records = append(records, map[string]any{"rismobj": "opaque-frame-reference"})
	}
	list, _ := json.Marshal(map[string]any{"curr_id": 42, "server": 1, "mapstr": records})
	frame := savedReadFrame(t, 11, "")
	b, _ := syntheticBlobReader(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/list" {
			w.Write(list)
		} else {
			io.WriteString(w, frame)
		}
	})
	var locators atomic.Int32
	c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case listPath:
			io.WriteString(w, deviceList)
		case historyPath:
			io.WriteString(w, syntheticHistory())
		case "/dreame-iot-com-shard-1/device/sendCommand":
			writeSavedReadProperties(t, w, r, false)
		case mapDownloadURLPath:
			locators.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			path := "/frame"
			if body["filename"] == "synthetic-list-reference" {
				path = "/list"
			}
			raw, _ := json.Marshal(map[string]any{"code": 0, "data": "https://storage.example.com" + path})
			w.Write(raw)
		}
	})
	_ = c.SetMapReadProfile(syntheticMapProfile())
	c.mapBlobs = b
	r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"})
	if !r.OK {
		t.Fatal(r.Error)
	}
	m := r.Data.(MapMetadata)
	d := m.Diagnostics.SavedMaps
	if d.BlobGetAttempts != 4 || d.BlobDownloads != 4 || locators.Load() != 4 || d.Outcome != "budget_exhausted" || d.SelectedMatched || len(m.Maps) != 0 || m.SelectedSavedMapID != "" {
		t.Fatal("budget exceeded or first-floor fallback invented")
	}
}
