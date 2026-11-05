package voice

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const contextProbeLeaf = "context-probe.json"
const contextProbeClaimedLeaf = "context-probe.claimed"
const maxContextProbeBytes = 1024
const contextProbeTTL = 300

var errContextProbeRejected = errors.New("context probe rejected; check private one-shot configuration")

// These are owner-selected thread/message anchors, not Voice session UUIDs or
// MCP receipts. No CLI, account-header or arbitrary tool selector is accepted.
type voiceContextProbe struct {
	Version         int    `json:"version"`
	ProjectID       string `json:"project_id"`
	ConversationID  string `json:"conversation_id"`
	ParentMessageID string `json:"parent_message_id"`
	ExpiresAt       int64  `json:"expires_at_unix"`
	claimed         bool
}

func validContextUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return value[14] >= '1' && value[14] <= '8' && (value[19] == '8' || value[19] == '9' || value[19] == 'a' || value[19] == 'b')
}
func (c *voiceContextProbe) valid(settings sessionSettings, now time.Time) bool {
	return c != nil && c.Version == 1 && validProjectID(c.ProjectID) && c.ProjectID == settings.ProjectID && settings.ActivationCommand == "" && validContextUUID(c.ConversationID) && validContextUUID(c.ParentMessageID) && c.ConversationID != c.ParentMessageID && c.ExpiresAt > now.Unix() && c.ExpiresAt-now.Unix() <= contextProbeTTL
}
func parseContextProbe(data []byte, settings sessionSettings, now time.Time) (*voiceContextProbe, error) {
	if len(data) == 0 || len(data) > maxContextProbeBytes {
		return nil, errContextProbeRejected
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(data, &fields) != nil || len(fields) != 5 {
		return nil, errContextProbeRejected
	}
	for _, name := range []string{"version", "project_id", "conversation_id", "parent_message_id", "expires_at_unix"} {
		if _, ok := fields[name]; !ok {
			return nil, errContextProbeRejected
		}
	}
	var c voiceContextProbe
	if json.Unmarshal(data, &c) != nil || !c.valid(settings, now) {
		return nil, errContextProbeRejected
	}
	return &c, nil
}

// A valid file is atomically moved once, before any network preparation.
// No-replace and inode checks prevent reuse/clobbering; unsupported renameat2
// fails closed. Cancellation/network failure never restores the armed file.
func takeVoiceContextProbe(parent string, settings sessionSettings, now time.Time) (sessionSettings, error) {
	if settings.contextProbe != nil {
		return sessionSettings{}, errContextProbeRejected
	}
	dir, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return settings, nil
	}
	if err != nil {
		return sessionSettings{}, errContextProbeRejected
	}
	defer unix.Close(dir)
	fd, err := unix.Openat(dir, contextProbeLeaf, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return settings, nil
	}
	if err != nil {
		return sessionSettings{}, errContextProbeRejected
	}
	f := os.NewFile(uintptr(fd), contextProbeLeaf)
	defer f.Close()
	var directory, opened, current unix.Stat_t
	if unix.Fstat(dir, &directory) != nil || !privateTimingDirectory(&directory) || unix.Fstat(fd, &opened) != nil || !privateTimingFile(&opened) || opened.Size < 1 || opened.Size > maxContextProbeBytes {
		return sessionSettings{}, errContextProbeRejected
	}
	data, err := io.ReadAll(io.LimitReader(f, maxContextProbeBytes+1))
	if err != nil {
		return sessionSettings{}, errContextProbeRejected
	}
	defer clear(data)
	c, err := parseContextProbe(data, settings, now)
	if err != nil {
		return sessionSettings{}, err
	}
	if unix.Fstatat(dir, contextProbeLeaf, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || current.Dev != opened.Dev || current.Ino != opened.Ino || !privateTimingFile(&current) {
		return sessionSettings{}, errContextProbeRejected
	}
	if unix.Renameat2(dir, contextProbeLeaf, dir, contextProbeClaimedLeaf, unix.RENAME_NOREPLACE) != nil {
		return sessionSettings{}, errContextProbeRejected
	}
	if unix.Fstatat(dir, contextProbeClaimedLeaf, &current, unix.AT_SYMLINK_NOFOLLOW) != nil || current.Dev != opened.Dev || current.Ino != opened.Ino || !privateTimingFile(&current) {
		return sessionSettings{}, errContextProbeRejected
	}
	c.claimed = true
	settings.contextProbe = c
	return settings, nil
}
