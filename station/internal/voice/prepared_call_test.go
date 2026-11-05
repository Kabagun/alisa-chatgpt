package voice

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const preparedSyntheticProject = "g-p-00000000000000000000000000000000"

var preparedSyntheticAuth = credential{AccessToken: "prepared-private-fixture", DeviceID: "prepared-offline-device"}

type preparedTransport struct {
	roundTrip func(*http.Request) (*http.Response, error)
	idleClose atomic.Int32
}

func (p *preparedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return p.roundTrip(r)
}

func (p *preparedTransport) CloseIdleConnections() { p.idleClose.Add(1) }

type preparedFixture struct {
	peer       *peer
	lifetime   context.Context
	closes     atomic.Int32
	closed     chan struct{}
	offerCalls atomic.Int32
	clients    atomic.Int32
	transport  *preparedTransport
	deps       webCallPreparationDeps
}

func newPreparedFixture(t *testing.T) *preparedFixture {
	t.Helper()
	f := &preparedFixture{closed: make(chan struct{})}
	f.transport = &preparedTransport{roundTrip: func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("v=0\r\ns=held-answer\r\n")), Header: make(http.Header), Request: r}, nil
	}}
	f.deps = webCallPreparationDeps{
		newPeer: func(ctx context.Context, report *log.Logger, feedback *voiceFeedback) (*peer, error) {
			if report == nil {
				t.Error("missing safe preparation logger")
			}
			f.lifetime = ctx
			peerCtx, cancel := context.WithCancelCause(ctx)
			// No PeerConnection or media objects exist in this fixture. An
			// accidental accept/transport/media call cannot succeed silently.
			f.peer = &peer{ctx: peerCtx, cancel: cancel, feedback: feedback}
			f.peer.close = func() error {
				cancel(context.Canceled)
				if f.closes.Add(1) == 1 {
					close(f.closed)
				}
				return nil
			}
			return f.peer, nil
		},
		offer: func(ctx context.Context, p *peer) (string, error) {
			f.offerCalls.Add(1)
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > setupTimeout {
				t.Error("offer is not bounded by the setup budget")
			}
			if _, ok := p.ctx.Deadline(); ok {
				t.Error("network setup deadline escaped into peer lifetime")
			}
			return "v=0\r\ns=local-offer\r\n", nil
		},
		client: func() *http.Client {
			f.clients.Add(1)
			return &http.Client{Transport: f.transport}
		},
	}
	return f
}

func TestPreparedCallHoldsAnswerWithoutAcceptOrMedia(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newPreparedFixture(t)
	var out bytes.Buffer
	feedback := newVoiceFeedback(ctx, log.New(&out, "", 0), nil)
	settings := sessionSettings{ProjectID: preparedSyntheticProject, BackendThinkingEffort: "disabled", ActivationCommand: "synthetic command", LedFeedback: true}
	f.transport.roundTrip = func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != signalingURL || r.Method != http.MethodPost {
			t.Fatal("preparation changed signaling destination")
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		form := multipart.NewReader(r.Body, params["boundary"])
		fields := map[string]string{}
		for {
			part, err := form.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(part)
			if err != nil {
				t.Fatal(err)
			}
			fields[part.FormName()] = string(data)
		}
		if len(fields) != 2 || fields["sdp"] != "v=0\r\ns=local-offer\r\n" {
			t.Fatal("preparation changed signaling form fields")
		}
		var payload map[string]any
		if json.Unmarshal([]byte(fields["session"]), &payload) != nil {
			t.Fatal("preparation produced invalid session JSON")
		}
		baseline, err := encodeSession(time.Now(), settings)
		if err != nil {
			t.Fatal(err)
		}
		var want map[string]any
		if json.Unmarshal(baseline, &want) != nil {
			t.Fatal("cannot decode baseline session")
		}
		for _, data := range []map[string]any{payload, want} {
			delete(data, "voice_session_id")
			delete(data, "voice_status_request_id")
		}
		if !reflect.DeepEqual(payload, want) {
			t.Fatal("preparation changed project/settings payload shape")
		}
		return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader("v=0\r\ns=held-answer\r\n")), Header: make(http.Header), Request: r}, nil
	}
	prepared, err := prepareWebCallWith(ctx, preparedSyntheticAuth, settings, feedback, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if prepared.peer != f.peer || prepared.answer != "v=0\r\ns=held-answer\r\n" || f.closes.Load() != 0 || f.lifetime.Err() != nil {
		t.Fatal("prepared peer/answer was not retained for admission")
	}
	if f.offerCalls.Load() != 1 || f.clients.Load() != 1 || f.transport.idleClose.Load() != 1 {
		t.Fatal("preparation retried or retained idle HTTP connections")
	}
	if f.peer.pc != nil || f.peer.mic != nil || f.peer.dc != nil || feedback.led != nil || feedback.ready {
		t.Fatal("preparation acquired transport/media/LED readiness")
	}
	if !strings.Contains(out.String(), "offer_ready") || !strings.Contains(out.String(), "answer_ready") || strings.Contains(out.String(), "media_ready") {
		t.Fatal("preparation emitted incorrect local milestones")
	}
	for _, marker := range []string{preparedSyntheticAuth.AccessToken, preparedSyntheticAuth.DeviceID, preparedSyntheticProject, "held-answer", "local-offer"} {
		if strings.Contains(out.String(), marker) {
			t.Fatal("preparation logged private/session data")
		}
	}
}

func TestPreparedCallFailureClosesResourcesAndSanitizesHTTPError(t *testing.T) {
	for _, stage := range []string{"peer", "offer", "HTTP", "bad answer"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := newPreparedFixture(t)
			if stage == "peer" {
				f.deps.newPeer = func(ctx context.Context, _ *log.Logger, _ *voiceFeedback) (*peer, error) {
					f.lifetime = ctx
					return nil, errors.New("cannot create fixture peer")
				}
			} else if stage == "offer" {
				f.deps.offer = func(context.Context, *peer) (string, error) { return "", errors.New("cannot create fixture offer") }
			} else {
				f.transport.roundTrip = func(r *http.Request) (*http.Response, error) {
					if stage == "HTTP" {
						return nil, errors.New(preparedSyntheticAuth.AccessToken + preparedSyntheticProject)
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(preparedSyntheticAuth.AccessToken)), Header: make(http.Header), Request: r}, nil
				}
			}
			prepared, err := prepareWebCallWith(ctx, preparedSyntheticAuth, sessionSettings{}, nil, f.deps)
			if prepared != nil || err == nil || f.lifetime.Err() == nil {
				t.Fatal("failed preparation retained its lifetime/resources")
			}
			if stage != "peer" && f.closes.Load() != 1 {
				t.Fatal("failed preparation did not close peer exactly once")
			}
			if strings.Contains(err.Error(), preparedSyntheticAuth.AccessToken) || strings.Contains(err.Error(), preparedSyntheticProject) {
				t.Fatal("preparation exposed signaling error/answer details")
			}
		})
	}
}

func TestPreparedCallCancellationClosesOfferAndHTTP(t *testing.T) {
	for _, stage := range []string{"offer", "HTTP", "peer failure"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(context.Canceled)
			f := newPreparedFixture(t)
			started := make(chan struct{})
			if stage == "offer" {
				f.deps.offer = func(ctx context.Context, _ *peer) (string, error) {
					close(started)
					<-ctx.Done()
					return "", context.Cause(ctx)
				}
			} else {
				f.transport.roundTrip = func(r *http.Request) (*http.Response, error) {
					close(started)
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
			}
			done := make(chan error, 1)
			go func() {
				prepared, err := prepareWebCallWith(ctx, preparedSyntheticAuth, sessionSettings{}, nil, f.deps)
				if prepared != nil {
					_ = prepared.Close()
				}
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("fixture preparation never started")
			}
			cause := errAlicePriority
			if stage == "peer failure" {
				f.peer.cancel(cause)
			} else {
				cancel(cause)
			}
			select {
			case err := <-done:
				if !errors.Is(err, cause) || f.closes.Load() != 1 {
					t.Fatal("cancellation lost its cause or leaked the peer", err)
				}
			case <-time.After(time.Second):
				t.Fatal("preparation ignored cancellation")
			}
		})
	}
}

func TestPreparedCallCloseAndParentCancellationAreIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newPreparedFixture(t)
	prepared, err := prepareWebCallWith(ctx, preparedSyntheticAuth, sessionSettings{}, nil, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if err := prepared.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	cancel()
	workers.Wait()
	if f.closes.Load() != 1 || f.lifetime.Err() == nil {
		t.Fatal("concurrent cleanup did not close once")
	}
	var empty *preparedCall
	if empty.Close() != nil {
		t.Fatal("nil prepared cleanup failed")
	}
	fixtureCloses := 0
	cleanupErr := errors.New("fixture cleanup failure")
	fixture := &preparedCall{peer: &peer{close: func() error { fixtureCloses++; return cleanupErr }}, answer: "fixture"}
	if !errors.Is(fixture.Close(), cleanupErr) || !errors.Is(fixture.Close(), cleanupErr) || fixtureCloses != 1 {
		t.Fatal("literal prepared fixture lost idempotent cleanup result")
	}
}

func TestPreparedCallParentCancellationAfterSuccessClosesPeer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newPreparedFixture(t)
	prepared, err := prepareWebCallWith(ctx, preparedSyntheticAuth, sessionSettings{}, nil, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	cancel()
	select {
	case <-f.closed:
		if f.closes.Load() != 1 {
			t.Fatal("parent cancellation closed peer multiple times")
		}
	case <-time.After(time.Second):
		t.Fatal("parent cancellation retained prepared peer")
	}
}

func TestPreparedCallRejectsCanceledOrInvalidInputBeforeWork(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		f := newPreparedFixture(t)
		auth := credential{}
		if canceled {
			cancel()
			auth = preparedSyntheticAuth
		}
		prepared, err := prepareWebCallWith(ctx, auth, sessionSettings{}, nil, f.deps)
		cancel()
		if err == nil || prepared != nil || f.peer != nil || f.clients.Load() != 0 {
			t.Fatal("invalid/canceled input performed preparation work")
		}
	}
}
