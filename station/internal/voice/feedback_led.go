package voice

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Experimental Lite1 adapter. It is opt-in and has no settings/RPC/trigger/red
// writes. Linux sysfs offers no atomic cross-process lease: compare-before-write
// narrows that race but cannot remove it. Device acceptance tests remain required.
var errLEDLease = errors.New("LED ownership unavailable")

// Native rendering can briefly outlive the native IDLE event. Only an entirely
// dark, unchanged trigger configuration held for this interval permits another
// lease in the same still-authorized epoch.
const ledReacquireQuiet = 500 * time.Millisecond

type ledNode struct {
	value, maximum int
	trigger        string
}

type ledDevice interface {
	read(index int) (ledNode, error)
	write(index, value int) error
}

// Index order is blue, green, red for ring1, then ring2 and ring3.
type sysfsLEDDevice struct{ root string }

func (d sysfsLEDDevice) path(index int, attribute string) string {
	color := [...]string{"blue", "green", "red"}[index%3]
	return filepath.Join(d.root, "micro:"+color+":ring"+strconv.Itoa(index/3+1), attribute)
}

func ledAttribute(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil || len(b) > 1024 {
		return "", errLEDLease
	}
	return strings.TrimSpace(string(b)), nil
}

func (d sysfsLEDDevice) read(index int) (ledNode, error) {
	var node ledNode
	for _, attr := range []string{"brightness", "max_brightness"} {
		s, err := ledAttribute(d.path(index, attr))
		if err != nil {
			return node, err
		}
		n, err := strconv.ParseUint(s, 10, 8)
		if err != nil {
			return node, errLEDLease
		}
		if attr == "brightness" {
			node.value = int(n)
		} else {
			node.maximum = int(n)
		}
	}
	triggers, err := ledAttribute(d.path(index, "trigger"))
	if err != nil {
		return node, err
	}
	for _, token := range strings.Fields(triggers) {
		if strings.HasPrefix(token, "[") && strings.HasSuffix(token, "]") {
			if node.trigger != "" {
				return node, errLEDLease
			}
			node.trigger = strings.TrimSuffix(strings.TrimPrefix(token, "["), "]")
		}
	}
	return node, nil
}

func (d sysfsLEDDevice) write(index, value int) error {
	if index < 0 || index >= 9 || index%3 == 2 || value < 0 || value > 64 {
		return errLEDLease
	}
	// Never create files, change triggers or write a red-channel attribute.
	f, err := os.OpenFile(d.path(index, "brightness"), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	data := []byte(strconv.Itoa(value) + "\n")
	n, err := f.Write(data)
	closeErr := f.Close()
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return errors.Join(err, closeErr)
}

type ledLease struct {
	device  ledDevice
	owned   [9]int
	claimed bool
	revoked bool
}

func (l *ledLease) snapshot() ([9]ledNode, error) {
	var nodes [9]ledNode
	for i := range nodes {
		n, err := l.device.read(i)
		if err != nil || n.maximum != 255 || n.trigger != "none" || n.value < 0 || n.value > 255 || i%3 == 2 && n.value != 0 {
			return nodes, errLEDLease
		}
		nodes[i] = n
	}
	return nodes, nil
}

func (l *ledLease) acquire(ctx context.Context) error {
	if ctx.Err() != nil || l.revoked {
		return errLEDLease
	}
	nodes, err := l.snapshot()
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if n.value != 0 {
			return errLEDLease
		}
	}
	l.claimed = true
	return nil
}

func (l *ledLease) check(ctx context.Context) error {
	if ctx.Err() != nil || !l.claimed || l.revoked {
		return errLEDLease
	}
	nodes, err := l.snapshot()
	if err != nil {
		return err
	}
	for i, n := range nodes {
		if n.value != l.owned[i] {
			return errLEDLease
		}
	}
	return nil
}

func (l *ledLease) frame(ctx context.Context, blue, green int) error {
	if err := l.check(ctx); err != nil {
		return err
	}
	for i := range l.owned {
		if i%3 == 2 {
			continue
		}
		value := blue
		if i%3 == 1 {
			value = green
		}
		if value == l.owned[i] {
			continue
		}
		// Recheck the complete visible frame before every individual write.
		if err := l.check(ctx); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return errLEDLease
		}
		if err := l.device.write(i, value); err != nil {
			return err
		}
		l.owned[i] = value
	}
	return nil
}

func (l *ledLease) release() {
	l.revoked = true
	if !l.claimed {
		return
	}
	l.claimed = false
	// Cleanup is the sole permitted post-cancel write. Restore saved zero only
	// where it still equals our value. Any red/trigger/read failure stops cleanup.
	for i, value := range l.owned {
		if i%3 == 2 || value == 0 {
			continue
		}
		nodes, err := l.snapshot()
		if err != nil {
			return
		}
		if nodes[i].value != value {
			continue
		}
		if l.device.write(i, 0) != nil {
			return
		}
		l.owned[i] = 0
	}
}

type stationLED struct {
	ctx     context.Context
	cancel  context.CancelFunc
	updates chan feedbackPhase
	done    chan struct{}
	ready   atomic.Bool
	yielded atomic.Bool
}

func newStationLED(ctx context.Context, device ledDevice, feedback *voiceFeedback, report *log.Logger) *stationLED {
	ctx, cancel := context.WithCancel(ctx)
	l := &stationLED{ctx: ctx, cancel: cancel, updates: make(chan feedbackPhase, 1), done: make(chan struct{})}
	go l.run(device, feedback, report)
	return l
}

func (l *stationLED) set(phase feedbackPhase) {
	if phase == feedbackStopped {
		l.cancel()
		return
	}
	if phase == feedbackMediaReady {
		// Readiness must survive latest-state channel coalescing.
		l.ready.Store(true)
	}
	if phase != feedbackNativeIdleReady && phase != feedbackCaptureReady && phase != feedbackMediaReady && (phase < feedbackServerIdle || phase > feedbackServerResponding) {
		return
	}
	select {
	case <-l.done:
		return
	default:
	}
	select {
	case l.updates <- phase:
	default:
		select {
		case <-l.updates:
		default:
		}
		select {
		case l.updates <- phase:
		default:
		}
	}
}

func (l *stationLED) run(device ledDevice, feedback *voiceFeedback, report *log.Logger) {
	defer close(l.done)
	defer l.cancel()
	lease := &ledLease{device: device}
	defer func() { lease.release() }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	playing := false
	renderStarted := false
	nativeRevoked := false
	var quietSince time.Time
	blue, green := 0, 0
	for {
		select {
		case <-l.ctx.Done():
			if nativeRedObserved(device) {
				feedback.nativeRed()
			}
			return
		case phase := <-l.updates:
			renderStarted = true
			switch phase {
			case feedbackNativeIdleReady, feedbackCaptureReady:
				blue, green = 48, 48
			case feedbackMediaReady:
				blue, green = 48, 0
			case feedbackServerIdle, feedbackServerListening:
				if l.ready.Load() {
					blue, green = 48, 0
				}
			case feedbackServerSpeaking, feedbackServerResponding:
				if l.ready.Load() {
					blue, green = 0, 48
				}
			}
		case <-ticker.C:
		}
		if nativeRedObserved(device) {
			lease.release()
			l.yielded.Store(true)
			nativeRevoked = true
			feedback.nativeRed()
			continue
		}
		if !renderStarted || nativeRevoked {
			continue
		}
		if l.yielded.Load() {
			// Never reuse the revoked lease or overwrite an external frame.
			// A transient read error, startup animation or native clear must
			// not leave an otherwise active Voice call dark forever.
			candidate := &ledLease{device: device}
			if candidate.acquire(l.ctx) != nil {
				quietSince = time.Time{}
				continue
			}
			if quietSince.IsZero() {
				quietSince = time.Now()
				continue
			}
			if time.Since(quietSince) < ledReacquireQuiet {
				continue
			}
			lease = candidate
			quietSince = time.Time{}
			l.yielded.Store(false)
			report.Print("VOICE_LED acquired")
		}
		if !lease.claimed {
			if lease.acquire(l.ctx) != nil {
				report.Print("VOICE_LED unavailable")
				lease.release()
				l.yielded.Store(true)
				continue
			}
			report.Print("VOICE_LED acquired")
		}
		last := feedback.playback.Load()
		audible := l.ready.Load() && last > 0 && time.Since(feedback.started)-time.Duration(last) < 500*time.Millisecond
		if audible != playing {
			playing = audible
			phase := feedbackPlaybackQuiet
			if audible {
				phase = feedbackPlaybackActive
			} else {
				// The mic remains live after response audio drains. A stale
				// server display state must not leave a permanent green ring.
				blue, green = 48, 0
			}
			feedback.mark(phase)
		}
		b, g := blue, green
		if audible {
			b, g = 0, 48
		}
		if lease.frame(l.ctx, b, g) != nil {
			report.Print("VOICE_LED released")
			lease.release()
			l.yielded.Store(true)
		}
	}
}

func nativeRedObserved(device ledDevice) bool {
	for i := 2; i < 9; i += 3 {
		n, err := device.read(i)
		if err == nil && n.maximum == 255 && n.value > 0 && n.value <= 255 {
			return true
		}
	}
	return false
}
