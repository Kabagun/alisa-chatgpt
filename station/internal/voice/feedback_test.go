package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
)

func TestFeedbackDisplayStatesAreAnExactNonfunctionalAllowlist(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  feedbackPhase
	}{
		{`"idle"`, feedbackServerIdle},
		{`"listening"`, feedbackServerListening},
		{`"speaking"`, feedbackServerSpeaking},
		{`"responding"`, feedbackServerResponding},
		{`"LISTENING"`, feedbackNone},
		{`"private-marker"`, feedbackNone},
		{`{"private-marker":true}`, feedbackNone},
		{`null`, feedbackNone},
		{`42`, feedbackNone},
	} {
		raw := `{"type":"state_update","payload":{"new_state":` + tc.state + `,"text":"private-marker"}}`
		for _, input := range [][]byte{[]byte(raw), testEnvelope(t, raw), testEnvelope(t, `{"payload":`+raw+`}`)} {
			ev, err := decodeEvent(input)
			if err != nil || ev.ended || ev.err != nil || ev.phase != tc.want {
				t.Fatalf("display state affected control or missed allowlist: %+v %v", ev, err)
			}
		}
	}
}

func TestFeedbackReadyOrderingRedactionAndRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out bytes.Buffer
	f := newVoiceFeedback(ctx, log.New(&out, "", 0), nil)
	f.mark(feedbackCaptureReady)
	f.mark(feedbackCaptureReady)
	f.mark(feedbackPhase(255))
	f.mark(feedbackServerListening)
	if strings.Count(out.String(), "VOICE_PHASE") != 1 {
		t.Fatal("early server state or unknown enum escaped readiness gate")
	}
	f.mark(feedbackOfferReady)
	f.mark(feedbackAnswerReady)
	f.mark(feedbackTransportReady)
	f.mark(feedbackMediaReady)
	f.mark(feedbackServerSpeaking)
	cancel()
	f.mark(feedbackServerListening)
	f.mark(feedbackMediaReady)
	f.mark(feedbackStopped)
	want := []string{"capture_ready", "offer_ready", "answer_ready", "transport_ready", "media_ready", "server_listening", "server_speaking", "stopped"}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(want) {
		t.Fatalf("duplicate or late feedback: %d lines", len(lines))
	}
	for i, phase := range want {
		if !strings.HasPrefix(lines[i], "VOICE_PHASE "+phase+" elapsed_ms=") {
			t.Fatal("feedback order or fixed format changed")
		}
	}
}

func TestFeedbackConcurrentBoundAndCanceledPeer(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	var out bytes.Buffer
	f := newVoiceFeedback(ctx, log.New(&out, "", 0), nil)
	f.mark(feedbackMediaReady)
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 150 {
				f.mark(feedbackServerListening)
				f.mark(feedbackServerSpeaking)
			}
		})
	}
	workers.Wait()
	cancel(errAlicePriority)
	f.mark(feedbackStopped)
	before := out.Len()
	p := &peer{ctx: ctx, cancel: cancel, feedback: f}
	p.handleMessage(testEnvelope(t, `{"type":"state_update","payload":{"new_state":"speaking","text":"private-marker"}}`))
	if out.Len() != before || strings.Count(out.String(), "VOICE_PHASE") > 128 || strings.Contains(out.String(), "private-marker") || !strings.Contains(out.String(), "VOICE_PHASE stopped") {
		t.Fatal("feedback exceeded bound, leaked data or survived native cancellation")
	}
	if !errors.Is(context.Cause(ctx), errAlicePriority) {
		t.Fatal("display feedback changed native priority")
	}
}

func TestCommandReadyCueIsShortFadedRisingPair(t *testing.T) {
	pcm := readyCuePCM()
	if len(pcm) != 11520*2 {
		t.Fatal("command ready cue is not the requested 240 ms pair")
	}
	data := make([]byte, len(pcm)*2)
	for i, sample := range pcm {
		if sample > 9831 || sample < -9831 || i%2 == 1 && sample != pcm[i-1] {
			t.Fatal("cue volume or stereo invariant failed")
		}
		binary.LittleEndian.PutUint16(data[i*2:], uint16(sample))
	}
	const firstEnd = 3360 * 4
	const secondStart = (3360 + 4800) * 4
	if !goodTone(data[:firstEnd], 660, 990) || !goodTone(data[secondStart:], 990, 660) || !allZero(data[firstEnd:secondStart]) || !allZero(data[:4]) || !allZero(data[len(data)-4:]) {
		t.Fatal("ready cue lacks its two faded tones or silent separation")
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errAlicePriority)
	if err := playReadyCue(ctx); !errors.Is(err, errAlicePriority) {
		t.Fatal("canceled ready cue attempted playback", err)
	}
}
