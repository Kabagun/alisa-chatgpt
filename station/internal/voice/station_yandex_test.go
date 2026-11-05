package voice

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type yandexTestDetector struct {
	feeds, closed atomic.Int32
	closedAt      chan struct{}
}

func (d *yandexTestDetector) feed([]int16) (bool, error) { d.feeds.Add(1); return true, nil }
func (d *yandexTestDetector) close() error {
	d.closed.Add(1)
	if d.closedAt != nil {
		close(d.closedAt)
	}
	return nil
}

func TestYandexStableIdleBoundaryAndNativeCancellation(t *testing.T) {
	g := newStationGate()
	now := time.Now()
	g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, now)
	e := g.beginAt(t.Context(), now)
	if wait, err := g.activateStableAt(e, now.Add(commandIdleStability-1)); err != nil || wait != 1 {
		t.Fatal("KWS bypassed stable native IDLE")
	}
	g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, now.Add(commandIdleStability-1))
	if wait, err := g.activateStableAt(e, now.Add(commandIdleStability)); err != nil || wait != 0 {
		t.Fatal("stable KWS admission failed")
	}
	g.observeAt(nativeEvent{activation: true}, now.Add(time.Second))
	if _, err := g.activateStableAt(e, now.Add(2*time.Second)); !errors.Is(err, errAlicePriority) {
		t.Fatal("native activation lost priority")
	}
	_ = g.finish(e, context.Cause(e.ctx))
}

func TestYandexNativeActivityDuringAdmissionPreventsCallAndFeedback(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	g := newStationGate()
	g.observe(nativeEvent{stateChanged: true, state: aliceIdle})
	e := g.begin(ctx)
	d := &yandexTestDetector{closedAt: make(chan struct{})}
	client, server := net.Pipe()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		done <- runYandexEpoch(e, g, credential{}, sessionSettings{}, yandexEpochDeps{
			detector: func() (wakeDetector, error) { return d, nil }, dial: func(context.Context) (net.Conn, error) { return client, nil },
			feedback: func(*stationEpoch, sessionSettings) func() {
				t.Error("feedback started before IDLE admission")
				return func() {}
			},
			context: func(s sessionSettings) (sessionSettings, error) {
				t.Error("context claimed before IDLE admission")
				return s, nil
			},
			call: func(*stationEpoch, credential, *stationCapture, sessionSettings) error {
				t.Error("Voice started before IDLE admission")
				return nil
			},
		})
	}()
	if _, err := readIPC(server); err != nil {
		t.Fatal("subscription fixture")
	}
	if _, err := server.Write(framedIPC(158, audioPayload(make([]byte, 640), 16000, 1))); err != nil {
		t.Fatal("capture fixture")
	}
	select {
	case <-d.closedAt:
	case <-ctx.Done():
		t.Fatal("detector did not confirm")
	}
	g.observe(nativeEvent{activation: true})
	select {
	case err := <-done:
		if !errors.Is(err, errAlicePriority) {
			t.Fatal("native takeover was lost", err)
		}
	case <-ctx.Done():
		t.Fatal("admission did not cancel")
	}
	_ = g.finish(e, context.Cause(e.ctx))
}

func TestYandexDetectorClosesBeforeVoiceAndCannotRelaunchWhileActive(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	g := newStationGate()
	g.observeAt(nativeEvent{stateChanged: true, state: aliceIdle}, time.Now().Add(-time.Second))
	e := g.begin(ctx)
	d := &yandexTestDetector{}
	client, server := net.Pipe()
	defer server.Close()
	entered := make(chan struct{})
	done := make(chan error, 1)
	var calls atomic.Int32
	settings := sessionSettings{ProjectID: syntheticProjectID, BackendThinkingEffort: "disabled"}
	go func() {
		done <- runYandexEpoch(e, g, credential{}, settings, yandexEpochDeps{
			detector: func() (wakeDetector, error) { return d, nil },
			dial:     func(context.Context) (net.Conn, error) { return client, nil },
			feedback: func(e *stationEpoch, s sessionSettings) func() {
				f := newVoiceFeedback(e.ctx, log.New(io.Discard, "", 0), nil)
				f.health = e.output.health
				f.started = e.commandMatched
				f.markAt(feedbackTargetMatched, f.started)
				e.feedback = f
				return startEpochFeedback(e, s)
			},
			call: func(e *stationEpoch, _ credential, _ *stationCapture, got sessionSettings) error {
				calls.Add(1)
				if d.closed.Load() != 1 || d.feeds.Load() != 1 || got != settings || e.feedback == nil {
					t.Error("detector/settings/feedback not handed off")
				}
				if e.feedback.setup.elapsed[0] != 0 || e.feedback.setup.elapsed[2] != 0 {
					t.Error("KWS/capture origin is not zero")
				}
				close(entered)
				<-e.ctx.Done()
				return context.Cause(e.ctx)
			},
		})
	}()
	request := make([]byte, len(audioRequest()))
	if _, err := io.ReadFull(server, request); err != nil {
		t.Fatal("subscription fixture")
	}
	packet := framedIPC(158, audioPayload(make([]byte, 640), 16000, 1))
	if _, err := server.Write(packet); err != nil {
		t.Fatal("capture fixture")
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("KWS did not hand off")
	}
	if _, err := server.Write(packet); err != nil {
		t.Fatal("active capture fixture")
	}
	g.observe(nativeEvent{activation: true})
	select {
	case err := <-done:
		if !errors.Is(err, errAlicePriority) {
			t.Fatal("native priority was lost", err)
		}
	case <-ctx.Done():
		t.Fatal("active KWS call did not close")
	}
	if d.feeds.Load() != 1 || d.closed.Load() != 1 || calls.Load() != 1 {
		t.Fatal("keyword detector ran or relaunched during active call")
	}
	_ = g.finish(e, context.Cause(e.ctx))
}
