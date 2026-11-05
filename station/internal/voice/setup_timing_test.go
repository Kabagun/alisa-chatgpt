package voice

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func completeSetupFixture() setupSnapshot {
	var trace setupTrace
	var snapshot setupSnapshot
	for i := 0; i < setupStageCount; i++ {
		snapshot, _ = trace.observe(feedbackTargetMatched+feedbackPhase(i), int64(i*100))
	}
	return snapshot
}

func TestSetupTraceIsBoundedAndOneShot(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var saved []setupSnapshot
	f := newVoiceFeedback(ctx, log.New(io.Discard, "", 0), func(s setupSnapshot) { saved = append(saved, s) })
	for _, p := range []feedbackPhase{feedbackTargetMatched, feedbackTargetMatched, feedbackNativeIdleReady, feedbackCaptureReady, feedbackOfferReady, feedbackServerListening, feedbackAnswerReady, feedbackTransportReady, feedbackMediaReady, feedbackServerSpeaking, feedbackStopped, feedbackMediaReady} {
		f.mark(p)
	}
	if len(saved) != 1 || !saved[0].ready || saved[0].seen != 127 {
		t.Fatal("setup did not persist exactly seven known milestones once")
	}
	var wire map[string]jsontext.Value
	if json.Unmarshal(saved[0].json(), &wire) != nil || len(wire) != 2 || string(wire["status"]) != `"ready"` {
		t.Fatal("unexpected setup JSON fields")
	}
	var stages map[string]int64
	if json.Unmarshal(wire["elapsed_ms"], &stages) != nil || len(stages) != setupStageCount {
		t.Fatal("timing JSON is not bounded to the seven stages")
	}
	for i := 0; i < setupStageCount; i++ {
		if v, ok := stages[(feedbackTargetMatched + feedbackPhase(i)).name()]; !ok || v < 0 {
			t.Fatal("unknown or negative setup stage")
		}
	}
}

func TestSetupIncompleteUsesOnlyKnownErrorStage(t *testing.T) {
	for completed := 0; completed < setupStageCount; completed++ {
		var trace setupTrace
		for i := 0; i < completed; i++ {
			trace.observe(feedbackTargetMatched+feedbackPhase(i), int64(i*100))
		}
		snapshot, done := trace.observe(feedbackStopped, 999)
		if !done || snapshot.ready {
			t.Fatal("incomplete setup was not captured")
		}
		var wire struct {
			Status     string           `json:"status"`
			ErrorStage string           `json:"error_stage"`
			Elapsed    map[string]int64 `json:"elapsed_ms"`
		}
		if json.Unmarshal(snapshot.json(), &wire) != nil || wire.Status != "incomplete" || wire.ErrorStage != (feedbackTargetMatched+feedbackPhase(completed)).name() || len(wire.Elapsed) != completed {
			t.Fatal("failure exposed an arbitrary error or an unknown stage")
		}
		if _, again := trace.observe(feedbackMediaReady, 1000); again {
			t.Fatal("late setup overwrote terminal timing")
		}
	}
}

func TestSetupTimingBlockedFailureNeverBlocksOffer(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	sink := newSetupTimingSink(func(setupSnapshot) bool {
		once.Do(func() { close(entered) })
		<-release
		return false
	})
	sink.offer(completeSetupFixture())
	<-entered
	queued := make(chan struct{})
	go func() {
		for range 1000 {
			sink.offer(completeSetupFixture())
		}
		close(queued)
	}()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("blocked timing I/O blocked Voice's enqueue path")
	}
	if cap(sink.pending) != 1 || len(sink.pending) > 1 {
		t.Fatal("timing queue can grow without bound")
	}
	close(sink.pending)
	close(release)
	<-sink.done
}

func TestSetupTimingPrivateAtomicReplacement(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("cannot prepare private fixture")
	}
	snapshot := completeSetupFixture()
	if !writeSetupTiming(dir, snapshot) {
		t.Fatal("private atomic timing write failed")
	}
	path := filepath.Join(dir, setupTimingLeaf)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("timing leaf is not a private regular file")
	}
	stop, readDone, invalid := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	go func() {
		defer close(readDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(path)
			var v map[string]jsontext.Value
			if err != nil || json.Unmarshal(b, &v) != nil || len(v) != 2 {
				select {
				case invalid <- struct{}{}:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 25; i++ {
		snapshot.elapsed[4]++
		if !writeSetupTiming(dir, snapshot) {
			close(stop)
			<-readDone
			t.Fatal("atomic replacement failed")
		}
	}
	close(stop)
	<-readDone
	if len(invalid) != 0 {
		t.Fatal("reader saw a partial or missing timing file")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != setupTimingLeaf {
		t.Fatal("timing writes accumulated temporary files")
	}
}

func TestSetupTimingRefusesUnsafeParentAndLeaf(t *testing.T) {
	for _, kind := range []string{"missing-parent", "public-parent", "parent-link", "leaf-link", "leaf-hardlink", "public-leaf", "directory-leaf", "fifo-leaf", "large-leaf"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "private")
			if os.Mkdir(dir, 0700) != nil {
				t.Fatal("cannot prepare fixture")
			}
			leaf := filepath.Join(dir, setupTimingLeaf)
			target := filepath.Join(base, "untouched")
			if os.WriteFile(target, []byte("preserved-fixture"), 0600) != nil {
				t.Fatal("cannot prepare target")
			}
			var err error
			switch kind {
			case "missing-parent":
				err = os.Remove(dir)
			case "public-parent":
				err = os.Chmod(dir, 0755)
			case "parent-link":
				link := filepath.Join(base, "parent-link")
				err = os.Symlink(dir, link)
				dir = link
			case "leaf-link":
				err = os.Symlink(target, leaf)
			case "leaf-hardlink":
				err = os.Link(target, leaf)
			case "public-leaf":
				err = os.WriteFile(leaf, []byte("preserved-fixture"), 0644)
				if err == nil {
					err = os.Chmod(leaf, 0644)
				}
			case "directory-leaf":
				err = os.Mkdir(leaf, 0700)
			case "fifo-leaf":
				err = unix.Mkfifo(leaf, 0600)
			case "large-leaf":
				err = os.WriteFile(leaf, []byte(strings.Repeat("x", 2049)), 0600)
			}
			if err != nil {
				t.Fatal("cannot prepare unsafe fixture")
			}
			if writeSetupTiming(dir, completeSetupFixture()) {
				t.Fatal("unsafe path was accepted")
			}
			b, err := os.ReadFile(target)
			if err != nil || string(b) != "preserved-fixture" {
				t.Fatal("unsafe target was modified")
			}
		})
	}
}

func TestSetupTimingOwnerAndModeValidation(t *testing.T) {
	st := unix.Stat_t{Mode: unix.S_IFDIR | 0700, Uid: uint32(os.Geteuid())}
	if !privateTimingDirectory(&st) {
		t.Fatal("private owned directory was refused")
	}
	st.Uid++
	if privateTimingDirectory(&st) {
		t.Fatal("other owner was accepted")
	}
	st.Uid--
	st.Mode |= 0020
	if privateTimingDirectory(&st) {
		t.Fatal("group writable directory was accepted")
	}
}

func TestSetupTimingAllowsOverlappedStageOrder(t *testing.T) {
	origin := time.Now().Add(-3 * time.Second)
	var saved setupSnapshot
	f := newVoiceFeedback(t.Context(), log.New(io.Discard, "", 0), func(s setupSnapshot) { saved = s })
	f.started = origin
	for _, step := range []struct {
		phase feedbackPhase
		ms    int64
	}{
		{feedbackTargetMatched, 0}, {feedbackOfferReady, 100}, {feedbackAnswerReady, 900},
		{feedbackNativeIdleReady, 1200}, {feedbackCaptureReady, 1201}, {feedbackTransportReady, 1500}, {feedbackMediaReady, 1800},
	} {
		f.markAt(step.phase, origin.Add(time.Duration(step.ms)*time.Millisecond))
	}
	if !saved.ready || saved.seen != 127 || saved.elapsed[0] != 0 || saved.elapsed[1] != 1200 || saved.elapsed[3] != 100 || saved.elapsed[4] != 900 {
		t.Fatal("overlap timestamps were sorted, reset or lost")
	}
}

func TestSetupNativeRedProofUsesPrivateAtomicLeaf(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("cannot prepare private fixture")
	}
	if !writePrivateSnapshot(dir, callEndedLeaf, []byte("{\"reason\":\"native_red\"}\n")) {
		t.Fatal("fixed proof write failed")
	}
	info, err := os.Lstat(filepath.Join(dir, callEndedLeaf))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("proof is not private")
	}
	if writePrivateSnapshot(dir, "../other", []byte("{}")) {
		t.Fatal("unapproved leaf was accepted")
	}
	b, err := os.ReadFile(filepath.Join(dir, callEndedLeaf))
	if err != nil || string(b) != "{\"reason\":\"native_red\"}\n" {
		t.Fatal("proof contains unexpected data")
	}
}
