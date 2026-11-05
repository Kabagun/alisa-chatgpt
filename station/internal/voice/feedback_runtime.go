package voice

import (
	"context"
	"time"
)

// Called only after command admission or a confirmed KWS hit has passed the
// current native IDLE gate. Pending preparation and idle KWS own no LED observer.
func startEpochFeedback(e *stationEpoch, settings sessionSettings) func() {
	f := e.feedback
	if f == nil {
		f = newVoiceFeedback(e.ctx, e.report)
		f.health = e.output.health
		if !e.commandMatched.IsZero() {
			f.started = e.commandMatched
		}
		f.markAt(feedbackTargetMatched, f.started)
		e.feedback = f
	} else if e.output.health != f.health {
		e.output.health = f.health
	}
	// Playback and idle activity observation do not depend on LED opt-in.
	// Publish this immutable pointer before playback or streaming can start.
	e.output.feedback = f
	f.mu.Lock()
	f.nativeRevoke = e.revoke
	if settings.LedFeedback {
		f.led = newStationLED(e.ctx, sysfsLEDDevice{root: "/sys/class/leds"}, f, e.report)
	}
	f.mu.Unlock()
	stopMuteObserver := func() {}
	if !settings.LedFeedback {
		// Physical mute must still dominate the Voice session when ring
		// rendering is disabled. This observer never writes a LED attribute.
		stopMuteObserver = observeNativeRed(e.ctx, f, sysfsLEDDevice{root: "/sys/class/leds"})
	}
	stop := context.AfterFunc(e.ctx, func() { f.mark(feedbackStopped) })
	idleReady := e.nativeIdleReady
	if idleReady.IsZero() {
		idleReady = time.Now()
	}
	f.markAt(feedbackNativeIdleReady, idleReady)
	return func() {
		f.mark(feedbackStopped)
		stop()
		stopMuteObserver()
		if f.led != nil {
			f.led.cancel()
			<-f.led.done
		}
	}
}

func observeNativeRed(parent context.Context, feedback *voiceFeedback, device ledDevice) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			if nativeRedObserved(device) {
				feedback.nativeRed()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
