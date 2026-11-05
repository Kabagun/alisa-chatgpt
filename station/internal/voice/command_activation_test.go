package voice

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testActivation = "запусти колю"

func commandPayload(state uint64, text string) []byte {
	return wireBytes(wireInt(nil, 1, state), 2, []byte(text))
}

func commandEvent() nativeEvent {
	return nativeEvent{stateChanged: true, state: aliceBusy, commandMatch: true}
}

func commandGate() *stationGate {
	g := newStationGate()
	g.command = &commandTrigger{phrase: testActivation}
	return g
}

func TestActivationCommandSettingsStayLocal(t *testing.T) {
	settings, err := parseRunSettings([]string{"--activation-command", "  Запусти, Колю! ", "--backend-thinking-effort=disabled"})
	if err != nil || settings.ActivationCommand != testActivation || settings.BackendThinkingEffort != "disabled" {
		t.Fatal("activation settings were not parsed")
	}
	defaults, err := parseRunSettings(nil)
	if err != nil || defaults.ActivationCommand != "" {
		t.Fatal("default activation changed")
	}
	for _, bad := range []string{"!!!", "  ", string([]byte{0xff}), strings.Repeat("x", 257)} {
		if _, err := parseRunSettings([]string{"--activation-command", bad}); err == nil {
			t.Fatal("invalid activation accepted")
		}
	}
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	encode := func(settings sessionSettings) map[string]any {
		t.Helper()
		data, err := encodeSession(now, settings)
		if err != nil || bytes.Contains(data, []byte(testActivation)) || bytes.Contains(data, []byte("activation")) {
			t.Fatal("local activation leaked into Voice payload")
		}
		var result map[string]any
		if json.Unmarshal(data, &result) != nil {
			t.Fatal("invalid Voice payload")
		}
		delete(result, "voice_session_id")
		delete(result, "voice_status_request_id")
		return result
	}
	before := encode(sessionSettings{BackendThinkingEffort: "disabled"})
	after := encode(settings)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("activation changed ordinary Voice payload")
	}
}

func TestActivationCommandDecodingIsExactAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		state uint64
		text  string
		want  bool
	}{
		{3, "запусти колю", true}, {3, "АЛИСА, Запусти Колю!", true},
		{3, "запусти\tколю", true}, {3, "не запусти колю", false},
		{3, "напомни фразу запусти колю", false}, {3, "запусти колю пожалуйста", false},
		{3, "запусти коляску", false}, {1, "запусти колю", false},
		{2, "запусти колю", false}, {0, "запусти колю", false},
		{3, string([]byte{0xff}), false},
	} {
		if got := decodeActivationCommand(ipcEnvelope{kind: 3, payload: commandPayload(tc.state, tc.text)}, testActivation); got != tc.want {
			t.Fatal("command classification changed")
		}
	}
	duplicate := wireBytes(commandPayload(3, testActivation), 2, []byte(testActivation))
	for _, payload := range [][]byte{duplicate, wireInt(wireInt(nil, 1, 3), 2, 1), {0x08, 0x80}} {
		if decodeActivationCommand(ipcEnvelope{kind: 3, payload: payload}, testActivation) {
			t.Fatal("malformed command accepted")
		}
	}
	if decodeActivationCommand(ipcEnvelope{kind: 210, payload: commandPayload(3, testActivation)}, testActivation) {
		t.Fatal("wrong message kind accepted")
	}
}

func TestCommandGateNeedsFreshUtteranceAndStableIdle(t *testing.T) {
	now := time.Unix(100, 0)
	g := commandGate()
	g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, now)
	if g.beginAt(t.Context(), now.Add(time.Second)) != nil {
		t.Fatal("idle alone opened command epoch")
	}
	g.observeAt(commandEvent(), now.Add(time.Second))
	g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, now.Add(2*time.Second))
	if g.beginAt(t.Context(), now.Add(3*time.Second)) != nil {
		t.Fatal("cached startup command was admitted")
	}
	g.observeAt(nativeEvent{stateChanged: true, state: aliceListening}, now.Add(4*time.Second))
	g.observeAt(commandEvent(), now.Add(5*time.Second))
	g.observeAt(commandEvent(), now.Add(5*time.Second+time.Millisecond))
	g.observeAt(nativeEvent{stateChanged: true, state: aliceSpeaking}, now.Add(6*time.Second))
	if g.beginAt(t.Context(), now.Add(7*time.Second)) != nil {
		t.Fatal("native TTS was interrupted")
	}
	g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, now.Add(8*time.Second))
	if g.beginAt(t.Context(), now.Add(8*time.Second+commandIdleStability-1)) != nil {
		t.Fatal("unstable idle admitted")
	}
	e := g.beginAt(t.Context(), now.Add(8*time.Second+commandIdleStability))
	if e == nil || g.activate(e) != nil {
		t.Fatal("fresh stable command did not admit")
	}
	g.finish(e, nil)
	if g.beginAt(t.Context(), now.Add(9*time.Second)) != nil {
		t.Fatal("one command opened a second epoch")
	}
}

func TestCommandGateExpiresAndKeepsAlicePriority(t *testing.T) {
	now := time.Unix(100, 0)
	queue := func(g *stationGate) {
		g.observeAt(nativeEvent{activation: true}, now)
		g.observeAt(commandEvent(), now.Add(time.Second))
	}
	g := commandGate()
	queue(g)
	g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, now.Add(12*time.Second))
	if g.beginAt(t.Context(), now.Add(13*time.Second)) != nil {
		t.Fatal("expired pending command admitted")
	}
	for _, takeover := range []nativeEvent{{activation: true}, {stateChanged: true, state: aliceListening}} {
		g = commandGate()
		queue(g)
		g.observeAt(takeover, now.Add(2*time.Second))
		g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, now.Add(3*time.Second))
		if g.beginAt(t.Context(), now.Add(4*time.Second)) != nil {
			t.Fatal("new Alice utterance did not cancel pending command")
		}
	}
	g = commandGate()
	queue(g)
	g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, now.Add(2*time.Second))
	e := g.beginAt(t.Context(), now.Add(3*time.Second))
	if e == nil {
		t.Fatal("test command failed admission")
	}
	g.observeAt(nativeEvent{activation: true}, now.Add(4*time.Second))
	if !errors.Is(context.Cause(e.ctx), errAlicePriority) || g.activate(e) == nil {
		t.Fatal("native activation failed to revoke active command epoch")
	}
	g.finish(e, context.Cause(e.ctx))
}

func TestCommandServiceWaitsLazilyAndDoesNotRearm(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	client, server := net.Pipe()
	defer server.Close()
	attempts := make(chan *stationEpoch, 2)
	done := make(chan error, 1)
	returned := make(chan struct{})
	var output bytes.Buffer
	go func() {
		defer close(returned)
		done <- serveStation(ctx, client, credential{}, &output, func(e *stationEpoch, _ *stationGate, _ credential) error {
			attempts <- e
			<-e.ctx.Done()
			return context.Cause(e.ctx)
		}, func(context.Context) error {
			t.Error("canceled command played completion cue")
			return nil
		}, testActivation)
	}()
	t.Cleanup(func() { cancel(); server.Close(); <-returned })
	send := func(kind uint64, payload []byte) {
		t.Helper()
		if server.SetWriteDeadline(time.Now().Add(time.Second)) != nil {
			t.Fatal("pipe write deadline")
		}
		if _, err := server.Write(framedIPC(kind, payload)); err != nil {
			t.Fatal(err)
		}
	}
	quiet := func() {
		t.Helper()
		select {
		case <-attempts:
			t.Fatal("attempt opened without a fresh stable-idle command")
		case <-time.After(60 * time.Millisecond):
		}
	}
	send(3, wireInt(nil, 1, 0))
	quiet()
	send(3, wireInt(nil, 1, 1))
	send(3, commandPayload(3, "Алиса, запусти Колю!"))
	send(3, wireInt(nil, 1, 2))
	quiet()
	send(3, wireInt(nil, 1, 0))
	var epoch *stationEpoch
	select {
	case epoch = <-attempts:
	case <-ctx.Done():
		t.Fatal("recognized command never admitted")
	}
	activation := wireBytes(wireBytes(nil, 7, []byte("ActivationSpotterCapability")), 15, wireInt(nil, 1, 2))
	send(210, activation)
	select {
	case <-epoch.ctx.Done():
	case <-ctx.Done():
		t.Fatal("native activation did not interrupt")
	}
	send(3, wireInt(nil, 1, 0))
	quiet()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("command service did not close cleanly", err)
	}
	if strings.Contains(output.String(), "Колю") || strings.Contains(output.String(), testActivation) {
		t.Fatal("recognized text leaked into diagnostics")
	}
}
