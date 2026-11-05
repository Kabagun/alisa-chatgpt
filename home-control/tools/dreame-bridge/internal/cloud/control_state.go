package cloud

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"
)

const previewTTL = 5 * time.Minute
const dispatchCooldown = 60 * time.Second

type privatePreview struct {
	Schema      int              `json:"schema"`
	Device      string           `json:"device_id"`
	Map         string           `json:"map_id"`
	Fingerprint string           `json:"fingerprint"`
	CreatedAt   int64            `json:"created_at_ms"`
	Drafts      []ReadinessDraft `json:"drafts"`
}

func (privatePreview) String() string               { return "[control preview redacted]" }
func (p privatePreview) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte(p.String())) }

type controlStore interface {
	lock(device string) (func(), error)
	load(device string) (privatePreview, error)
	save(privatePreview) error
	lastAttempt(device string) (int64, error)
	markAttempt(device string, at int64) error
}

// Only the protected session's account directory can hold control state. No
// caller-supplied snapshot/fingerprint/path is accepted through the protocol.
func (c *Client) EnableControlState(sessionPath string) error {
	if err := CheckSessionDestination(sessionPath); err != nil {
		return err
	}
	s, err := newControlStore(filepath.Dir(sessionPath))
	if err != nil {
		return err
	}
	c.controlStore = s
	return nil
}
func previewFingerprint(device string, m *MapMetadata) string {
	if m.CleaningReadiness == nil || len(m.CleaningReadiness.Drafts) == 0 || m.currentProof == nil {
		return ""
	}
	// Current frame/request IDs do not make stable saved content appear changed
	// on every refresh. This is a change guard, not vendor integrity proof.
	v := struct {
		Account, Device, Model, Map string
		Descriptor, Frame           [32]byte
		Rooms                       any
		Drafts                      any
	}{idString(m.ownedRecord.UID), device, m.ownedRecord.Model, m.SelectedSavedMapID, m.Diagnostics.descriptorHash, m.savedFrameHash, m.Maps[0].Items, m.CleaningReadiness.Drafts}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	clear(raw)
	return hex.EncodeToString(sum[:])
}
func (c *Client) saveControlPreview(device string, m *MapMetadata) {
	if c.controlStore == nil || !m.ActiveMapVerified || m.CleaningReadiness == nil || m.CleaningReadiness.Outcome != "draft_available" {
		return
	}
	fingerprint := previewFingerprint(device, m)
	if fingerprint == "" {
		return
	}
	unlock, err := c.controlStore.lock(device)
	if err != nil {
		return
	}
	defer unlock()
	p := privatePreview{Schema: 1, Device: device, Map: m.SelectedSavedMapID, Fingerprint: fingerprint, CreatedAt: c.now().UnixMilli(), Drafts: m.CleaningReadiness.Drafts}
	if c.controlStore.save(p) == nil {
		m.CleaningReadiness.VerifiedForControl = true
	}
}
