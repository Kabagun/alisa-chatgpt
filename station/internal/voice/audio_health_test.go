package voice

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFeedbackAudioHealthCapturePhasesEnergyAndPrivacy(t *testing.T) {
	e := newStationEpoch(t.Context(), 3)
	defer e.revoke(context.Canceled)
	c := newStationCapture(e)
	h := e.output.health
	pcm := make([]byte, 640)
	for i := 0; i < len(pcm); i += 8 {
		binary.LittleEndian.PutUint16(pcm[i:], 32768)
		binary.LittleEndian.PutUint16(pcm[i+4:], 32767)
	}
	if c.pushPCM(pcm) != nil || h.count[audioPCMSamples].Load() != 0 {
		t.Fatal("idle KWS entered call diagnostics")
	}
	if c.beginConnecting() != nil || c.pushPCM([]byte{0xe8, 3, 0x18, 0xfc, 0, 0, 0, 0}) != nil {
		t.Fatal("connecting fixture")
	}
	if c.beginStreaming(t.Context()) != nil || c.pushPCM(pcm) != nil {
		t.Fatal("streaming fixture")
	}
	clear(pcm)
	h.control(event{phase: feedbackServerListening}, false)
	h.control(event{err: errors.New("synthetic-private-text")}, false)
	s := h.snapshot(false)
	if s.count[audioConnectingDropped] != 4 || s.count[audioStreamingSamples] != 320 || s.count[audioQueued] != 1 || s.nonzero[1] != 160 || s.peak[1] != 32768 || s.rms[1] != 23170 || s.rms[0] != 707 {
		t.Fatal("phase/energy counters changed", s.count, s.nonzero, s.peak, s.rms)
	}
	if s.count[audioKnownStates] != 1 || s.count[audioKnownErrors] != 1 {
		t.Fatal("known event counts changed")
	}
	var wire map[string]any
	if json.Unmarshal(s.json(), &wire) != nil {
		t.Fatal("invalid snapshot")
	}
	for _, key := range []string{"serial", "revision", "text", "error", "token", "pcm", "project_id", "ssrc"} {
		if _, ok := wire[key]; ok {
			t.Fatal("private/unbounded field in snapshot")
		}
	}
	if len(wire) != len(audioCounterNames)+len(audioActivityNames)+13 {
		t.Fatal("snapshot schema changed")
	}
	if s.last[audioLastStreaming] < 0 || s.last[audioLastSampleWrite] != -1 {
		t.Fatal("activity sentinel changed")
	}
}

func TestFeedbackAudioHealthPlaybackDiagnostics(t *testing.T) {
	h := newAudioHealth()
	h.playbackQueued.Store(960)
	h.playbackPeak.Store(2880)
	h.add(audioPlaybackUnderruns, 2)
	h.add(audioPlaybackUnderrunFrames, 1440)
	h.add(audioRemoteFECAttempts, 3)
	var wire map[string]any
	if json.Unmarshal(h.snapshot(false).json(), &wire) != nil {
		t.Fatal("invalid playback diagnostics")
	}
	if wire["playback_queue_frames"] != float64(960) || wire["playback_queue_peak_frames"] != float64(2880) || wire["playback_underruns"] != float64(2) || wire["playback_underrun_frames"] != float64(1440) || wire["remote_fec_attempts"] != float64(3) {
		t.Fatal("playback diagnostics changed", wire)
	}
}

func TestFeedbackAudioHealthBoundedPrivateFile(t *testing.T) {
	s := audioHealthSnapshot{serial: 1, revision: 1, active: true, elapsed: math.MaxInt64, playbackQueued: math.MaxInt64, playbackPeak: math.MaxInt64}
	for i := range s.count {
		s.count[i] = math.MaxUint64
	}
	for i := range s.last {
		s.last[i] = math.MaxInt64
	}
	for i := range s.peak {
		s.peak[i] = 32768
		s.rms[i] = 32768
		s.nonzero[i] = math.MaxUint64
	}
	data := s.json()
	if len(data) == 0 || len(data) > 2048 {
		t.Fatal("snapshot exceeded fixed file budget")
	}
	dir := filepath.Join(t.TempDir(), "private")
	if os.Mkdir(dir, 0700) != nil || !writePrivateSnapshot(dir, audioHealthLeaf, data) {
		t.Fatal("private audio leaf rejected")
	}
	info, err := os.Lstat(filepath.Join(dir, audioHealthLeaf))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("audio marker is not private")
	}
	if !writePrivateSnapshot(dir, audioHealthLeaf, (&audioHealth{}).snapshot(false).json()) {
		t.Fatal("atomic replacement failed")
	}
}

func TestFeedbackAudioHealthSinkOrdersFinalAndNewCallWithoutBlocking(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	writes := make(chan audioHealthSnapshot, 8)
	var once sync.Once
	sink := newAudioHealthSink(func(s audioHealthSnapshot) bool {
		once.Do(func() { close(entered); <-release })
		writes <- s
		return false
	})
	sink.offer(audioHealthSnapshot{serial: 1, revision: 1})
	<-entered
	done := make(chan struct{})
	go func() {
		sink.offer(audioHealthSnapshot{serial: 1, revision: 2, final: true})
		sink.offer(audioHealthSnapshot{serial: 2, revision: 1, active: true})
		sink.offer(audioHealthSnapshot{serial: 1, revision: 3, final: true})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("filesystem I/O blocked metadata producer")
	}
	close(release)
	<-writes
	second := <-writes
	if second.serial != 2 || second.final {
		t.Fatal("old final displaced a new call")
	}
	sink.offer(audioHealthSnapshot{serial: 2, revision: 2, final: true})
	sink.offer(audioHealthSnapshot{serial: 2, revision: 99})
	if final := <-writes; !final.final {
		t.Fatal("late periodic snapshot reopened final call")
	}
	close(sink.wake)
	<-sink.done
	if len(writes) != 0 {
		t.Fatal("stale snapshot reached I/O")
	}
}

func TestFeedbackAudioHealthPublishingFinishesWithoutSamples(t *testing.T) {
	h := newAudioHealth()
	out := make(chan audioHealthSnapshot, 8)
	h.begin(func(s audioHealthSnapshot) { out <- s })
	select {
	case first := <-out:
		if first.final {
			t.Fatal("premature final")
		}
	case <-time.After(time.Second):
		t.Fatal("no initial snapshot")
	}
	h.finish(true)
	select {
	case last := <-out:
		if !last.final || !last.peerClosed {
			t.Fatal("missing local peer closure proof")
		}
	case <-time.After(time.Second):
		t.Fatal("final marker blocked")
	}
}
