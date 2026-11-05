package voice

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Entered only after a fresh command and stable native IDLE. No wake detector,
// model overlay or processed-audio subscription exists while command mode idles.
func runCommandEpoch(e *stationEpoch, gate *stationGate, auth credential, settings sessionSettings) error {
	if err := gate.activate(e); err != nil {
		return err
	}
	cleanupFeedback := startEpochFeedback(e, settings)
	defer cleanupFeedback()
	feedback := e.feedback
	conn, err := dialStation(e.ctx, "127.0.0.1:9850")
	if err != nil {
		if !e.allowed() {
			return context.Cause(e.ctx)
		}
		return errors.New("cannot connect to native processed audio")
	}
	capture := newStationCapture(e)
	if err := capture.beginConnecting(); err != nil {
		_ = conn.Close()
		return err
	}
	feedback.mark(feedbackCaptureReady)
	var reader sync.WaitGroup
	reader.Go(func() {
		if err := readCapture(e.ctx, conn, capture); err != nil {
			e.revoke(fmt.Errorf("%w: %w", errStationAudio, err))
		}
	})
	defer func() { e.revoke(context.Canceled); reader.Wait() }()
	e.report.Print("Native activation command accepted; connecting voice.")
	err = runCall(e, auth, capture, settings)
	if cause := context.Cause(e.ctx); errors.Is(cause, errStationAudio) && !errors.Is(err, errStationAudio) {
		return errors.Join(cause, err)
	}
	return err
}
