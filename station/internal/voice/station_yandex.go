package voice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

type wakeDetector interface {
	feed([]int16) (bool, error)
	close() error
}
type yandexEpochDeps struct {
	detector func() (wakeDetector, error)
	dial     func(context.Context) (net.Conn, error)
	call     func(*stationEpoch, credential, *stationCapture, sessionSettings) error
	feedback func(*stationEpoch, sessionSettings) func()
	context  func(sessionSettings) (sessionSettings, error)
}

func (g *stationGate) activateStableAt(e *stationEpoch, now time.Time) (time.Duration, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cause := context.Cause(e.ctx); cause != nil {
		return 0, cause
	}
	if g.state != aliceIdle || g.current != e || g.number != e.number || g.idleSince.IsZero() {
		return 0, errAlicePriority
	}
	remaining := commandIdleStability - now.Sub(g.idleSince)
	if remaining > 0 {
		return remaining, nil
	}
	e.nativeIdleReady = now
	return 0, nil
}

func awaitNativeIdle(e *stationEpoch, g *stationGate) error {
	for {
		wait, err := g.activateStableAt(e, time.Now())
		if err != nil || wait <= 0 {
			return err
		}
		timer := time.NewTimer(wait)
		select {
		case <-e.ctx.Done():
			timer.Stop()
			return context.Cause(e.ctx)
		case <-timer.C:
		}
	}
}

func runYandexEpoch(e *stationEpoch, gate *stationGate, auth credential, settings sessionSettings, deps yandexEpochDeps) (result error) {
	detector, err := deps.detector()
	if err != nil {
		return err
	}
	defer func() {
		if detector != nil {
			result = errors.Join(result, detector.close())
		}
	}()
	if !e.allowed() {
		return context.Cause(e.ctx)
	}
	conn, err := deps.dial(e.ctx)
	if err != nil {
		if !e.allowed() {
			return context.Cause(e.ctx)
		}
		return errors.New("cannot connect to native processed audio")
	}
	capture := newStationCapture(e)
	var reader sync.WaitGroup
	reader.Go(func() {
		if err := readCapture(e.ctx, conn, capture); err != nil {
			e.revoke(fmt.Errorf("%w: %w", errStationAudio, err))
		}
	})
	defer func() { e.revoke(context.Canceled); reader.Wait() }()
	e.report.Print("Yandex wake detection armed.")
	var captureReady time.Time
	for {
		select {
		case <-e.ctx.Done():
			return context.Cause(e.ctx)
		case frame := <-capture.detector:
			if !e.allowed() {
				return context.Cause(e.ctx)
			}
			if captureReady.IsZero() {
				captureReady = time.Now()
			}
			confirmed, err := detector.feed(frame.pcm[:])
			if err != nil {
				return err
			}
			if !confirmed {
				continue
			}
			e.commandMatched = time.Now()
			// Stop detector processing before any wait or active Voice turn.
			if err := capture.beginConnecting(); err != nil {
				return err
			}
			err = detector.close()
			detector = nil
			if err != nil {
				return err
			}
			if err := awaitNativeIdle(e, gate); err != nil {
				return err
			}
			loadContext := deps.context
			if loadContext == nil {
				loadContext = func(s sessionSettings) (sessionSettings, error) {
					return takeVoiceContextProbe(setupTimingDirectory, s, time.Now())
				}
			}
			settings, err = loadContext(settings)
			if err != nil {
				return voiceCallError{errContextProbeRejected}
			}
			startFeedback := deps.feedback
			if startFeedback == nil {
				startFeedback = startEpochFeedback
			}
			cleanupFeedback := startFeedback(e, settings)
			defer cleanupFeedback()
			// The local subscriber/model already fed valid PCM before the hit.
			// Relative to KWS confirmation this retrospective readiness is zero.
			e.feedback.markAt(feedbackCaptureReady, captureReady)
			e.report.Print("Yandex wake confirmed; connecting voice.")
			err = deps.call(e, auth, capture, settings)
			if cause := context.Cause(e.ctx); errors.Is(cause, errStationAudio) && !errors.Is(err, errStationAudio) {
				return errors.Join(cause, err)
			}
			return err
		}
	}
}
