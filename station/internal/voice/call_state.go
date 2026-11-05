package voice

import (
	"context"
	"encoding/json/v2"
	"errors"
	"sync"
	"sync/atomic"
)

const callStateLeaf = "last-call-state.json"

type callStatePhase uint8

const (
	callActive callStatePhase = 1
	callEnded  callStatePhase = 2
)

type callStateReason uint8

const (
	callReasonReady callStateReason = iota
	callReasonNormal
	callReasonNativePriority
	callReasonNativeRed
	callReasonSetupFailed
	callReasonConnectionFailed
	callReasonCanceled
	callReasonCleanupFailed
	callReasonIdleTimeout
)

type callStateSnapshot struct {
	serial     uint64 // Internal ordering only; never serialized.
	phase      callStatePhase
	reason     callStateReason
	peerClosed bool
}

func (s callStateSnapshot) json() []byte {
	reasons := [...]string{"media_ready", "normal", "native_priority", "native_red", "setup_failed", "connection_failed", "canceled", "cleanup_failed", "idle_timeout"}
	if s.reason > callReasonIdleTimeout || s.phase != callActive && s.phase != callEnded {
		return nil
	}
	state := "active"
	if s.phase == callEnded {
		state = "ended"
	}
	if s.phase == callEnded && !s.peerClosed {
		state = "unknown"
	}
	b, _ := json.Marshal(struct {
		State      string `json:"state"`
		Reason     string `json:"reason"`
		PeerClosed bool   `json:"peer_closed"`
	}{state, reasons[s.reason], s.peerClosed})
	return append(b, '\n')
}

// One serialized writer, one latest value. The high-water key rejects stale
// previous-call endings and same-call late active callbacks before they queue.
type orderedCallStateSink struct {
	mu     sync.Mutex
	once   sync.Once
	wake   chan struct{}
	done   chan struct{}
	latest callStateSnapshot
	write  func(callStateSnapshot) bool
}

func newCallStateSink(write func(callStateSnapshot) bool) *orderedCallStateSink {
	return &orderedCallStateSink{wake: make(chan struct{}, 1), done: make(chan struct{}), write: write}
}

func (s *orderedCallStateSink) offer(snapshot callStateSnapshot) {
	s.mu.Lock()
	if snapshot.serial == 0 || snapshot.phase != callActive && snapshot.phase != callEnded || snapshot.reason > callReasonIdleTimeout || snapshot.serial < s.latest.serial || snapshot.serial == s.latest.serial && snapshot.phase <= s.latest.phase {
		s.mu.Unlock()
		return
	}
	s.latest = snapshot
	s.once.Do(func() {
		go func() {
			defer close(s.done)
			for range s.wake {
				s.mu.Lock()
				value := s.latest
				s.mu.Unlock()
				_ = s.write(value)
			}
		}()
	})
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.mu.Unlock()
}

var callStateSerial atomic.Uint64
var lastCallStateSink = newCallStateSink(func(s callStateSnapshot) bool {
	return writePrivateSnapshot(setupTimingDirectory, callStateLeaf, s.json())
})

func (f *voiceFeedback) beginCall() {
	if f == nil {
		return
	}
	f.mu.Lock()
	if f.callSerial == 0 && f.persistState != nil {
		f.callSerial = callStateSerial.Add(1)
	}
	health, persistAudio := f.health, f.persistAudio
	f.mu.Unlock()
	health.begin(persistAudio)
}

func (f *voiceFeedback) endCall(result error, peerClosed bool) {
	if f == nil {
		return
	}
	defer f.health.finish(peerClosed)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.callSerial == 0 || f.callEnded || f.persistState == nil {
		return
	}
	f.callEnded = true
	reason := callReasonNormal
	switch {
	case !peerClosed:
		reason = callReasonCleanupFailed
	case f.redSeen:
		reason = callReasonNativeRed
	case errors.Is(context.Cause(f.ctx), errAlicePriority), errors.Is(result, errAlicePriority):
		reason = callReasonNativePriority
	case result == nil && (errors.Is(context.Cause(f.ctx), errVoiceIdleTimeout) || f.idle != nil && f.idle.timedOut.Load()):
		reason = callReasonIdleTimeout
	case result == nil:
		reason = callReasonNormal
	case errors.Is(result, context.Canceled):
		reason = callReasonCanceled
	case !f.ready:
		reason = callReasonSetupFailed
	default:
		reason = callReasonConnectionFailed
	}
	f.persistState(callStateSnapshot{serial: f.callSerial, phase: callEnded, reason: reason, peerClosed: peerClosed})
}
