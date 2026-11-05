package voice

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

func TestVoiceIdleInitialSilenceAndRepeatedWaitingStates(t *testing.T) {
	now := time.Unix(1, 0)
	i := newVoiceIdle(t.Context(), now)
	if i.expiredAt(now.Add(time.Hour)) {
		t.Fatal("preparation started the waiting timer")
	}
	i.observe(feedbackMediaReady, now.Add(10*time.Second))
	for _, elapsed := range []time.Duration{20 * time.Second, 40 * time.Second, 69 * time.Second} {
		i.observe(feedbackServerListening, now.Add(elapsed))
		i.observe(feedbackServerIdle, now.Add(elapsed))
		i.observe(feedbackMediaReady, now.Add(elapsed))
		i.observe(feedbackNone, now.Add(elapsed))
	}
	if i.expiredAt(now.Add(70*time.Second-time.Nanosecond)) || !i.expiredAt(now.Add(70*time.Second)) {
		t.Fatal("initial waiting was not exactly one minute after media readiness")
	}
}

func TestVoiceIdleSpeechReplyAndLongBusyPeriods(t *testing.T) {
	now := time.Unix(1, 0)
	i := newVoiceIdle(t.Context(), now)
	i.observe(feedbackMediaReady, now)
	i.microphone([]int16{1000}, now.Add(50*time.Second))
	if i.expiredAt(now.Add(109*time.Second)) || !i.expiredAt(now.Add(110*time.Second)) {
		t.Fatal("real microphone speech did not restart the minute")
	}
	i.observe(feedbackServerResponding, now.Add(80*time.Second))
	if i.expiredAt(now.Add(10 * time.Minute)) {
		t.Fatal("long assistant work was timed out as user silence")
	}
	i.observe(feedbackServerSpeaking, now.Add(10*time.Minute))
	i.activityAt(now.Add(11 * time.Minute))
	if i.expiredAt(now.Add(12 * time.Minute)) {
		t.Fatal("long explicit speaking state was cut off")
	}
	i.observe(feedbackServerListening, now.Add(12*time.Minute))
	// The last audible callback can follow the state transition because of
	// buffered playback. It earns the full minute too.
	i.activityAt(now.Add(12*time.Minute + 5*time.Second))
	i.observe(feedbackServerListening, now.Add(13*time.Minute))
	if i.expiredAt(now.Add(13*time.Minute+5*time.Second-time.Nanosecond)) || !i.expiredAt(now.Add(13*time.Minute+5*time.Second)) {
		t.Fatal("reply tail or repeated waiting state changed the fresh quiet minute")
	}
}

func TestVoiceIdleExistingToolEventsPauseUntilExplicitWaiting(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	f := newVoiceFeedback(ctx, log.New(io.Discard, "", 0), nil)
	now := f.started
	f.markAt(feedbackMediaReady, now)
	p := &peer{ctx: ctx, cancel: cancel, feedback: f}
	for _, typ := range []string{"tool_update", "client_tool_invoke", "client_tool_update", "client_tool_result"} {
		p.handleMessage([]byte(`{"type":"` + typ + `","payload":{"status":"synthetic-unqualified-status"}}`))
		if f.idle.expiredAt(now.Add(time.Hour)) {
			t.Fatal("private tool event failed to pause waiting")
		}
	}
	p.handleMessage([]byte(`{"type":"unknown_keepalive"}`))
	if f.idle.expiredAt(now.Add(2 * time.Hour)) {
		t.Fatal("unknown traffic cleared an explicit busy period")
	}
	f.markAt(feedbackServerListening, now.Add(2*time.Hour))
	if f.idle.expiredAt(now.Add(2*time.Hour+voiceIdleTimeout-time.Nanosecond)) || !f.idle.expiredAt(now.Add(2*time.Hour+voiceIdleTimeout)) {
		t.Fatal("explicit return to listening did not start a fresh minute")
	}
}

func TestVoiceIdleEpochTimeoutIsNonfatalAndNextActivationWorks(t *testing.T) {
	now := time.Unix(1, 0)
	g := newStationGate()
	g.observe(nativeEvent{stateChanged: true, state: aliceIdle})
	e := g.begin(t.Context())
	if e == nil {
		t.Fatal("first activation fixture")
	}
	i := newVoiceIdle(e.ctx, now)
	i.observe(feedbackMediaReady, now)
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); i.watch(e.ctx, ticks, e.revoke) }()
	ticks <- now.Add(voiceIdleTimeout)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fake clock timeout failed to stop the epoch worker")
	}
	if e.allowed() || !errors.Is(context.Cause(e.ctx), errVoiceIdleTimeout) || !i.timedOut.Load() {
		t.Fatal("idle timeout did not revoke the full audio/capture epoch")
	}
	p := &peer{}
	if result := g.finish(e, p.outcome(context.Cause(e.ctx))); result != nil {
		t.Fatal("idle ending became fatal", result)
	}
	next := g.begin(t.Context())
	if next == nil || !next.allowed() || next.number == e.number {
		t.Fatal("idle ending prevented the next wake activation")
	}
	defer g.finish(next, context.Canceled)
	fresh := newVoiceIdle(next.ctx, now.Add(time.Hour))
	fresh.observe(feedbackMediaReady, now.Add(time.Hour))
	if fresh.expiredAt(now.Add(time.Hour + voiceIdleTimeout - time.Nanosecond)) {
		t.Fatal("previous call's deadline leaked into a new activation")
	}
}

func TestVoiceIdleAdmittedEpochCancellationDominatesPreparedFeedback(t *testing.T) {
	for _, cause := range []error{context.Canceled, errAlicePriority} {
		t.Run(cause.Error(), func(t *testing.T) {
			now := time.Unix(1, 0)
			// A prepared feedback context can outlive its admitted epoch.
			i := newVoiceIdle(t.Context(), now)
			i.observe(feedbackMediaReady, now)
			ctx, cancel := context.WithCancelCause(t.Context())
			cancel(cause)
			ticks := make(chan time.Time, 1)
			ticks <- now.Add(time.Hour)
			done := make(chan struct{})
			go func() {
				defer close(done)
				i.watch(ctx, ticks, func(error) { t.Error("idle timeout replaced native/cancel ending") })
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("admitted cancellation left idle worker blocking normal teardown")
			}
			if i.timedOut.Load() || context.Cause(ctx) != cause {
				t.Fatal("cancellation lost its cause")
			}
		})
	}
}

func TestVoiceIdleConcurrentActivityAndStateObservation(t *testing.T) {
	now := time.Unix(1, 0)
	i := newVoiceIdle(t.Context(), now)
	i.observe(feedbackMediaReady, now)
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for n := range 1000 {
				i.activityAt(now.Add(time.Duration(n) * time.Millisecond))
				i.observe(feedbackServerListening, now)
				_ = i.expiredAt(now.Add(time.Second))
			}
		})
	}
	workers.Wait()
	if i.activity.Load() != int64(999*time.Millisecond) {
		t.Fatal("concurrent producers moved activity backwards")
	}
}
