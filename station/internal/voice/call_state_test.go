package voice

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCallStateOrderedAcrossCallsAndLateCallbacks(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	writes := make(chan callStateSnapshot, 8)
	var once sync.Once
	sink := newCallStateSink(func(s callStateSnapshot) bool {
		once.Do(func() { close(entered); <-release })
		writes <- s
		return false
	})
	sink.offer(callStateSnapshot{serial: 1, phase: callActive, reason: callReasonReady})
	<-entered
	done := make(chan struct{})
	go func() {
		sink.offer(callStateSnapshot{serial: 1, phase: callEnded, reason: callReasonNormal, peerClosed: true})
		sink.offer(callStateSnapshot{serial: 2, phase: callActive, reason: callReasonReady})
		sink.offer(callStateSnapshot{serial: 1, phase: callEnded, reason: callReasonNativePriority, peerClosed: true})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("metadata I/O blocked call state offers")
	}
	close(release)
	first, second := <-writes, <-writes
	if first.serial != 1 || second.serial != 2 || second.phase != callActive {
		t.Fatal("stale ending displaced new active state")
	}
	sink.offer(callStateSnapshot{serial: 2, phase: callEnded, reason: callReasonNativePriority, peerClosed: true})
	sink.offer(callStateSnapshot{serial: 2, phase: callActive, reason: callReasonReady})
	last := <-writes
	if last.phase != callEnded || last.serial != 2 {
		t.Fatal("late active callback reopened ended call")
	}
	close(sink.wake)
	<-sink.done
	if len(writes) != 0 {
		t.Fatal("stale snapshots reached the writer")
	}
}

func TestCallStateLifecycleAndClosedPeerProof(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	f := newVoiceFeedback(ctx, log.New(io.Discard, "", 0), nil)
	var saved []callStateSnapshot
	f.persistState = func(s callStateSnapshot) { saved = append(saved, s) }
	f.beginCall()
	f.mark(feedbackMediaReady)
	cancel(errAlicePriority)
	f.endCall(errAlicePriority, true)
	f.endCall(nil, true)
	if len(saved) != 2 || saved[0].phase != callActive || saved[1].phase != callEnded || saved[1].reason != callReasonNativePriority || !saved[1].peerClosed {
		t.Fatal("call lifecycle or teardown proof changed")
	}
	if saved[0].serial != saved[1].serial {
		t.Fatal("one call got multiple ordering tickets")
	}
	var wire map[string]any
	if json.Unmarshal(saved[1].json(), &wire) != nil || len(wire) != 3 || wire["state"] != "ended" || wire["peer_closed"] != true {
		t.Fatal("proof schema changed")
	}
	if strings.Contains(string(saved[1].json()), "serial") {
		t.Fatal("internal identity leaked")
	}
	unknown := callStateSnapshot{serial: 3, phase: callEnded, reason: callReasonCleanupFailed}
	if json.Unmarshal(unknown.json(), &wire) != nil || wire["state"] != "unknown" || wire["peer_closed"] != false {
		t.Fatal("failed cleanup claimed peer closure")
	}
}

func TestCallStatePrivateAtomicLeaf(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("fixture")
	}
	snapshot := callStateSnapshot{serial: 7, phase: callEnded, reason: callReasonNativeRed, peerClosed: true}
	if !writePrivateSnapshot(dir, callStateLeaf, snapshot.json()) {
		t.Fatal("private marker write failed")
	}
	info, err := os.Lstat(filepath.Join(dir, callStateLeaf))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("marker is not private")
	}
	target := filepath.Join(t.TempDir(), "absent-target")
	if os.Remove(filepath.Join(dir, callStateLeaf)) != nil || os.Symlink(target, filepath.Join(dir, callStateLeaf)) != nil {
		t.Fatal("fixture")
	}
	if writePrivateSnapshot(dir, callStateLeaf, snapshot.json()) {
		t.Fatal("marker followed unsafe leaf")
	}
	if _, err := os.Stat(target); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unexpected fixture target")
	}
}

func TestCallStateIdleTimeoutAndNativePriority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result error
		closed bool
		want   callStateReason
	}{
		{"idle", nil, true, callReasonIdleTimeout},
		{"native_priority", errAlicePriority, true, callReasonNativePriority},
		{"cleanup_failure", nil, false, callReasonCleanupFailed},
		{"real_failure", errors.New("synthetic transport failure"), true, callReasonConnectionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Prepared feedback may keep its own live parent context while the
			// admitted epoch has already timed out and normalized its result.
			f := newVoiceFeedback(t.Context(), log.New(io.Discard, "", 0), nil)
			var saved []callStateSnapshot
			f.persistState = func(s callStateSnapshot) { saved = append(saved, s) }
			f.beginCall()
			f.mark(feedbackMediaReady)
			f.idle.timedOut.Store(true)
			f.endCall(tc.result, tc.closed)
			if len(saved) != 2 || saved[1].reason != tc.want || saved[1].phase != callEnded {
				t.Fatal("idle ending lost its reason or overrode native/cleanup priority")
			}
			var wire map[string]any
			if json.Unmarshal(saved[1].json(), &wire) != nil || len(wire) != 3 || wire["peer_closed"] != tc.closed {
				t.Fatal("idle ending lost bounded teardown proof")
			}
		})
	}
}
