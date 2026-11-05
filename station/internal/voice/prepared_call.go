package voice

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// preparedCall owns an offered local peer and the website's SDP answer. Only
// the admitted call may install that answer and start the media transport.
type preparedCall struct {
	peer   *peer
	answer string
	cancel context.CancelCauseFunc
	once   sync.Once
	err    error
}

// Close also works for a test fixture with only peer and answer populated.
// Cancellation callbacks and the admitted owner may close concurrently.
func (p *preparedCall) Close() error {
	if p == nil {
		return nil
	}
	p.once.Do(func() {
		if p.cancel != nil {
			p.cancel(context.Canceled)
		}
		if p.peer != nil && p.peer.close != nil {
			p.err = p.peer.close()
		}
	})
	return p.err
}

// Per-call dependencies keep offline tests away from ICE, sockets and account
// requests without mutable package-wide hooks.
type webCallPreparationDeps struct {
	newPeer func(context.Context, *log.Logger, *voiceFeedback) (*peer, error)
	offer   func(context.Context, *peer) (string, error)
	client  func() *http.Client
}

func prepareWebCall(ctx context.Context, auth credential, settings sessionSettings, feedback *voiceFeedback) (*preparedCall, error) {
	return prepareWebCallWith(ctx, auth, settings, feedback, webCallPreparationDeps{
		newPeer: func(ctx context.Context, report *log.Logger, feedback *voiceFeedback) (*peer, error) {
			return newPeer(ctx, webrtc.SettingEngine{}, []webrtc.ICEServer{{URLs: []string{
				"stun:stun.l.google.com:19302", "stun:stun4.l.google.com:19302",
			}}}, report, feedback)
		},
		offer:  func(ctx context.Context, p *peer) (string, error) { return p.offer(ctx) },
		client: signalingClient,
	})
}

func prepareWebCallWith(ctx context.Context, auth credential, settings sessionSettings, feedback *voiceFeedback, deps webCallPreparationDeps) (_ *preparedCall, result error) {
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	if !validCredential(auth) {
		return nil, errors.New("invalid web credential")
	}
	// This context has no setup/pending TTL deadline of its own. It remains the
	// peer's lifetime parent after admission; Close can still cancel it early.
	lifetime, cancel := context.WithCancelCause(ctx)
	prepared := &preparedCall{cancel: cancel}
	keep := false
	defer func() {
		if !keep {
			result = errors.Join(result, prepared.Close())
		}
	}()
	report := log.New(io.Discard, "", 0)
	if feedback != nil && feedback.report != nil {
		report = feedback.report
	}
	p, err := deps.newPeer(lifetime, report, feedback)
	if err != nil {
		return nil, err
	}
	prepared.peer = p
	// Register only after peer publication. The callback uses Close, whose
	// fields are now immutable; manual Close cancels both callback contexts.
	context.AfterFunc(lifetime, func() { _ = prepared.Close() })
	context.AfterFunc(p.ctx, func() { cancel(context.Cause(p.ctx)) })
	setup, finishSetup := context.WithTimeout(lifetime, setupTimeout)
	defer finishSetup()
	offer, err := deps.offer(setup, p)
	if err != nil {
		return nil, err
	}
	if cause := context.Cause(setup); cause != nil {
		return nil, cause
	}
	feedback.mark(feedbackOfferReady)
	session, err := encodeSession(time.Now(), settings)
	if err != nil {
		return nil, errors.New("cannot encode voice session")
	}
	client := deps.client()
	defer client.CloseIdleConnections()
	answer, err := exchangeOffer(setup, client, auth, offer, session)
	if err != nil {
		return nil, err
	}
	if cause := context.Cause(setup); cause != nil {
		return nil, cause
	}
	if cause := context.Cause(p.ctx); cause != nil {
		return nil, cause
	}
	prepared.answer = answer
	feedback.mark(feedbackAnswerReady)
	keep = true
	return prepared, nil
}
