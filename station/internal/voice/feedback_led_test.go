package voice

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

type ledWrite struct{ index, value int }

type memoryLED struct {
	mu         sync.Mutex
	nodes      [9]ledNode
	writes     []ledWrite
	readError  bool
	writeError bool
	afterWrite func(*memoryLED)
}

func newMemoryLED() *memoryLED {
	d := &memoryLED{}
	for i := range d.nodes {
		d.nodes[i] = ledNode{maximum: 255, trigger: "none"}
	}
	return d
}

func (d *memoryLED) read(index int) (ledNode, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.readError {
		return ledNode{}, errLEDLease
	}
	return d.nodes[index], nil
}

func (d *memoryLED) write(index, value int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.writeError || index%3 == 2 {
		return errLEDLease
	}
	d.nodes[index].value = value
	d.writes = append(d.writes, ledWrite{index, value})
	if d.afterWrite != nil {
		d.afterWrite(d)
	}
	return nil
}

func (d *memoryLED) frame() [9]ledNode {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.nodes
}

func TestLEDLeaseScopeDedupAndCanceledCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := newMemoryLED()
	l := &ledLease{device: d}
	if l.acquire(ctx) != nil || l.frame(ctx, 48, 48) != nil || len(d.writes) != 6 {
		t.Fatal("off/none frame was not acquired within the six B/G channels")
	}
	if l.frame(ctx, 48, 48) != nil || len(d.writes) != 6 {
		t.Fatal("identical frame was written again")
	}
	if l.frame(ctx, 48, 0) != nil || len(d.writes) != 9 {
		t.Fatal("deduplication missed unchanged blue channels")
	}
	cancel()
	if l.frame(ctx, 0, 48) == nil || len(d.writes) != 9 {
		t.Fatal("canceled epoch emitted a new color frame")
	}
	l.release()
	for _, n := range d.frame() {
		if n.value != 0 || n.trigger != "none" {
			t.Fatal("conditional cleanup changed native state or left owned color")
		}
	}
	for _, w := range d.writes {
		if w.index%3 == 2 || w.value != 0 && w.value != 48 {
			t.Fatal("red or an unexpected value was written")
		}
	}
	if l.acquire(t.Context()) == nil || l.frame(t.Context(), 48, 0) == nil {
		t.Fatal("revoked lease was reacquired")
	}
}

func TestLEDLeaseFailsClosed(t *testing.T) {
	for _, name := range []string{"colored", "red", "trigger", "maximum", "read", "write"} {
		t.Run(name, func(t *testing.T) {
			d := newMemoryLED()
			switch name {
			case "colored":
				d.nodes[0].value = 1
			case "red":
				d.nodes[2].value = 255
			case "trigger":
				d.nodes[0].trigger = "led_ring_fb"
			case "maximum":
				d.nodes[0].maximum = 100
			case "read":
				d.readError = true
			case "write":
				d.writeError = true
			}
			before := d.frame()
			l := &ledLease{device: d}
			err := l.acquire(t.Context())
			if name == "write" && err == nil {
				err = l.frame(t.Context(), 48, 48)
			}
			l.release()
			if err == nil || len(d.writes) != 0 || d.frame() != before {
				t.Fatal("unsafe/unknown LED state produced a write")
			}
		})
	}
}

func TestLEDLeasePreservesExternalValuesAndRechecksEachWrite(t *testing.T) {
	d := newMemoryLED()
	l := &ledLease{device: d}
	if l.acquire(t.Context()) != nil || l.frame(t.Context(), 48, 48) != nil {
		t.Fatal("cannot acquire fixture")
	}
	d.nodes[0].value = 17
	before := len(d.writes)
	if l.frame(t.Context(), 48, 0) == nil || len(d.writes) != before {
		t.Fatal("external frame was fought")
	}
	l.release()
	if d.nodes[0].value != 17 {
		t.Fatal("cleanup overwrote an external value")
	}
	d = newMemoryLED()
	l = &ledLease{device: d}
	d.afterWrite = func(d *memoryLED) { d.nodes[2].value = 255 }
	if l.acquire(t.Context()) != nil || l.frame(t.Context(), 48, 48) == nil || len(d.writes) != 1 {
		t.Fatal("native red change between individual writes was missed")
	}
	l.release()
	if len(d.writes) != 1 || d.nodes[2].value != 255 {
		t.Fatal("red-active cleanup wrote another frame")
	}
}

func waitLED(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("LED worker did not reach expected state")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLEDWorkerReadyPlaybackFallbackAndNativeRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := newMemoryLED()
	report := log.New(io.Discard, "", 0)
	f := newVoiceFeedback(ctx, report, nil)
	f.nativeRevoke = func(error) { cancel() }
	l := newStationLED(ctx, d, f, report)
	f.led = l
	defer func() { l.cancel(); <-l.done }()
	isColor := func(blue, green int) bool {
		n := d.frame()
		for i := 0; i < 9; i += 3 {
			if n[i].value != blue || n[i+1].value != green || n[i+2].value != 0 {
				return false
			}
		}
		return true
	}
	f.mark(feedbackCaptureReady)
	waitLED(t, func() bool { return isColor(48, 48) })
	f.mark(feedbackServerListening)
	if !isColor(48, 48) {
		t.Fatal("server display state announced readiness before actual media")
	}
	f.mark(feedbackMediaReady)
	waitLED(t, func() bool { return isColor(48, 0) })
	f.mark(feedbackServerSpeaking)
	waitLED(t, func() bool { return isColor(0, 48) })
	f.playback.Store(time.Since(f.started).Nanoseconds())
	waitLED(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.phase == feedbackPlaybackActive
	})
	f.playback.Store(0)
	waitLED(t, func() bool { return isColor(48, 0) })
	d.mu.Lock()
	for i := range d.nodes {
		d.nodes[i].value = 0
		if i%3 == 2 {
			d.nodes[i].value = 255
		}
	}
	d.mu.Unlock()
	select {
	case <-l.done:
	case <-time.After(2 * time.Second):
		t.Fatal("native frame did not permanently revoke lease")
	}
	d.mu.Lock()
	count := len(d.writes)
	d.mu.Unlock()
	f.mark(feedbackServerSpeaking)
	f.mark(feedbackMediaReady)
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.writes) != count {
		t.Fatal("released worker fought native LEDs")
	}
}

func TestLEDWorkerCancellationAndReadFailureAreIsolated(t *testing.T) {
	for _, failRead := range []bool{false, true} {
		t.Run(strconv.FormatBool(failRead), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			d := newMemoryLED()
			report := log.New(io.Discard, "", 0)
			f := newVoiceFeedback(ctx, report, nil)
			l := newStationLED(ctx, d, f, report)
			f.led = l
			defer func() { l.cancel(); <-l.done }()
			f.mark(feedbackMediaReady)
			waitLED(t, func() bool { return d.frame()[0].value == 48 })
			if failRead {
				d.mu.Lock()
				d.readError = true
				d.mu.Unlock()
				waitLED(t, func() bool { return l.yielded.Load() })
				if ctx.Err() != nil {
					t.Fatal("optional LED read failure canceled voice")
				}
				cancel()
			} else {
				cancel()
			}
			select {
			case <-l.done:
			case <-time.After(2 * time.Second):
				t.Fatal("LED worker did not release")
			}
			if !failRead {
				for _, n := range d.frame() {
					if n.value != 0 {
						t.Fatal("normal cancellation did not restore still-owned color")
					}
				}
			}
		})
	}
}

func TestLEDWorkerRecoversOnlyAfterStableAllOff(t *testing.T) {
	for _, failure := range []string{"startup_color", "external_clear", "read_failure"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			d := newMemoryLED()
			if failure == "startup_color" {
				d.nodes[0].value = 17
			}
			report := log.New(io.Discard, "", 0)
			f := newVoiceFeedback(ctx, report, nil)
			l := newStationLED(ctx, d, f, report)
			f.led = l
			defer func() { cancel(); <-l.done }()
			f.mark(feedbackMediaReady)
			if failure != "startup_color" {
				waitLED(t, func() bool { return d.frame()[0].value == 48 })
				d.mu.Lock()
				if failure == "read_failure" {
					d.readError = true
				} else {
					for i := range d.nodes {
						d.nodes[i].value = 0
					}
				}
				d.mu.Unlock()
			}
			waitLED(t, func() bool { return l.yielded.Load() })
			d.mu.Lock()
			count := len(d.writes)
			d.mu.Unlock()
			// The external frame/read failure persists: the worker must yield.
			time.Sleep(100 * time.Millisecond)
			d.mu.Lock()
			if len(d.writes) != count {
				d.mu.Unlock()
				t.Fatal("yielding worker fought the external state")
			}
			d.readError = false
			for i := range d.nodes {
				d.nodes[i].value = 0
			}
			d.mu.Unlock()
			// An all-off sample is insufficient; a native animation may have gaps.
			time.Sleep(ledReacquireQuiet / 2)
			if !l.yielded.Load() || d.frame()[0].value != 0 {
				t.Fatal("worker reacquired before a stable all-off interval")
			}
			waitLED(t, func() bool { return !l.yielded.Load() && d.frame()[0].value == 48 })
			cancel()
			<-l.done
			for _, n := range d.frame() {
				if n.value != 0 {
					t.Fatal("cancellation did not release the replacement lease")
				}
			}
		})
	}
}

func TestLEDWorkerNativeRedNeverReacquiresAfterUnmute(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := newMemoryLED()
	report := log.New(io.Discard, "", 0)
	f := newVoiceFeedback(ctx, report, nil)
	l := newStationLED(ctx, d, f, report)
	f.led = l
	defer func() { cancel(); <-l.done }()
	f.mark(feedbackMediaReady)
	waitLED(t, func() bool { return d.frame()[0].value == 48 })
	d.mu.Lock()
	d.nodes[2].value = 255
	d.mu.Unlock()
	waitLED(t, func() bool { return l.yielded.Load() })
	d.mu.Lock()
	for i := range d.nodes {
		d.nodes[i].value = 0
	}
	count := len(d.writes)
	d.mu.Unlock()
	time.Sleep(ledReacquireQuiet + 150*time.Millisecond)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !l.yielded.Load() || len(d.writes) != count {
		t.Fatal("unmute reacquired LEDs within a revoked native-red epoch")
	}
}

func TestLEDNativeRedEndsEpochAfterLeaseYield(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	d := newMemoryLED()
	report := log.New(io.Discard, "", 0)
	f := newVoiceFeedback(e.ctx, report, nil)
	f.nativeRevoke = e.revoke
	proof := make(chan callEndReason, 2)
	f.persistEnd = func(reason callEndReason) { proof <- reason }
	l := newStationLED(e.ctx, d, f, report)
	f.led = l
	defer func() { e.revoke(context.Canceled); l.cancel(); <-l.done }()
	f.mark(feedbackMediaReady)
	waitLED(t, func() bool { return d.frame()[0].value == 48 })
	d.mu.Lock()
	d.nodes[0].value = 17 // Ordinary external B/G takeover releases colors only.
	d.mu.Unlock()
	waitLED(t, func() bool { return l.yielded.Load() })
	if !e.allowed() {
		t.Fatal("ordinary LED ownership loss ended Voice")
	}
	d.mu.Lock()
	d.nodes[2].value = 255
	d.mu.Unlock()
	select {
	case <-e.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("red after LED yield did not cancel the entire epoch")
	}
	if !errors.Is(context.Cause(e.ctx), errAlicePriority) {
		t.Fatal("red did not use native-priority teardown")
	}
	waitLED(t, func() bool { return len(proof) == 1 })
	if <-proof != callEndedNativeRed {
		t.Fatal("native-red proof is not the fixed enum")
	}
	<-l.done
	d.mu.Lock()
	count := len(d.writes)
	for i := range d.nodes {
		d.nodes[i].value = 0
	}
	d.mu.Unlock()
	f.mark(feedbackMediaReady)
	f.nativeRed()
	if e.allowed() || len(proof) != 0 {
		t.Fatal("unmute resumed Voice or duplicated proof")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.writes) != count {
		t.Fatal("unmute restarted LED rendering")
	}
}

func TestPlaybackFeedbackIsOnlyPostGateOutputActivity(t *testing.T) {
	e := newStationEpoch(t.Context(), 1)
	f := newVoiceFeedback(e.ctx, log.New(io.Discard, "", 0), nil)
	e.output.feedback = f
	pcm := make([]int16, samplesPerFrame*2)
	for i := range pcm {
		pcm[i] = 1000
	}
	output := make([]byte, samplesPerFrame*4)
	for range 3 {
		e.output.queueDecoded(pcm)
	}
	e.output.callback(output, nil, samplesPerFrame)
	before := f.playback.Load()
	if before == 0 || allZero(output) {
		t.Fatal("actual remote playback did not signal activity")
	}
	e.revoke(errAlicePriority)
	e.output.queueDecoded(pcm)
	e.output.callback(output, nil, samplesPerFrame)
	if f.playback.Load() != before || !allZero(output) {
		t.Fatal("revoked output generated activity or escaped mute gate")
	}
}

func TestLEDSysfsAdapterBoundsAndNeverWritesRed(t *testing.T) {
	d := sysfsLEDDevice{root: t.TempDir()}
	for i := 0; i < 9; i++ {
		dir := filepath.Dir(d.path(i, "brightness"))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]string{"brightness": "0\n", "max_brightness": "255\n", "trigger": "[none] led_ring_fb\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	n, err := d.read(0)
	if err != nil || n.trigger != "none" || n.maximum != 255 || n.value != 0 {
		t.Fatal("valid sysfs metadata was not decoded")
	}
	for _, w := range []ledWrite{{2, 48}, {0, 65}, {-1, 0}, {9, 0}} {
		if d.write(w.index, w.value) == nil {
			t.Fatal("out-of-scope sysfs write was accepted")
		}
	}
	if d.write(0, 48) != nil {
		t.Fatal("bounded B/G value was not accepted")
	}
	if err := os.Remove(d.path(0, "brightness")); err != nil {
		t.Fatal(err)
	}
	if err := d.write(0, 0); err == nil {
		t.Fatal("adapter created a missing brightness attribute")
	}
	if _, err := os.Stat(d.path(0, "brightness")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing attribute was created")
	}
}
