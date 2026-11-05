package voice

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const syntheticConversation = "11111111-1111-4111-8111-111111111111"
const syntheticParent = "22222222-2222-4222-8222-222222222222"

func contextFixture(now time.Time) voiceContextProbe {
	return voiceContextProbe{Version: 1, ProjectID: syntheticProjectID, ConversationID: syntheticConversation, ParentMessageID: syntheticParent, ExpiresAt: now.Unix() + 120}
}
func contextDirectory(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("private fixture")
	}
	return dir
}
func armSyntheticContext(t *testing.T, dir string, c voiceContextProbe) {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil || os.WriteFile(filepath.Join(dir, contextProbeLeaf), data, 0600) != nil {
		t.Fatal("configuration fixture")
	}
}

func TestContextProbeSingleUseAndProjectPayload(t *testing.T) {
	now := time.Now()
	dir := contextDirectory(t)
	settings := sessionSettings{ProjectID: syntheticProjectID, BackendThinkingEffort: "disabled", LedFeedback: true}
	armSyntheticContext(t, dir, contextFixture(now))
	resumed, err := takeVoiceContextProbe(dir, settings, now)
	if err != nil || resumed.contextProbe == nil || !resumed.contextProbe.claimed {
		t.Fatal("valid context not claimed")
	}
	if _, err := os.Lstat(filepath.Join(dir, contextProbeLeaf)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("armed context remained reusable")
	}
	info, err := os.Lstat(filepath.Join(dir, contextProbeClaimedLeaf))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("claimed context not private")
	}
	next, err := takeVoiceContextProbe(dir, settings, now)
	if err != nil || next != settings {
		t.Fatal("next activation did not return to fresh default")
	}
	decode := func(s sessionSettings) map[string]any {
		t.Helper()
		data, err := encodeSession(now, s)
		if err != nil {
			t.Fatal("encoding failed")
		}
		var p map[string]any
		if json.Unmarshal(data, &p) != nil {
			t.Fatal("payload fixture")
		}
		return p
	}
	fresh, probe := decode(settings), decode(resumed)
	if probe["conversation_id"] != syntheticConversation || probe["parent_message_id"] != syntheticParent || !reflect.DeepEqual(probe["conversation_mode"], map[string]any{"kind": "gizmo_interaction", "gizmo_id": syntheticProjectID}) {
		t.Fatal("thread anchors or Project routing changed")
	}
	if probe["voice_session_id"] == fresh["voice_session_id"] || probe["voice_status_request_id"] != probe["voice_session_id"] {
		t.Fatal("resumption reused Voice session identity")
	}
	if tools, ok := probe["client_tools"].([]any); !ok || len(tools) != 0 {
		t.Fatal("context experiment advertised tools")
	}
	for _, key := range []string{"conversation_id", "parent_message_id"} {
		if _, ok := fresh[key]; ok {
			t.Fatal("default fresh payload changed")
		}
		delete(probe, key)
	}
	for _, p := range []map[string]any{fresh, probe} {
		delete(p, "voice_session_id")
		delete(p, "voice_status_request_id")
	}
	if !reflect.DeepEqual(fresh, probe) {
		t.Fatal("context experiment changed unrelated session fields")
	}
	if _, err := encodeSession(now.Add(121*time.Second), resumed); err == nil {
		t.Fatal("expired claimed context encoded")
	}
	unclaimed := resumed
	copy := *resumed.contextProbe
	copy.claimed = false
	unclaimed.contextProbe = &copy
	if _, err := encodeSession(now, unclaimed); err == nil {
		t.Fatal("unclaimed context encoded")
	}
}

func TestContextProbeStrictValidationAndPrivateErrors(t *testing.T) {
	now := time.Now()
	settings := sessionSettings{ProjectID: syntheticProjectID}
	cases := []func(*voiceContextProbe){func(c *voiceContextProbe) { c.Version = 2 }, func(c *voiceContextProbe) { c.ProjectID = "g-p-11111111111111111111111111111111" }, func(c *voiceContextProbe) { c.ConversationID = "00000000-0000-0000-0000-000000000000" }, func(c *voiceContextProbe) { c.ParentMessageID = strings.ToUpper(syntheticParent[:24]) + "ABCDEFABCDEF" }, func(c *voiceContextProbe) { c.ParentMessageID = c.ConversationID }, func(c *voiceContextProbe) { c.ExpiresAt = now.Unix() }, func(c *voiceContextProbe) { c.ExpiresAt = now.Unix() + 301 }}
	for _, mutate := range cases {
		c := contextFixture(now)
		mutate(&c)
		data, _ := json.Marshal(c)
		_, err := parseContextProbe(data, settings, now)
		if err != errContextProbeRejected || strings.Contains(err.Error(), syntheticConversation) || strings.Contains(err.Error(), c.ProjectID) {
			t.Fatal("invalid context accepted or disclosed")
		}
	}
	data, _ := json.Marshal(contextFixture(now))
	for _, bad := range [][]byte{nil, make([]byte, maxContextProbeBytes+1), append([]byte(`{"account_id":"synthetic-private",`), data[1:]...), []byte(strings.Replace(string(data), `"version":1`, `"version":1,"version":1`, 1)), []byte(strings.Replace(string(data), `"version":1`, `"version":"1"`, 1))} {
		if _, err := parseContextProbe(bad, settings, now); err != errContextProbeRejected {
			t.Fatal("unknown/duplicate/oversized configuration accepted")
		}
	}
	command := settings
	command.ActivationCommand = "synthetic command"
	if _, err := parseContextProbe(data, command, now); err == nil {
		t.Fatal("non-Yandex mode accepted probe")
	}
}

func TestContextProbeUnsafeFilesAndNoReplace(t *testing.T) {
	now := time.Now()
	settings := sessionSettings{ProjectID: syntheticProjectID}
	for _, kind := range []string{"mode", "parent", "symlink", "hardlink", "collision"} {
		t.Run(kind, func(t *testing.T) {
			dir := contextDirectory(t)
			armSyntheticContext(t, dir, contextFixture(now))
			path := filepath.Join(dir, contextProbeLeaf)
			switch kind {
			case "mode":
				_ = os.Chmod(path, 0644)
			case "parent":
				_ = os.Chmod(dir, 0755)
			case "symlink":
				_ = os.Remove(path)
				_ = os.Symlink("absent", path)
			case "hardlink":
				if os.Link(path, filepath.Join(dir, "second-link")) != nil {
					t.Fatal("link fixture")
				}
			case "collision":
				if os.WriteFile(filepath.Join(dir, contextProbeClaimedLeaf), []byte("protected-fixture"), 0600) != nil {
					t.Fatal("collision fixture")
				}
			}
			if _, err := takeVoiceContextProbe(dir, settings, now); err != errContextProbeRejected {
				t.Fatal("unsafe context accepted")
			}
			if kind == "collision" {
				data, err := os.ReadFile(filepath.Join(dir, contextProbeClaimedLeaf))
				if err != nil || string(data) != "protected-fixture" {
					t.Fatal("claimed destination clobbered")
				}
			}
		})
	}
	if got, err := takeVoiceContextProbe(filepath.Join(t.TempDir(), "missing"), settings, now); err != nil || got != settings {
		t.Fatal("missing configuration changed fresh mode")
	}
}

func TestContextProbeConcurrentAtomicClaim(t *testing.T) {
	now := time.Now()
	dir := contextDirectory(t)
	armSyntheticContext(t, dir, contextFixture(now))
	settings := sessionSettings{ProjectID: syntheticProjectID}
	var won atomic.Int32
	var group sync.WaitGroup
	for range 12 {
		group.Go(func() {
			s, err := takeVoiceContextProbe(dir, settings, now)
			if err == nil && s.contextProbe != nil {
				won.Add(1)
			}
		})
	}
	group.Wait()
	if won.Load() != 1 {
		t.Fatal("context was not consumed exactly once")
	}
}

func TestContextProbeRejectedActivationCannotStartVoice(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	g := newStationGate()
	g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, time.Now().Add(-time.Second))
	e := g.begin(ctx)
	client, server := net.Pipe()
	defer server.Close()
	done := make(chan error, 1)
	var consumed atomic.Int32
	go func() {
		done <- runYandexEpoch(e, g, credential{}, sessionSettings{}, yandexEpochDeps{
			detector: func() (wakeDetector, error) { return &yandexTestDetector{}, nil }, dial: func(context.Context) (net.Conn, error) { return client, nil },
			context: func(s sessionSettings) (sessionSettings, error) {
				if e.nativeIdleReady.IsZero() {
					t.Error("claim before gate")
				}
				consumed.Add(1)
				return sessionSettings{}, errContextProbeRejected
			},
			call: func(*stationEpoch, credential, *stationCapture, sessionSettings) error {
				t.Error("rejected context reached Voice")
				return nil
			},
		})
	}()
	request := make([]byte, len(audioRequest()))
	if _, err := io.ReadFull(server, request); err != nil {
		t.Fatal("IPC fixture")
	}
	if _, err := server.Write(framedIPC(158, audioPayload(make([]byte, 640), 16000, 1))); err != nil {
		t.Fatal("PCM fixture")
	}
	select {
	case err := <-done:
		if _, ok := errors.AsType[voiceCallError](err); !ok || consumed.Load() != 1 {
			t.Fatal("context rejection changed attempt lifecycle")
		}
	case <-ctx.Done():
		t.Fatal("context rejection blocked")
	}
	_ = g.finish(e, context.Cause(e.ctx))
}
