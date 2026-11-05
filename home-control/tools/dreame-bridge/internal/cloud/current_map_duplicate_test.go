package cloud

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"

	"alisa-dreame-readonly/internal/savedmap"
)

func TestCurrentDuplicateCategoryIsTypedAndNeverDynamic(t *testing.T) {
	for _, category := range []savedmap.DuplicateCategory{savedmap.DuplicateList, savedmap.DuplicateFrameMetadata, savedmap.DuplicateFrameRooms, savedmap.DuplicateFrameVersion, savedmap.DuplicateFrameBinding, savedmap.DuplicateSegmentAlias} {
		if got := currentDuplicateCategory(&savedmap.Error{Code: savedmap.CodeDuplicateField, DuplicateCategory: category}); got != string(category) {
			t.Fatal("fixed duplicate category lost")
		}
	}
	if currentDuplicateCategory(&savedmap.Error{Code: savedmap.CodeDuplicateField, DuplicateCategory: savedmap.DuplicateCategory("private.key.path")}) != "" || currentDuplicateCategory(&savedmap.Error{Code: savedmap.CodeInvalidFrame, DuplicateCategory: savedmap.DuplicateFrameBinding}) != "" {
		t.Fatal("dynamic/nonduplicate category escaped")
	}
	raw := make([]byte, 28)
	binary.LittleEndian.PutUint16(raw, 77)
	raw[4] = 'I'
	binary.LittleEndian.PutUint16(raw[19:], 1)
	binary.LittleEndian.PutUint16(raw[21:], 1)
	raw[27] = 3
	raw = append(raw, []byte(`{"ris":2,"ris":2}`)...)
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	_, _ = z.Write(raw)
	_ = z.Close()
	c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("unexpected network request") })
	p := syntheticMapProfile()
	_ = c.SetMapReadProfile(p)
	m := MapMetadata{Diagnostics: newMapDiagnostics()}
	m.Diagnostics.SavedMaps = &SavedMapReadDiagnostics{VersionHint: 1}
	d := &CurrentMapDiagnostics{}
	if _, ok := c.decodeCurrent(context.Background(), "fixture-vacuum-1", "dreame.vacuum.synthetic", privateCurrentCandidate{inline: base64.StdEncoding.EncodeToString(compressed.Bytes())}, &m, d); ok || d.LastError != "saved_map_duplicate_field" || d.DuplicateCategory != "frame_metadata" || strings.Contains(d.DuplicateCategory, "ris") {
		t.Fatal("proof duplicate was accepted or exposed dynamic field")
	}
}
