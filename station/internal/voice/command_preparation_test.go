package voice

import (
	"context"
	"errors"
	"io"
	"log"
	"sync/atomic"
	"testing"
	"time"
)

func preparationGate(t *testing.T, prepare prepareCallFunc) (*stationGate, *commandPreparer) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	g := commandGate()
	p := &commandPreparer{gate: g, ctx: ctx, report: log.New(io.Discard, "", 0), prepare: prepare,
		feedback: func(ctx context.Context, report *log.Logger) *voiceFeedback {
			return newVoiceFeedback(ctx, report, nil)
		}}
	g.preparation = p
	t.Cleanup(func() { cancel(); p.close() })
	return g, p
}

func freshPreparationTarget(g *stationGate, now time.Time) {
	g.observeAt(nativeEvent{stateChanged: true, state: aliceListening}, now)
	g.observeAt(commandEvent(), now.Add(time.Millisecond))
}

func pendingCandidate(g *stationGate) *commandCandidate {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.preparation.current
}

func waitPreparation(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("preparation did not complete or clean up")
	}
}

func TestCommandPreparationOverlapsSceneButPromotesOnlyStableIdle(t *testing.T) {
	var calls, closed atomic.Int32
	g, p := preparationGate(t, func(ctx context.Context, _ credential, _ sessionSettings, f *voiceFeedback) (*preparedCall, error) {
		calls.Add(1)
		f.mark(feedbackOfferReady)
		f.mark(feedbackAnswerReady)
		return &preparedCall{peer: &peer{ctx: ctx, close: func() error { closed.Add(1); return nil }}, answer: "fixture"}, nil
	})
	now := time.Now()
	g.observeAt(commandEvent(), now) // Cached text has no fresh native generation.
	if calls.Load() != 0 || pendingCandidate(g) != nil {
		t.Fatal("cached text prepared a call")
	}
	freshPreparationTarget(g, now)
	c := pendingCandidate(g)
	if c == nil {
		t.Fatal("fresh exact target did not create preparation")
	}
	waitPreparation(t, c.ready)
	g.observeAt(commandEvent(), now.Add(2*time.Millisecond))
	g.observeAt(nativeEvent{stateChanged: true, state: aliceSpeaking}, now.Add(3*time.Millisecond))
	if calls.Load() != 1 || c.ctx.Err() != nil || g.beginAt(t.Context(), now.Add(time.Second)) != nil {
		t.Fatal("scenario speech canceled preparation, duplicated it or admitted media")
	}
	idle := now.Add(4 * time.Millisecond)
	g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, idle)
	if g.beginAt(t.Context(), idle.Add(commandIdleStability-1)) != nil {
		t.Fatal("stable IDLE guard weakened")
	}
	e := g.beginAt(t.Context(), idle.Add(commandIdleStability))
	if e == nil || e.candidate != c || e.feedback != c.feedback || !e.commandMatched.Equal(now.Add(time.Millisecond)) || e.feedback.led != nil || e.feedback.nativeRevoke != nil || e.output.native != nil {
		t.Fatal("promotion lost origin/candidate or touched pre-gate hardware")
	}
	p.expire(c) // A stale pending TTL callback must not end a promoted call.
	if c.ctx.Err() != nil || !e.allowed() {
		t.Fatal("pending TTL survived promotion")
	}
	g.observeAt(nativeEvent{activation: true}, now.Add(time.Second))
	waitPreparation(t, e.ctx.Done())
	if !errors.Is(context.Cause(e.ctx), errAlicePriority) {
		t.Fatal("active native priority changed")
	}
	if err := g.finish(e, context.Cause(e.ctx)); !errors.Is(err, errAlicePriority) || closed.Load() != 1 {
		t.Fatal("promoted peer did not close exactly once")
	}
}

func TestCommandPreparationExpiresWithoutRetry(t *testing.T) {
	var calls, closed atomic.Int32
	g, p := preparationGate(t, func(ctx context.Context, _ credential, _ sessionSettings, _ *voiceFeedback) (*preparedCall, error) {
		calls.Add(1)
		return &preparedCall{peer: &peer{ctx: ctx, close: func() error { closed.Add(1); return nil }}}, nil
	})
	now := time.Now()
	freshPreparationTarget(g, now)
	c := pendingCandidate(g)
	waitPreparation(t, c.ready)
	p.expire(c)
	waitPreparation(t, c.done)
	g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, now.Add(time.Second))
	if g.beginAt(t.Context(), now.Add(2*time.Second)) != nil || closed.Load() != 1 || !errors.Is(context.Cause(c.ctx), context.DeadlineExceeded) {
		t.Fatal("expired candidate was admitted or leaked")
	}
	g.observeAt(commandEvent(), now.Add(3*time.Second))
	if calls.Load() != 1 {
		t.Fatal("one utterance retried preparation")
	}
}

func TestCommandPreparationReplacementWaitsForCleanup(t *testing.T) {
	var calls, live, maximum atomic.Int32
	closing, release := make(chan struct{}), make(chan struct{})
	g, p := preparationGate(t, func(ctx context.Context, _ credential, _ sessionSettings, _ *voiceFeedback) (*preparedCall, error) {
		n := calls.Add(1)
		active := live.Add(1)
		for old := maximum.Load(); active > old && !maximum.CompareAndSwap(old, active); old = maximum.Load() {
		}
		return &preparedCall{peer: &peer{ctx: ctx, close: func() error {
			if n == 1 {
				close(closing)
				<-release
			}
			live.Add(-1)
			return nil
		}}}, nil
	})
	now := time.Now()
	freshPreparationTarget(g, now)
	old := pendingCandidate(g)
	waitPreparation(t, old.ready)
	g.observeAt(nativeEvent{activation: true}, now.Add(time.Second))
	waitPreparation(t, closing)
	freshPreparationTarget(g, now.Add(2*time.Second))
	if calls.Load() != 1 || pendingCandidate(g) != old {
		t.Fatal("replacement overlapped retired peer cleanup")
	}
	close(release)
	waitPreparation(t, old.done)
	_ = g.beginAt(t.Context(), now.Add(2*time.Second+2*time.Millisecond))
	current := pendingCandidate(g)
	if current == nil || current == old {
		t.Fatal("fresh replacement was lost after cleanup")
	}
	waitPreparation(t, current.ready)
	p.expire(old)
	if current.ctx.Err() != nil || maximum.Load() != 1 {
		t.Fatal("stale expiry canceled replacement or resource bound broke")
	}
}

func TestCommandPreparationPromotionWhileHTTPPendingAndShutdown(t *testing.T) {
	started := make(chan struct{})
	g, _ := preparationGate(t, func(ctx context.Context, _ credential, _ sessionSettings, _ *voiceFeedback) (*preparedCall, error) {
		close(started)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	})
	now := time.Now()
	freshPreparationTarget(g, now)
	waitPreparation(t, started)
	c := pendingCandidate(g)
	idle := now.Add(2 * time.Millisecond)
	g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, idle)
	e := g.beginAt(t.Context(), idle.Add(commandIdleStability))
	if e == nil || e.output.native != nil || e.feedback.led != nil {
		t.Fatal("pending HTTP could not promote cleanly or touched media")
	}
	g.observeAt(nativeEvent{stateChanged: true, state: aliceListening}, now.Add(time.Second))
	if _, err := c.await(e.ctx); !errors.Is(err, errAlicePriority) {
		t.Fatal("native cancellation did not abort admitted wait")
	}
	_ = g.finish(e, context.Cause(e.ctx))
	waitPreparation(t, c.done)
}

func TestCommandPreparationFailureConsumesCandidate(t *testing.T) {
	var calls atomic.Int32
	g, _ := preparationGate(t, func(context.Context, credential, sessionSettings, *voiceFeedback) (*preparedCall, error) {
		calls.Add(1)
		return nil, errors.New("offline fixture failure")
	})
	now := time.Now()
	freshPreparationTarget(g, now)
	c := pendingCandidate(g)
	waitPreparation(t, c.ready)
	g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, now.Add(time.Second))
	if g.beginAt(t.Context(), now.Add(2*time.Second)) != nil {
		t.Fatal("failed preparation reached media")
	}
	waitPreparation(t, c.done)
	_ = g.beginAt(t.Context(), now.Add(3*time.Second))
	if calls.Load() != 1 || g.preparationFailure() == nil {
		t.Fatal("failure retried or was lost")
	}
}
