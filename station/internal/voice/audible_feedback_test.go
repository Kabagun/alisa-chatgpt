package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log"
	"slices"
	"testing"
	"time"
)

func TestAudibleMicrophoneActivityPreservesPCMAndIgnoresSilence(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	now := time.Unix(1, 0)
	i := newVoiceIdle(ctx, now)
	i.observe(feedbackMediaReady, now)
	for _, pcm := range [][]int16{nil, {0, 64, 128, 256, -256}} {
		i.microphone(pcm, now.Add(59*time.Second))
	}
	if !i.expiredAt(now.Add(time.Minute)) {
		t.Fatal("silence/comfort noise extended quiet waiting")
	}
	pcm := []int16{1000, -1000, 0}
	want := slices.Clone(pcm)
	i.microphone(pcm, now.Add(59*time.Second))
	if !slices.Equal(pcm, want) || i.expiredAt(now.Add(time.Minute)) || !i.expiredAt(now.Add(119*time.Second)) {
		t.Fatal("real microphone activity changed PCM or failed to reset waiting")
	}
	cancel()
	before := i.activity.Load()
	i.microphone(pcm, now.Add(5*time.Minute))
	if i.activity.Load() != before {
		t.Fatal("canceled epoch accepted late microphone activity")
	}
}

func TestAudiblePlaybackActivityAndNoLocalSpeech(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	defer e.revoke(context.Canceled)
	f := newVoiceFeedback(e.ctx, log.New(io.Discard, "", 0), nil)
	e.output.feedback = f
	f.mark(feedbackMediaReady)
	f.mark(feedbackServerListening)
	output := make([]byte, samplesPerFrame*4)
	for _, elapsed := range []time.Duration{time.Second, 30 * time.Second, time.Minute, 3 * time.Minute} {
		e.output.callbackAt(output, samplesPerFrame, f.started.Add(elapsed))
		if !allZero(output) || f.idle.activity.Load() != -1 {
			t.Fatal("quiet callback generated local startup/reminder speech or activity")
		}
	}
	pcm := make([]int16, samplesPerFrame*2)
	for i := range pcm {
		pcm[i] = 1000
	}
	for range 3 {
		e.output.queueDecoded(pcm)
	}
	if f.idle.activity.Load() != -1 {
		t.Fatal("queued PCM counted as actual playback")
	}
	now := f.started.Add(4 * time.Minute)
	e.output.callbackAt(output, samplesPerFrame, now)
	want := bytes.Repeat([]byte{0xe8, 3, 0xe8, 3}, samplesPerFrame)
	if !bytes.Equal(output, want) || f.idle.expiredAt(now.Add(voiceIdleTimeout-time.Nanosecond)) {
		t.Fatal("remote playback changed or failed to reset waiting")
	}
	if !f.idle.expiredAt(now.Add(voiceIdleTimeout)) {
		t.Fatal("remote playback did not expire after a fresh minute")
	}
	e.revoke(errAlicePriority)
	e.output.callbackAt(output, samplesPerFrame, now.Add(time.Second))
	if !allZero(output) || int16(binary.LittleEndian.Uint16(want)) != 1000 {
		t.Fatal("native priority did not silence remaining remote PCM")
	}
}
