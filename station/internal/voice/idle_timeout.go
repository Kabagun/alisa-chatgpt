package voice

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

const voiceIdleTimeout = time.Minute

var errVoiceIdleTimeout = errors.New("voice waiting timeout")

// Only fixed state and monotonic activity times survive. Audio producers update
// one atomic; they never take a lock, retain samples, log or perform I/O here.
type voiceIdle struct {
	ctx      context.Context
	origin   time.Time
	activity atomic.Int64
	timedOut atomic.Bool
	mu       sync.Mutex
	ready    bool
	busy     bool
	waiting  int64
}

func newVoiceIdle(ctx context.Context, now time.Time) *voiceIdle {
	i := &voiceIdle{ctx: ctx, origin: now}
	i.activity.Store(-1)
	return i
}

func (i *voiceIdle) elapsed(now time.Time) int64 {
	return max(0, now.Sub(i.origin).Nanoseconds())
}

func (i *voiceIdle) activityAt(now time.Time) {
	if i == nil || i.ctx.Err() != nil {
		return
	}
	n := i.elapsed(now)
	for old := i.activity.Load(); n > old && !i.activity.CompareAndSwap(old, n); old = i.activity.Load() {
	}
}

func (i *voiceIdle) observe(phase feedbackPhase, now time.Time) {
	if i == nil || i.ctx.Err() != nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	switch phase {
	case feedbackMediaReady:
		if !i.ready {
			i.ready, i.waiting = true, i.elapsed(now)
		}
	case feedbackServerSpeaking, feedbackServerResponding:
		i.busy = true
	case feedbackServerIdle, feedbackServerListening:
		if i.busy {
			// A completed reply/work period starts a fresh minute of waiting.
			// Repeated waiting-state messages cannot keep the call alive.
			i.waiting = i.elapsed(now)
		}
		i.busy = false
	case feedbackStopped:
		i.ready = false
	}
}

func (i *voiceIdle) expiredAt(now time.Time) bool {
	if i == nil || i.ctx.Err() != nil {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.ready && !i.busy && i.elapsed(now)-max(i.waiting, i.activity.Load()) >= int64(voiceIdleTimeout)
}

func (i *voiceIdle) watch(ctx context.Context, ticks <-chan time.Time, revoke func(error)) {
	if i == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-i.ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			if i.expiredAt(now) && i.ctx.Err() == nil && ctx.Err() == nil {
				i.timedOut.Store(true)
				revoke(errVoiceIdleTimeout)
				return
			}
		}
	}
}

func (i *voiceIdle) run(ctx context.Context, revoke func(error)) {
	if i == nil {
		return
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	i.watch(ctx, ticker.C, revoke)
}
