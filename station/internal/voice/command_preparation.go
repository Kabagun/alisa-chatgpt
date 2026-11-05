package voice

import (
	"context"
	"errors"
	"log"
	"time"
)

type prepareCallFunc func(context.Context, credential, sessionSettings, *voiceFeedback) (*preparedCall, error)

// The gate mutex owns the single pending slot. A retired candidate remains in
// that slot until local HTTP/peer cleanup completes, before any replacement.
type commandPreparer struct {
	gate     *stationGate
	ctx      context.Context
	auth     credential
	settings sessionSettings
	report   *log.Logger
	prepare  prepareCallFunc
	feedback func(context.Context, *log.Logger) *voiceFeedback
	current  *commandCandidate
	failure  error
	fatal    bool
}

type commandCandidate struct {
	owner      *commandPreparer
	generation uint64
	matchedAt  time.Time
	ctx        context.Context
	cancel     context.CancelCauseFunc
	timer      *time.Timer
	ready      chan struct{}
	promoted   chan struct{}
	done       chan struct{}
	feedback   *voiceFeedback
	prepared   *preparedCall // Published by closing ready.
	err        error         // Published by closing ready.
	cleanupErr error         // Published by closing done.
}

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (p *commandPreparer) matches(c *commandCandidate) bool {
	command := p.gate.command
	return command.pending && command.generation == c.generation && command.matchedAt.Equal(c.matchedAt)
}

func (p *commandPreparer) reconcileLocked(now time.Time) {
	if c := p.current; c != nil {
		if channelClosed(c.ready) && c.err == nil && c.prepared != nil && c.prepared.peer.ctx.Err() != nil {
			c.err = context.Cause(c.prepared.peer.ctx)
		}
		if !p.matches(c) {
			c.cancel(errAlicePriority)
		} else if !now.Before(p.gate.command.deadline) {
			p.gate.command.pending = false
			c.cancel(context.DeadlineExceeded)
		} else if channelClosed(c.ready) && c.err != nil {
			p.gate.command.pending = false
			p.failure = c.err
			p.fatal = p.fatal || errors.Is(c.err, errCleanup)
			c.cancel(c.err)
		}
		if c.ctx.Err() != nil && channelClosed(c.done) {
			if c.timer != nil {
				c.timer.Stop()
			}
			p.failure = errors.Join(p.failure, c.cleanupErr)
			p.fatal = p.fatal || errors.Is(c.cleanupErr, errCleanup)
			p.current = nil
		}
	}
	command := p.gate.command
	if p.fatal || p.current != nil || p.gate.current != nil || p.ctx.Err() != nil || !command.pending || command.matchedAt.IsZero() || !now.Before(command.deadline) {
		return
	}
	ctx, cancel := context.WithCancelCause(p.ctx) // No pending TTL deadline survives promotion.
	c := &commandCandidate{owner: p, generation: command.generation, matchedAt: command.matchedAt, ctx: ctx, cancel: cancel,
		ready: make(chan struct{}), promoted: make(chan struct{}), done: make(chan struct{})}
	if p.feedback != nil {
		c.feedback = p.feedback(ctx, p.report)
	} else {
		c.feedback = newVoiceFeedback(ctx, p.report)
	}
	c.feedback.started = c.matchedAt
	c.feedback.markAt(feedbackTargetMatched, c.matchedAt)
	p.current = c
	c.timer = time.AfterFunc(command.deadline.Sub(now), func() { p.expire(c) })
	go c.run()
}

func (p *commandPreparer) expire(c *commandCandidate) {
	p.gate.mu.Lock()
	if p.current == c {
		if p.matches(c) {
			p.gate.command.pending = false
		}
		c.cancel(context.DeadlineExceeded)
	}
	p.gate.mu.Unlock()
	p.gate.wake()
}

func (c *commandCandidate) run() {
	defer func() { close(c.done); c.owner.gate.wake() }()
	c.prepared, c.err = c.owner.prepare(c.ctx, c.owner.auth, c.owner.settings, c.feedback)
	close(c.ready)
	c.owner.gate.wake()
	select {
	case <-c.promoted:
		return // Ownership now belongs to the active epoch, including failures.
	case <-c.ctx.Done():
		c.cleanupErr = c.prepared.Close()
		c.feedback.mark(feedbackStopped)
	}
}

func (p *commandPreparer) admissibleLocked() bool {
	c := p.current
	return c != nil && p.matches(c) && c.ctx.Err() == nil && (!channelClosed(c.ready) || c.err == nil)
}

func (p *commandPreparer) promoteLocked(e *stationEpoch) {
	c := p.current
	c.timer.Stop()
	p.current = nil
	e.candidate, e.feedback = c, c.feedback
	// Establish lifetime cancellation while still holding the admission lock.
	context.AfterFunc(e.ctx, func() { c.cancel(context.Cause(e.ctx)) })
	close(c.promoted)
}

func (c *commandCandidate) await(ctx context.Context) (*preparedCall, error) {
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-c.ready:
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		return c.prepared, c.err
	}
}

func (c *commandCandidate) close(cause error) error {
	c.cancel(cause)
	<-c.done
	err := c.prepared.Close()
	c.feedback.mark(feedbackStopped)
	return errors.Join(c.cleanupErr, err)
}

func (p *commandPreparer) close() {
	p.gate.mu.Lock()
	c := p.current
	p.current = nil
	if c != nil {
		c.timer.Stop()
		c.cancel(context.Canceled)
	}
	p.gate.mu.Unlock()
	if c != nil {
		_ = c.close(context.Canceled)
	}
}

func (g *stationGate) wake() {
	select {
	case g.changed <- struct{}{}:
	default:
	}
}

func (g *stationGate) preparationFailure() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.preparation == nil {
		return nil
	}
	err := g.preparation.failure
	g.preparation.failure = nil
	return err
}
