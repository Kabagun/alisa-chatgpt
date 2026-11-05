package voice

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// These are local milestones or a small allowlist of website display states.
// They never authorize microphone/playback work or override native Alice.
type feedbackPhase uint8

const (
	feedbackNone feedbackPhase = iota
	feedbackTargetMatched
	feedbackNativeIdleReady
	feedbackCaptureReady
	feedbackOfferReady
	feedbackAnswerReady
	feedbackTransportReady
	feedbackMediaReady
	feedbackServerIdle
	feedbackServerListening
	feedbackServerSpeaking
	feedbackServerResponding
	feedbackPlaybackActive
	feedbackPlaybackQuiet
	feedbackStopped
)

func (p feedbackPhase) name() string {
	switch p {
	case feedbackTargetMatched:
		return "target_matched"
	case feedbackNativeIdleReady:
		return "native_idle_ready"
	case feedbackCaptureReady:
		return "capture_ready"
	case feedbackOfferReady:
		return "offer_ready"
	case feedbackAnswerReady:
		return "answer_ready"
	case feedbackTransportReady:
		return "transport_ready"
	case feedbackMediaReady:
		return "media_ready"
	case feedbackServerIdle:
		return "server_idle"
	case feedbackServerListening:
		return "server_listening"
	case feedbackServerSpeaking:
		return "server_speaking"
	case feedbackServerResponding:
		return "server_responding"
	case feedbackPlaybackActive:
		return "playback_active"
	case feedbackPlaybackQuiet:
		return "playback_quiet"
	case feedbackStopped:
		return "stopped"
	default:
		return ""
	}
}

// Only fixed enum names and local elapsed milliseconds reach the existing
// report sink. No server string, transcription, identifier or audio is retained.
// The production supervisor discards that sink. The separate fixed RAM setup
// snapshot contains at most seven numeric setup durations, never conversation data.
type voiceFeedback struct {
	mu           sync.Mutex
	ctx          context.Context
	report       *log.Logger
	started      time.Time
	phase        feedbackPhase
	pending      feedbackPhase
	ready        bool
	stopped      bool
	reported     int
	playback     atomic.Int64
	led          *stationLED
	setup        setupTrace
	persist      func(setupSnapshot)
	persistEnd   func(callEndReason)
	nativeRevoke func(error)
	redSeen      bool
	persistState func(callStateSnapshot)
	callSerial   uint64
	callEnded    bool
	health       *audioHealth
	persistAudio func(audioHealthSnapshot)
	idle         *voiceIdle
}

func newVoiceFeedback(ctx context.Context, report *log.Logger, persist ...func(setupSnapshot)) *voiceFeedback {
	f := &voiceFeedback{ctx: ctx, report: report, started: time.Now(), persist: lastSetupSink.offer, persistEnd: lastCallEndedSink.offer, persistState: lastCallStateSink.offer, health: newAudioHealth(), persistAudio: lastAudioHealthSink.offer}
	f.idle = newVoiceIdle(ctx, f.started)
	if len(persist) > 0 {
		f.persist = persist[0]
		f.persistEnd = nil
		f.persistState = nil
		f.persistAudio = nil
	}
	return f
}

func (f *voiceFeedback) nativeRed() {
	f.mu.Lock()
	cause := context.Cause(f.ctx)
	if f.redSeen || f.nativeRevoke == nil || cause != nil && !errors.Is(cause, errAlicePriority) {
		f.mu.Unlock()
		return
	}
	f.redSeen = true
	revoke, persist := f.nativeRevoke, f.persistEnd
	f.mu.Unlock()
	// Use the existing full-epoch native-priority teardown, not the LED context.
	if cause == nil {
		revoke(errAlicePriority)
	}
	if persist != nil {
		persist(callEndedNativeRed)
	}
}

func (f *voiceFeedback) mark(phase feedbackPhase) {
	f.markAt(phase, time.Now())
}

func (f *voiceFeedback) markAt(phase feedbackPhase, now time.Time) {
	if f == nil || phase.name() == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	elapsed := max(0, now.Sub(f.started).Milliseconds())
	if f.stopped {
		return
	}
	if f.ctx.Err() != nil || phase == feedbackStopped {
		f.stopped = true
		f.pending = feedbackNone
		f.emitAt(feedbackStopped, elapsed, now)
		return
	}
	if phase >= feedbackServerIdle && phase <= feedbackServerResponding && !f.ready {
		f.pending = phase
		return
	}
	f.emitAt(phase, elapsed, now)
	if phase == feedbackMediaReady {
		f.ready = true
		if f.pending != feedbackNone {
			f.emitAt(f.pending, elapsed, now)
			f.pending = feedbackNone
		}
	}
}

func (f *voiceFeedback) emitAt(phase feedbackPhase, elapsed int64, now time.Time) {
	f.idle.observe(phase, now)
	if f.phase != phase {
		f.phase = phase
		if phase == feedbackMediaReady && f.callSerial != 0 && !f.callEnded && f.persistState != nil {
			f.persistState(callStateSnapshot{serial: f.callSerial, phase: callActive, reason: callReasonReady})
		}
		if phase == feedbackMediaReady && f.health != nil {
			f.health.active.Store(true)
		}
		if snapshot, done := f.setup.observe(phase, elapsed); done && f.persist != nil {
			f.persist(snapshot) // The production sink only queues; it performs no I/O here.
		}
		if f.led != nil {
			f.led.set(phase)
		}
		// Reserve the final line for release even on a very long call or a
		// server alternating known display states. No unbounded state ledger.
		if phase != feedbackStopped && f.reported >= 127 {
			return
		}
		f.report.Printf("VOICE_PHASE %s elapsed_ms=%d", phase.name(), elapsed)
		f.reported++
	}
}
