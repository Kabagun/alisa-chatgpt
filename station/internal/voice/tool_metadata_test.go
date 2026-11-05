package voice

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func metadataFixture(now time.Time) *toolMetadataProbe {
	return &toolMetadataProbe{started: now, snapshot: toolMetadataSnapshot{serial: 1}}
}
func TestToolMetadataShapesRedactValuesAndUnknownNames(t *testing.T) {
	const secret = "synthetic-private-marker"
	inner := `{"type":"client_tool_invoke","payload":{"call_id":"` + secret + `","name":"` + secret + `","arguments":{"` + secret + `":"` + secret + `"}}}`
	outer, _ := json.Marshal(map[string]any{"type": "data_message", "data": inner})
	now := time.Now()
	m := metadataFixture(now)
	m.observeAt(outer, now)
	m.observeAt([]byte(`{"type":"private_tool_`+secret+`","payload":{"result":"`+secret+`"}}`), now)
	m.observeAt([]byte(`{"type":"user_transcription_text","payload":{"text":"`+secret+`"}}`), now)
	s := m.snapshot
	data := s.json()
	if bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte(`"text":`)) || bytes.Contains(data, []byte("private_tool")) {
		t.Fatal("metadata leaked a value or arbitrary field/type name")
	}
	if s.messages != 3 || s.unknownToolLike != 1 || s.depth != 2 || s.unknownKeys == 0 {
		t.Fatal("envelope or unknown shape classification changed")
	}
	var wire struct {
		Types map[string]uint64 `json:"event_counts"`
		Keys  map[string]uint8  `json:"field_type_masks"`
	}
	if json.Unmarshal(data, &wire) != nil || wire.Types["client_tool_invoke"] != 1 || wire.Types["user_transcription_text"] != 1 || wire.Keys["call_id"] != 8 || wire.Keys["arguments"] != 16 || wire.Keys["result"] != 8 {
		t.Fatal("allowlisted shapes changed")
	}
	if _, ok := wire.Keys["text"]; ok {
		t.Fatal("transcript field entered tool ledger")
	}
	if _, ok := parseToolMetadata(make([]byte, maxMessageBytes+1)); ok {
		t.Fatal("oversized metadata accepted")
	}
}

func TestToolMetadataBudgetExpiryAndConcurrentClose(t *testing.T) {
	now := time.Now()
	m := metadataFixture(now)
	data := []byte(`{"type":"tool_update","payload":{"update_type":"hangup"}}`)
	for range toolMetadataEventLimit + 20 {
		m.observeAt(data, now)
	}
	if m.snapshot.messages != toolMetadataEventLimit || m.snapshot.status != toolMetadataEventLimitReached {
		t.Fatal("metadata event bound changed")
	}
	expired := metadataFixture(now)
	expired.observeAt(data, now.Add(toolMetadataDuration))
	if expired.snapshot.status != toolMetadataTimeLimit || expired.snapshot.messages != 0 {
		t.Fatal("expired window retained metadata")
	}
	concurrent := metadataFixture(now)
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			for range 100 {
				concurrent.observeAt(data, now)
			}
		})
	}
	group.Go(func() { concurrent.close() })
	group.Wait()
	if concurrent.snapshot.messages > toolMetadataEventLimit || concurrent.snapshot.status == toolMetadataObserving {
		t.Fatal("concurrent finalization exceeded budget")
	}
}

func TestToolMetadataPrivateEnableAndBoundedAtomicFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("fixture")
	}
	flag := filepath.Join(dir, toolMetadataEnableLeaf)
	if toolMetadataEnabled(dir) {
		t.Fatal("missing marker enabled observer")
	}
	for _, content := range []string{"1", "1\n"} {
		if os.WriteFile(flag, []byte(content), 0600) != nil || !toolMetadataEnabled(dir) {
			t.Fatal("private explicit marker rejected")
		}
	}
	for _, content := range []string{"", "0", "true", "1\nextra"} {
		if os.WriteFile(flag, []byte(content), 0600) != nil || toolMetadataEnabled(dir) {
			t.Fatal("invalid marker enabled observer")
		}
	}
	if os.WriteFile(flag, []byte("1"), 0600) != nil || os.Chmod(flag, 0644) != nil || toolMetadataEnabled(dir) {
		t.Fatal("public marker enabled observer")
	}
	if os.Remove(flag) != nil || os.Symlink("absent", flag) != nil || toolMetadataEnabled(dir) {
		t.Fatal("symlink marker enabled observer")
	}
	s := toolMetadataSnapshot{serial: 1, revision: 1, messages: 256, rejected: 256, unknownToolLike: 256, unknownKeys: 65535, depth: 2}
	for i := range s.counts {
		s.counts[i] = 256
	}
	for i := range s.kinds {
		s.kinds[i] = 63
	}
	data := s.json()
	if len(data) == 0 || len(data) > 2048 || !writePrivateSnapshot(dir, toolMetadataLeaf, data) {
		t.Fatal("metadata size/atomic privacy bound failed")
	}
	info, err := os.Lstat(filepath.Join(dir, toolMetadataLeaf))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("metadata leaf is not private")
	}
	if os.Remove(flag) != nil || os.WriteFile(flag, []byte("1"), 0600) != nil || !toolMetadataEnabled(dir) {
		t.Fatal("private marker reset fixture")
	}
	if os.Chmod(dir, 0755) != nil || toolMetadataEnabled(dir) {
		t.Fatal("unsafe parent enabled observer")
	}
}

func TestToolMetadataSinkDoesNotBlockOrOverwriteNewCall(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	out := make(chan toolMetadataSnapshot, 8)
	var once sync.Once
	sink := newToolMetadataSink(func(s toolMetadataSnapshot) bool {
		once.Do(func() { close(entered); <-release })
		out <- s
		return false
	})
	sink.offer(toolMetadataSnapshot{serial: 1, revision: 1})
	<-entered
	done := make(chan struct{})
	go func() {
		sink.offer(toolMetadataSnapshot{serial: 1, revision: 2, status: toolMetadataClosed})
		sink.offer(toolMetadataSnapshot{serial: 2, revision: 1})
		sink.offer(toolMetadataSnapshot{serial: 1, revision: 9, status: toolMetadataClosed})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("metadata I/O blocked producer")
	}
	close(release)
	<-out
	if s := <-out; s.serial != 2 || s.status != toolMetadataObserving {
		t.Fatal("old call displaced new metadata")
	}
	sink.offer(toolMetadataSnapshot{serial: 2, revision: 2, status: toolMetadataClosed})
	sink.offer(toolMetadataSnapshot{serial: 2, revision: 99})
	if s := <-out; s.status != toolMetadataClosed {
		t.Fatal("late periodic reopened final metadata")
	}
	close(sink.wake)
	<-sink.done
	if len(out) != 0 {
		t.Fatal("stale metadata reached I/O")
	}
}

func TestToolMetadataDoesNotAdvertiseToolsOrDispatchEvents(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	m := metadataFixture(time.Now())
	p := &peer{ctx: ctx, cancel: cancel, report: log.New(io.Discard, "", 0), toolMetadata: m}
	p.handleMessage([]byte(`{"type":"client_tool_invoke","payload":{"name":"synthetic_function","arguments":{},"call_id":"synthetic-call"}}`))
	if ctx.Err() != nil || m.snapshot.messages != 1 {
		t.Fatal("observer changed event behavior")
	}
	settings := sessionSettings{ProjectID: syntheticProjectID, BackendThinkingEffort: "disabled"}
	var previous map[string]any
	for range 2 {
		data, err := encodeSession(time.Now(), settings)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if json.Unmarshal(data, &payload) != nil {
			t.Fatal("payload fixture")
		}
		tools, ok := payload["client_tools"].([]any)
		if !ok || len(tools) != 0 {
			t.Fatal("observer advertised tools")
		}
		delete(payload, "voice_session_id")
		delete(payload, "voice_status_request_id")
		if previous != nil && !reflect.DeepEqual(previous, payload) {
			t.Fatal("observer changed Voice/Project request")
		}
		previous = payload
	}
	for _, raw := range []string{`{"type":"unknown-private-event","payload":{"text":"private"}}`, `not-json`} {
		p.handleMessage([]byte(raw))
		if strings.Contains(string(m.snapshot.json()), "private") {
			t.Fatal("raw event disclosed")
		}
	}
}
