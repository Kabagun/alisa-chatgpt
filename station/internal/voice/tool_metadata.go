package voice

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const toolMetadataEnableLeaf = "tool-probe.enabled"
const toolMetadataLeaf = "last-tool-metadata.json"
const toolMetadataDuration = 120 * time.Second
const toolMetadataEventLimit = 256

// Names are fixed public protocol vocabulary, not dynamic server/user strings.
var toolMetadataTypes = [...]string{"other", "state_update", "usage_update", "tool_update", "error", "action_request", "goodbye", "close_request", "close_ready", "track_state", "speaking_update", "conversation_update", "streaming_message_update", "full_chat_message", "chat_message_delta", "relay_message", "relay_message_processed", "user_transcription_text", "live_captioning_text", "client_tool_invoke", "client_tool_update", "client_tool_result"}
var toolMetadataKeys = [...]string{"type", "payload", "tool", "name", "tool_name", "id", "call_id", "callId", "arguments", "args", "input", "result", "status", "update_type", "error", "output"}

// Type masks: null1, boolean2, number4, string8, object16, array32.
func metadataValueKind(raw []byte) uint8 {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0
	}
	switch raw[0] {
	case 'n':
		return 1
	case 't', 'f':
		return 2
	case '"':
		return 8
	case '{':
		return 16
	case '[':
		return 32
	default:
		return 4
	}
}

type toolMetadataShape struct {
	category    int
	toolLike    bool
	kinds       [len(toolMetadataKeys)]uint8
	unknownKeys uint64
	depth       uint8
}

func parseToolMetadata(data []byte) (toolMetadataShape, bool) {
	var shape toolMetadataShape
	if len(data) > maxMessageBytes {
		return shape, false
	}
	var wire wireEvent
	if json.Unmarshal(data, &wire) != nil {
		return shape, false
	}
	if wire.Type == "data_message" {
		var inner string
		if json.Unmarshal(wire.Data, &inner) != nil || len(inner) > maxMessageBytes {
			return shape, false
		}
		data = []byte(inner)
		wire = wireEvent{}
		if json.Unmarshal(data, &wire) != nil {
			return shape, false
		}
	}
	if len(wire.Payload) > 0 {
		var nested wireEvent
		if json.Unmarshal(wire.Payload, &nested) == nil && nested.Type != "" {
			data = wire.Payload
			wire = nested
		}
	}
	for i, name := range toolMetadataTypes {
		if wire.Type == name {
			shape.category = i
			break
		}
	}
	if len(wire.Type) <= 128 {
		lower := strings.ToLower(wire.Type)
		shape.toolLike = strings.Contains(lower, "tool") || strings.Contains(lower, "function")
	}
	var scan func([]byte, int)
	scan = func(raw []byte, depth int) {
		var fields map[string]jsontext.Value
		if json.Unmarshal(raw, &fields) != nil {
			return
		}
		shape.depth = max(shape.depth, uint8(depth))
		known := 0
		for i, key := range toolMetadataKeys {
			if value, ok := fields[key]; ok {
				known++
				shape.kinds[i] |= metadataValueKind(value)
			}
		}
		shape.unknownKeys += uint64(max(0, len(fields)-known))
		if depth >= 2 {
			return
		}
		for _, key := range []string{"payload", "tool", "arguments", "args", "input", "result"} {
			if value, ok := fields[key]; ok && metadataValueKind(value) == 16 {
				scan(value, depth+1)
			}
		}
	}
	scan(data, 0)
	return shape, true
}

func toolMetadataEnabled(parent string) bool {
	dir, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(dir)
	var st unix.Stat_t
	if unix.Fstat(dir, &st) != nil || !privateTimingDirectory(&st) {
		return false
	}
	fd, err := unix.Openat(dir, toolMetadataEnableLeaf, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	f := os.NewFile(uintptr(fd), toolMetadataEnableLeaf)
	defer f.Close()
	if unix.Fstat(fd, &st) != nil || !privateTimingFile(&st) || st.Size < 1 || st.Size > 2 {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(f, 3))
	return err == nil && (bytes.Equal(data, []byte("1")) || bytes.Equal(data, []byte("1\n")))
}

type toolMetadataStatus uint8

const (
	toolMetadataObserving toolMetadataStatus = iota
	toolMetadataTimeLimit
	toolMetadataEventLimitReached
	toolMetadataClosed
)

type toolMetadataSnapshot struct {
	serial, revision                                 uint64 // Private ordering only; never serialized.
	status                                           toolMetadataStatus
	elapsed                                          int64
	messages, rejected, unknownToolLike, unknownKeys uint64
	counts                                           [len(toolMetadataTypes)]uint64
	kinds                                            [len(toolMetadataKeys)]uint8
	depth                                            uint8
}

func (s toolMetadataSnapshot) json() []byte {
	statuses := [...]string{"observing", "time_limit", "event_limit", "closed"}
	if int(s.status) >= len(statuses) {
		return nil
	}
	types := map[string]uint64{}
	for i, count := range s.counts {
		if count > 0 {
			types[toolMetadataTypes[i]] = count
		}
	}
	keys := map[string]uint8{}
	for i, kind := range s.kinds {
		if kind > 0 {
			keys[toolMetadataKeys[i]] = kind
		}
	}
	wire := struct {
		Status          string            `json:"status"`
		Elapsed         int64             `json:"elapsed_ms"`
		Messages        uint64            `json:"messages"`
		Rejected        uint64            `json:"parse_rejected"`
		UnknownToolLike uint64            `json:"unknown_tool_like"`
		UnknownKeys     uint64            `json:"unknown_keys"`
		Depth           uint8             `json:"max_depth"`
		Types           map[string]uint64 `json:"event_counts"`
		Keys            map[string]uint8  `json:"field_type_masks"`
	}{statuses[s.status], s.elapsed, s.messages, s.rejected, s.unknownToolLike, s.unknownKeys, s.depth, types, keys}
	b, _ := json.Marshal(wire)
	if len(b) > 2047 {
		return nil
	}
	return append(b, '\n')
}

type toolMetadataProbe struct {
	mu       sync.Mutex
	started  time.Time
	snapshot toolMetadataSnapshot
	persist  func(toolMetadataSnapshot)
}

var toolMetadataSerial atomic.Uint64

func newToolMetadataProbe(ctx context.Context, persist func(toolMetadataSnapshot)) *toolMetadataProbe {
	m := &toolMetadataProbe{started: time.Now(), persist: persist, snapshot: toolMetadataSnapshot{serial: toolMetadataSerial.Add(1)}}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		m.publish()
		for {
			select {
			case <-ctx.Done():
				m.close()
				return
			case now := <-ticker.C:
				if m.expire(now) {
					return
				}
				m.publish()
			}
		}
	}()
	return m
}
func (m *toolMetadataProbe) publish() {
	if m == nil || m.persist == nil {
		return
	}
	m.mu.Lock()
	m.snapshot.revision++
	m.snapshot.elapsed = max(0, time.Since(m.started).Milliseconds())
	s := m.snapshot
	m.mu.Unlock()
	m.persist(s)
}
func (m *toolMetadataProbe) expire(now time.Time) bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	done := m.snapshot.status != toolMetadataObserving
	changed := false
	if !done && now.Sub(m.started) >= toolMetadataDuration {
		m.snapshot.status = toolMetadataTimeLimit
		done = true
		changed = true
	}
	m.mu.Unlock()
	if changed {
		m.publish()
	}
	return done
}
func (m *toolMetadataProbe) observe(data []byte) { m.observeAt(data, time.Now()) }
func (m *toolMetadataProbe) observeAt(data []byte, now time.Time) {
	if m == nil || m.expire(now) {
		return
	}
	shape, ok := parseToolMetadata(data)
	m.mu.Lock()
	if m.snapshot.status != toolMetadataObserving {
		m.mu.Unlock()
		return
	}
	m.snapshot.messages++
	if !ok {
		m.snapshot.rejected++
	} else {
		m.snapshot.counts[shape.category]++
		if shape.category == 0 && shape.toolLike {
			m.snapshot.unknownToolLike++
		}
		m.snapshot.unknownKeys = min(65535, m.snapshot.unknownKeys+shape.unknownKeys)
		m.snapshot.depth = max(m.snapshot.depth, shape.depth)
		// Only tool-like event fields enter the shape ledger. No values or names.
		if shape.toolLike {
			for i, kind := range shape.kinds {
				m.snapshot.kinds[i] |= kind
			}
		}
	}
	limit := m.snapshot.messages >= toolMetadataEventLimit
	if limit {
		m.snapshot.status = toolMetadataEventLimitReached
	}
	m.mu.Unlock()
	if limit {
		m.publish()
	}
}
func (m *toolMetadataProbe) close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	changed := false
	if m.snapshot.status == toolMetadataObserving {
		m.snapshot.status = toolMetadataClosed
		changed = true
	}
	m.mu.Unlock()
	if changed {
		m.publish()
	}
}

type toolMetadataSink struct {
	mu     sync.Mutex
	once   sync.Once
	wake   chan struct{}
	done   chan struct{}
	latest toolMetadataSnapshot
	write  func(toolMetadataSnapshot) bool
}

func newToolMetadataSink(write func(toolMetadataSnapshot) bool) *toolMetadataSink {
	return &toolMetadataSink{wake: make(chan struct{}, 1), done: make(chan struct{}), write: write}
}
func (s *toolMetadataSink) offer(value toolMetadataSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value.serial == 0 || value.serial < s.latest.serial || value.serial == s.latest.serial && (s.latest.status != toolMetadataObserving || value.revision <= s.latest.revision) {
		return
	}
	s.latest = value
	s.once.Do(func() {
		go func() {
			defer close(s.done)
			for range s.wake {
				s.mu.Lock()
				v := s.latest
				s.mu.Unlock()
				_ = s.write(v)
			}
		}()
	})
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

var lastToolMetadataSink = newToolMetadataSink(func(s toolMetadataSnapshot) bool {
	return writePrivateSnapshot(setupTimingDirectory, toolMetadataLeaf, s.json())
})
