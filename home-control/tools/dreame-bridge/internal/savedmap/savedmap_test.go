package savedmap

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// All map data, IDs, names, keys and IVs in these tests are synthetic.
func frameBytes(pixels []byte, width, height int, tail string) []byte {
	raw := make([]byte, headerSize)
	binary.LittleEndian.PutUint16(raw[0:2], 42)
	binary.LittleEndian.PutUint16(raw[2:4], 7)
	raw[4] = 'I'
	binary.LittleEndian.PutUint16(raw[19:21], uint16(width))
	binary.LittleEndian.PutUint16(raw[21:23], uint16(height))
	raw = append(raw, pixels...)
	return append(raw, tail...)
}

func compress(t *testing.T, raw []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func frameText(t *testing.T, pixels []byte, width, height int, tail string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(compress(t, frameBytes(pixels, width, height, tail)))
}

func encryptedText(t *testing.T, raw []byte, key string, iv []byte) string {
	t.Helper()
	data := compress(t, raw)
	// Deliberately non-PKCS trailing bytes. The source does not unpad.
	for len(data)%aes.BlockSize != 0 {
		data = append(data, 0xa5)
	}
	block, err := aes.NewCipher(deriveKey(key))
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(data, data)
	return base64.URLEncoding.EncodeToString(data)
}

func expectCode(t *testing.T, err error, code Code) {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != code || err.Error() != string(code) {
		t.Fatalf("expected fixed code %q, received error type %T", code, err)
	}
}

func TestParseListInlineReferenceAndSelection(t *testing.T) {
	list, err := ParseList([]byte(`{"curr_id":"0042","server":1,"mapstr":[{"rismobj":"synthetic-ref,key","mapobj":"saved-ref","angle":"90","name":"Synthetic floor"},{"map":"frame","rismobj":"also-kept"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if list.CurrentID != "42" || list.Server != 1 || len(list.Entries) != 2 {
		t.Fatal("list selection metadata mismatch")
	}
	entry := list.Entries[0]
	if entry.EncodedFrame != "" || entry.ObjectReference != "synthetic-ref,key" || entry.MapObjectReference != "saved-ref" || entry.Angle != 90 {
		t.Fatal("reference branch changed opaque data")
	}
	if list.Entries[1].EncodedFrame != "frame" || list.Entries[1].ObjectReference != "also-kept" {
		t.Fatal("inline precedence fields mismatch")
	}
	missing, err := ParseList([]byte(`{"mapstr":[]}`))
	if err != nil || missing.CurrentID != "" {
		t.Fatal("missing current identifier must remain unknown")
	}
	unicodeReference, err := ParseList([]byte(`{"server":1,"mapstr":[{"rismobj":"ref\ud83d\ude80"}]}`))
	if err != nil || unicodeReference.Entries[0].ObjectReference != "ref🚀" {
		t.Fatal("valid surrogate pair was changed")
	}
}

func TestParseListInvalid(t *testing.T) {
	cases := []struct {
		name, text string
		code       Code
	}{
		{"duplicate top", `{"mapstr":[],"curr_id":1,"curr_id":2}`, CodeDuplicateField},
		{"duplicate unknown", `{"mapstr":[],"other":{"x":1,"x":2}}`, CodeDuplicateField},
		{"duplicate escaped", `{"mapstr":[],"map\u0073tr":[]}`, CodeDuplicateField},
		{"duplicate entry", `{"mapstr":[{"map":"one","map":"two"}]}`, CodeDuplicateField},
		{"missing array", `{}`, CodeInvalidList},
		{"null array", `{"mapstr":null}`, CodeInvalidList},
		{"array element", `{"mapstr":[null]}`, CodeInvalidList},
		{"frame type", `{"mapstr":[{"map":1}]}`, CodeInvalidList},
		{"reference server", `{"mapstr":[{"rismobj":"ref"}]}`, CodeInvalidList},
		{"lone surrogate reference", `{"server":1,"mapstr":[{"rismobj":"ref\ud800"}]}`, CodeInvalidList},
		{"empty reference", `{"server":1,"mapstr":[{"rismobj":""}]}`, CodeInvalidList},
		{"numeric current", `{"curr_id":1.5,"mapstr":[]}`, CodeInvalidList},
		{"current range", `{"curr_id":32768,"mapstr":[]}`, CodeInvalidList},
		{"angle type", `{"mapstr":[{"map":"x","angle":true}]}`, CodeInvalidList},
		{"trailing JSON", `{"mapstr":[]}[]`, CodeInvalidJSON},
		{"unknown nesting", `{"mapstr":[],"a":` + strings.Repeat("[", 50) + `0` + strings.Repeat("]", 50) + `}`, CodeLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { _, err := ParseList([]byte(tc.text)); expectCode(t, err, tc.code) })
	}
	_, err := ParseList(make([]byte, MaxListBytes+1))
	expectCode(t, err, CodeLimit)
	_, err = ParseList([]byte(`{"mapstr":[` + strings.TrimSuffix(strings.Repeat(`{"map":"x"},`, 33), ",") + `]}`))
	expectCode(t, err, CodeLimit)
	_, err = ParseList([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})
	expectCode(t, err, CodeInvalidJSON)
}

func TestDecodeVisibleMappedRooms(t *testing.T) {
	name := base64.StdEncoding.EncodeToString([]byte("Тестовая комната"))
	tail := `{"delsr":[2],"seg_inf":{"1":{"name":"` + name + `","type":4,"index":2,"roomID":9007199254740993},"2":{"name":"SGlkZGVu"},"3":{"type":2,"index":1},"7":{"name":"VW5tYXBwZWQ="}}}`
	frame, err := DecodeFrame(frameText(t, []byte{1, 2, 3, 0x81}, 4, 1, tail), "", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if frame.MapID != "42" || frame.FrameID != 7 || frame.Version != 1 || !frame.Saved || frame.SavedMapID != "" {
		t.Fatal("header or saved status mismatch")
	}
	if len(frame.Rooms) != 2 || frame.Rooms[0].ID != 1 || frame.Rooms[1].ID != 3 {
		t.Fatal("rooms must be sorted, visible and pixel-mapped")
	}
	if frame.Rooms[0].Name != "Тестовая комната" || frame.Rooms[0].UniqueID != "9007199254740993" || frame.Rooms[0].Type != 4 || frame.Rooms[0].Index != 2 {
		t.Fatal("custom room metadata mismatch")
	}
	if frame.Rooms[1].Name != "Primary Bedroom 2" {
		t.Fatal("source default naming mismatch")
	}
	optional, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, `{"seg_inf":{"1":{"type":null,"index":null,"roomID":null,"name":null}}}`), "", nil, 1)
	if err != nil || len(optional.Rooms) != 1 || optional.Rooms[0].Name != "Room 1" {
		t.Fatal("nullable optional room metadata mismatch")
	}
}

func TestPixelClassificationBySourceBranch(t *testing.T) {
	cases := []struct {
		name    string
		version int
		pixels  []byte
		tail    string
		want    []int
		saved   bool
	}{
		{"v0 saved", 0, []byte{1, 2, 63, 0x81}, `{}`, []int{1, 2, 63}, true},
		{"v1 wall normalization", 1, []byte{1, 0x81, 0x80, 0xc2}, `{}`, []int{1, 2}, true},
		{"v2", 2, []byte{1, 3}, `{}`, []int{1, 3}, true},
		{"frame map", 1, []byte{4, 11, 240, 244, 248, 252, 2, 3}, `{"fsm":1}`, []int{1, 2, 60}, false},
		{"status zero", 1, []byte{1, 2, 3, 4, 255}, `{"ris":0}`, nil, false},
		{"status one", 1, []byte{1, 2, 3, 4, 255}, `{"ris":1}`, nil, false},
		{"status two", 1, []byte{1, 2, 3, 4, 255}, `{"ris":2}`, nil, false},
		{"v0 current", 0, []byte{1, 2, 3, 4, 255}, `{"rpur":1}`, nil, false},
		{"v0 recovery", 0, []byte{1, 2, 3}, `{"rpur":1,"us":1}`, []int{1, 2, 3}, false},
		{"v3", 3, []byte{1, 0x21, 0x42, 0x63, 0x9e, 31, 0x3f, 0x7f}, `{}`, []int{1, 2, 30}, true},
		{"v3 ignores old status branch", 3, []byte{1, 2}, `{"ris":1}`, []int{1, 2}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := DecodeFrame(frameText(t, tc.pixels, len(tc.pixels), 1, tc.tail), "", nil, tc.version)
			if err != nil {
				t.Fatal(err)
			}
			if len(frame.Rooms) != len(tc.want) || frame.Saved != tc.saved {
				t.Fatal("source branch mismatch")
			}
			for i, id := range tc.want {
				if frame.Rooms[i].ID != id {
					t.Fatal("segment identity mismatch")
				}
			}
		})
	}
}

func TestEncryptionAndSuffixSeparation(t *testing.T) {
	iv := []byte("synthetic-iv-123")
	key := "synthetickey"
	encoded := encryptedText(t, frameBytes([]byte{1}, 1, 1, `{}`), key, iv)
	for _, text := range []string{encoded + "," + key, encoded + "," + key + ",ignored"} {
		frame, err := DecodeFrame(text, "", iv, 1)
		if err != nil || len(frame.Rooms) != 1 {
			t.Fatal("suffix-key CBC decode failed")
		}
	}
	frame, err := DecodeFrame(encoded, key, iv, 1)
	if err != nil || frame.MapID != "42" {
		t.Fatal("separate-key decode failed")
	}
	_, err = DecodeFrame(encoded+","+key, key, iv, 1)
	expectCode(t, err, CodeInvalidBase64)
	_, err = DecodeFrame(encoded+","+key, "", nil, 1)
	expectCode(t, err, CodeInvalidCrypto)
	_, err = DecodeFrame(encoded, key, []byte("short"), 1)
	expectCode(t, err, CodeInvalidCrypto)
	_, err = DecodeFrame(base64.StdEncoding.EncodeToString([]byte("not a CBC block")), key, iv, 1)
	expectCode(t, err, CodeInvalidCrypto)
	_, err = DecodeFrame(encoded, "wrong-synthetic-key", iv, 1)
	expectCode(t, err, CodeInvalidCompression)
	if string(deriveKey("abc")) != "ba7816bf8f01cfea414140de5dae2223" {
		t.Fatal("SHA-256 hex-text derivation mismatch")
	}
	// Upstream URL replacement affects an embedded suffix before extraction,
	// while an explicitly supplied key remains unchanged.
	normalized := encryptedText(t, frameBytes([]byte{1}, 1, 1, `{}`), "suffix+key/with", iv)
	_, err = DecodeFrame(normalized+",suffix-key_with", "", iv, 1)
	if err != nil {
		t.Fatal("source suffix normalization mismatch")
	}
	literal := encryptedText(t, frameBytes([]byte{1}, 1, 1, `{}`), "suffix-key_with", iv)
	_, err = DecodeFrame(literal, "suffix-key_with", iv, 1)
	if err != nil {
		t.Fatal("explicit key was normalized")
	}
}

func TestIndependentOpenSSLVector(t *testing.T) {
	// Generated with Python stdlib zlib and OpenSSL 3.0.13 enc -aes-256-cbc
	// -nopad. Key is the first 32 SHA256("abc") hex-text bytes; synthetic IV
	// is "synthetic-iv-123". Non-PKCS trailing 0xa5 bytes pad the zlib stream.
	const vector = "eHscJ55ye1gsASxCOuMkaqKFWovioD0yOw60HjNNurc="
	frame, err := DecodeFrame(vector, "abc", []byte("synthetic-iv-123"), 1)
	if err != nil || frame.MapID != "42" || frame.FrameID != 7 || len(frame.Rooms) != 1 || frame.Rooms[0].ID != 1 {
		t.Fatal("independent CBC vector mismatch")
	}
}

func TestNestedSavedBindingAndVersionMarkers(t *testing.T) {
	innerRaw := frameBytes([]byte{2}, 1, 1, `{}`)
	binary.LittleEndian.PutUint16(innerRaw[0:2], 99)
	inner := base64.StdEncoding.EncodeToString(compress(t, innerRaw))
	nested, _ := json.Marshal(map[string]any{"ris": 2, "rism": inner})
	outer, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, string(nested)), "", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if outer.MapID != "42" || outer.SavedMapID != "99" || len(outer.Rooms) != 0 || outer.Saved {
		t.Fatal("nested identity must not substitute outer rooms or ID")
	}
	list, err := ParseList([]byte(`{"curr_id":42,"mapstr":[]}`))
	if err != nil || list.CurrentID == outer.SavedMapID {
		t.Fatal("synthetic selected/current mismatch lost")
	}
	for _, hint := range []int{0, 1, 2} {
		frame, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, `{"ris":2,"saveMapId":99,"cover":{}}`), "", nil, hint)
		if err != nil || frame.Version != hint || frame.SavedMapID != "" {
			t.Fatal("v3 markers must not promote partial.version")
		}
	}
	frame, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, `{"ris":2,"saveMapId":"0099"}`), "", nil, 3)
	if err != nil || frame.SavedMapID != "99" {
		t.Fatal("explicit v3 binding failed")
	}
	frame, err = DecodeFrame(frameText(t, []byte{1}, 1, 1, `{"saveMapId":99}`), "", nil, 3)
	if err != nil || !frame.Saved || frame.SavedMapID != "" {
		t.Fatal("saved v3 must not gain current binding")
	}
	frame, err = DecodeFrame(frameText(t, []byte{1}, 1, 1, `{"cleanareaorder":[]}`), "", nil, 1)
	if err != nil || frame.Version != 2 {
		t.Fatal("source v1 to v2 promotion missing")
	}
	frame, err = DecodeFrame(frameText(t, []byte{1}, 1, 1, string(nested)), "", nil, 3)
	if err != nil || frame.SavedMapID != "" {
		t.Fatal("missing v3 saveMapId must clear rism binding")
	}
	for level := 0; level < 3; level++ {
		tail, _ := json.Marshal(map[string]any{"ris": 2, "rism": inner})
		inner = frameText(t, []byte{1}, 1, 1, string(tail))
	}
	_, err = DecodeFrame(inner, "", nil, 1)
	expectCode(t, err, CodeLimit)
	// Nested source call does not forward the outer IV to an encrypted rism.
	encInner := encryptedText(t, innerRaw, "nested-key", []byte("synthetic-iv-123")) + ",nested-key"
	tail, _ := json.Marshal(map[string]any{"ris": 2, "rism": encInner})
	_, err = DecodeFrame(frameText(t, []byte{1}, 1, 1, string(tail)), "", []byte("synthetic-iv-123"), 1)
	expectCode(t, err, CodeInvalidCrypto)
}

func TestMalformedFramesAndMetadata(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		hint int
		code Code
	}{
		{"short", []byte{1, 2}, 1, CodeInvalidFrame},
		{"negative dimension", frameBytes(nil, -1, 1, `{}`), 1, CodeInvalidFrame},
		{"pixel product", frameBytes(nil, 32767, 32767, `{}`), 1, CodeLimit},
		{"truncated pixels", frameBytes(nil, 10, 10, `{}`), 1, CodeInvalidFrame},
		{"missing tail", frameBytes([]byte{1}, 1, 1, ``), 1, CodeInvalidJSON},
		{"tail array", frameBytes([]byte{1}, 1, 1, `[]`), 1, CodeInvalidJSON},
		{"duplicate metadata", frameBytes([]byte{1}, 1, 1, `{"ris":1,"ris":2}`), 1, CodeDuplicateField},
		{"duplicate room", frameBytes([]byte{1}, 1, 1, `{"seg_inf":{"1":{"name":"YQ==","name":"Yg=="}}}`), 1, CodeDuplicateField},
		{"aliased segment", frameBytes([]byte{1}, 1, 1, `{"seg_inf":{"1":{},"01":{}}}`), 1, CodeDuplicateField},
		{"bad segment key", frameBytes([]byte{1}, 1, 1, `{"seg_inf":{"not-an-id":{}}}`), 1, CodeInvalidMetadata},
		{"bad hidden type", frameBytes([]byte{1}, 1, 1, `{"delsr":"1"}`), 1, CodeInvalidMetadata},
		{"bad status type", frameBytes([]byte{1}, 1, 1, `{"ris":"2"}`), 1, CodeInvalidMetadata},
		{"bad room name", frameBytes([]byte{1}, 1, 1, `{"seg_inf":{"1":{"name":"/w=="}}}`), 1, CodeInvalidMetadata},
		{"bad room type", frameBytes([]byte{1}, 1, 1, `{"seg_inf":{"1":{"type":false}}}`), 1, CodeInvalidMetadata},
		{"bad unique ID", frameBytes([]byte{1}, 1, 1, `{"seg_inf":{"1":{"roomID":1.5}}}`), 1, CodeInvalidMetadata},
		{"bad v3 ID", frameBytes([]byte{1}, 1, 1, `{"ris":2,"saveMapId":{}}`), 3, CodeInvalidMetadata},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeFrame(base64.StdEncoding.EncodeToString(compress(t, tc.raw)), "", nil, tc.hint)
			expectCode(t, err, tc.code)
		})
	}
	for _, kind := range []byte{'P', 'W', '?'} {
		raw := frameBytes([]byte{1}, 1, 1, `{}`)
		raw[4] = kind
		_, err := DecodeFrame(base64.StdEncoding.EncodeToString(compress(t, raw)), "", nil, 1)
		expectCode(t, err, CodeUnsupportedFrame)
	}
	for _, hint := range []int{-1, 4, 99} {
		_, err := DecodeFrame("abc", "", nil, hint)
		expectCode(t, err, CodeUnsupportedVersion)
	}
	_, err := DecodeFrame("bad!", "", nil, 1)
	expectCode(t, err, CodeInvalidBase64)
	_, err = DecodeFrame("AAAA", "", nil, 1)
	expectCode(t, err, CodeInvalidCompression)
	_, err = DecodeFrame(strings.Repeat("A", MaxFrameBytes+1), "", nil, 1)
	expectCode(t, err, CodeLimit)
}

func TestInflationTailAndAggregateLimits(t *testing.T) {
	_, err := DecodeFrame(base64.StdEncoding.EncodeToString(compress(t, make([]byte, MaxInflatedBytes+1))), "", nil, 1)
	expectCode(t, err, CodeLimit)
	_, err = DecodeFrame(frameText(t, []byte{1}, 1, 1, `{"padding":"`+strings.Repeat("x", MaxTailBytes)+`"}`), "", nil, 1)
	expectCode(t, err, CodeLimit)
	// Directly exercise the shared remaining budget without retaining enormous
	// nested JSON fixtures. The same budget instance is passed to every rism.
	b := budget{inflated: 28, compressed: 16 << 20}
	_, err = decodeFrame(frameText(t, []byte{1}, 1, 1, `{}`), "", nil, 1, 0, &b)
	expectCode(t, err, CodeLimit)
	b = budget{inflated: 48 << 20, compressed: 1}
	_, err = decodeFrame(frameText(t, []byte{1}, 1, 1, `{}`), "", nil, 1, 0, &b)
	expectCode(t, err, CodeLimit)
}
