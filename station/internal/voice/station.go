package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

const detectorQueueFrames = 40
const captureQueueFrames = 600
const setupTimeout = 8 * time.Second

var errAlicePriority = errors.New("native Alice has priority")
var errCleanup = errors.New("resource cleanup failed")
var errStationAudio = errors.New("Station audio")
var errCaptureOverflow = fmt.Errorf("%w: processed capture queue overflow", errStationAudio)

type voiceCallError struct{ error }

func (e voiceCallError) Unwrap() error { return e.error }

type stationEpoch struct {
	number          uint64
	ctx             context.Context
	cancel          context.CancelCauseFunc
	output          *audio
	report          *log.Logger
	candidate       *commandCandidate
	feedback        *voiceFeedback
	nativeIdleReady time.Time
	commandMatched  time.Time
}

func newStationEpoch(parent context.Context, number uint64) *stationEpoch {
	ctx, cancel := context.WithCancelCause(parent)
	e := &stationEpoch{number: number, ctx: ctx, cancel: cancel, report: log.New(io.Discard, "", 0)}
	e.output = newAudio(e)
	return e
}

func (e *stationEpoch) revoke(cause error) {
	e.cancel(cause)
}

func (e *stationEpoch) allowed() bool {
	return e.ctx.Err() == nil
}

type stationGate struct {
	mu          sync.Mutex
	state       aliceState
	number      uint64
	current     *stationEpoch
	changed     chan struct{}
	command     *commandTrigger
	preparation *commandPreparer
	idleSince   time.Time
}

func newStationGate() *stationGate { return &stationGate{changed: make(chan struct{}, 1)} }

func (g *stationGate) observe(ev nativeEvent) {
	g.observeAt(ev, time.Now())
}

func (g *stationGate) observeAt(ev nativeEvent, now time.Time) {
	if !ev.activation && !ev.stateChanged && !ev.commandMatch {
		return
	}
	g.mu.Lock()
	previous := g.state
	if ev.activation {
		g.state = aliceUnknown
	} else {
		g.state = ev.state
	}
	if g.state == aliceIdle {
		if previous != aliceIdle || g.idleSince.IsZero() {
			g.idleSince = now
		}
	} else {
		g.idleSince = time.Time{}
	}
	if g.state != aliceIdle {
		g.number++
		if g.current != nil {
			g.current.revoke(errAlicePriority)
		}
	}
	if g.command != nil {
		g.command.observe(ev, now)
	}
	if g.preparation != nil {
		g.preparation.reconcileLocked(now)
	}
	g.mu.Unlock()
	select {
	case g.changed <- struct{}{}:
	default:
	}
}

func (g *stationGate) begin(ctx context.Context) *stationEpoch {
	return g.beginAt(ctx, time.Now())
}

func (g *stationGate) beginAt(ctx context.Context, now time.Time) *stationEpoch {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.preparation != nil {
		g.preparation.reconcileLocked(now)
	}
	if g.state != aliceIdle || g.current != nil || ctx.Err() != nil {
		return nil
	}
	if g.preparation != nil && !g.preparation.admissibleLocked() {
		return nil
	}
	if g.command != nil && !g.command.take(now) {
		return nil
	}
	g.number++
	g.current = newStationEpoch(ctx, g.number)
	g.current.nativeIdleReady = now
	if g.command != nil {
		g.current.commandMatched = g.command.matchedAt
	}
	if g.preparation != nil {
		g.preparation.promoteLocked(g.current)
	}
	return g.current
}

func (g *stationGate) activate(e *stationEpoch) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cause := context.Cause(e.ctx); cause != nil {
		return cause
	}
	if g.state != aliceIdle || g.current != e || g.number != e.number {
		return errAlicePriority
	}
	return nil
}

func (g *stationGate) completion(parent context.Context, old *stationEpoch) *stationEpoch {
	g.mu.Lock()
	defer g.mu.Unlock()
	if parent.Err() != nil || g.current != old || g.number != old.number || g.state != aliceIdle {
		return nil
	}
	old.revoke(context.Canceled)
	g.number++
	g.current = newStationEpoch(parent, g.number)
	return g.current
}

func (g *stationGate) finish(e *stationEpoch, result error) error {
	if cause := context.Cause(e.ctx); errors.Is(cause, errAlicePriority) {
		result = errors.Join(result, cause)
	}
	e.revoke(context.Canceled)
	if e.candidate != nil {
		result = errors.Join(result, e.candidate.close(context.Cause(e.ctx)))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.current == e {
		g.current = nil
		g.number++
	}
	return result
}

type capturePhase uint8

const (
	captureDetecting capturePhase = iota
	captureConnecting
	captureStreaming
)

type stationCapture struct {
	mu       sync.Mutex
	epoch    *stationEpoch
	phase    capturePhase
	partial  captureFrame
	used     int
	detector chan captureFrame
	input    chan captureFrame
}

func newStationCapture(e *stationEpoch) *stationCapture {
	return &stationCapture{
		epoch:    e,
		partial:  captureFrame{epoch: e.number},
		detector: make(chan captureFrame, detectorQueueFrames),
		input:    make(chan captureFrame, captureQueueFrames),
	}
}

func (c *stationCapture) beginConnecting() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.epoch.allowed() {
		return context.Cause(c.epoch.ctx)
	}
	if c.phase != captureDetecting {
		return errors.New("processed capture is not detecting")
	}
	c.phase = captureConnecting
	c.used = 0
	c.epoch.output.health.start()
	return nil
}

func (c *stationCapture) beginStreaming(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if !c.epoch.allowed() {
		return context.Cause(c.epoch.ctx)
	}
	if c.phase != captureConnecting {
		return errors.New("processed capture is not connecting")
	}
	c.phase = captureStreaming
	c.used = 0
	return nil
}

func (c *stationCapture) pushPCM(pcm []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.epoch.allowed() {
		return context.Cause(c.epoch.ctx)
	}
	if c.phase != captureDetecting {
		c.epoch.output.health.pcm(pcm, c.phase == captureConnecting)
	}
	if c.phase == captureConnecting {
		return nil
	}
	output := c.detector
	if c.phase == captureStreaming {
		output = c.input
	}
	for len(pcm) > 0 {
		c.partial.pcm[c.used] = int16(binary.LittleEndian.Uint16(pcm))
		pcm = pcm[2:]
		c.used++
		if c.used != captureSamples {
			continue
		}
		if !c.epoch.allowed() {
			return context.Cause(c.epoch.ctx)
		}
		select {
		case output <- c.partial:
			if c.phase == captureStreaming {
				c.epoch.output.health.add(audioQueued, 1)
			}
			c.partial.sequence++
			c.used = 0
		default:
			c.epoch.revoke(errCaptureOverflow)
			return errCaptureOverflow
		}
	}
	return nil
}

func runStation(parent context.Context, credentialFile string, settings sessionSettings, out io.Writer) error {
	auth, err := loadCredential(credentialFile)
	if err != nil {
		return err
	}
	conn, err := dialStation(parent, "127.0.0.1:9875")
	if err != nil {
		if parent.Err() != nil {
			return parent.Err()
		}
		return errors.New("cannot connect to native Alice state")
	}
	return serveStationConfigured(parent, conn, auth, out, func(e *stationEpoch, gate *stationGate, auth credential) error {
		return runStationEpoch(e, gate, auth, settings)
	}, playEndCue, settings, prepareWebCall)
}

func serveStation(parent context.Context, conn net.Conn, auth credential, out io.Writer, attempt func(*stationEpoch, *stationGate, credential) error, endCue func(context.Context) error, activationCommand ...string) error {
	var settings sessionSettings
	if len(activationCommand) > 0 {
		settings.ActivationCommand = activationCommand[0]
	}
	return serveStationConfigured(parent, conn, auth, out, attempt, endCue, settings, nil)
}

func serveStationConfigured(parent context.Context, conn net.Conn, auth credential, out io.Writer, attempt func(*stationEpoch, *stationGate, credential) error, endCue func(context.Context) error, settings sessionSettings, prepare prepareCallFunc) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	gate := newStationGate()
	var commandPoll <-chan time.Time
	if settings.ActivationCommand != "" {
		gate.command = &commandTrigger{phrase: settings.ActivationCommand}
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		commandPoll = ticker.C
	}
	report := log.New(out, "", 0)
	if gate.command != nil && prepare != nil {
		gate.preparation = &commandPreparer{gate: gate, ctx: ctx, auth: auth, settings: settings, report: report, prepare: prepare}
		defer gate.preparation.close()
	}
	defer report.Print("Station voice stopped.")
	stateDone := make(chan error, 1)
	var stateWorker sync.WaitGroup
	stateWorker.Go(func() { stateDone <- readAlice(ctx, conn, gate) })
	defer func() { cancel(); stateWorker.Wait() }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-stateDone:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("native Alice state transport ended: %w", err)
		default:
		}
		if err := gate.preparationFailure(); err != nil {
			if errors.Is(err, errCleanup) {
				return err
			}
			report.Print("Voice preparation failed.")
		}
		if epoch := gate.begin(ctx); epoch != nil {
			epoch.report = report
			err := attempt(epoch, gate, auth)
			var cue *stationEpoch
			if err == nil {
				cue = gate.completion(ctx, epoch)
			}
			err = gate.finish(epoch, err)
			if cue != nil {
				cueErr := gate.finish(cue, endCue(cue.ctx))
				if cueErr != nil && !errors.Is(cueErr, errAlicePriority) && ctx.Err() == nil {
					report.Print("Completion cue failed: ", cueErr)
				}
			}
			if dropped := epoch.output.playbackDrops.Load(); dropped > 0 {
				report.Printf("Playback queue overflow: %d audio frames dropped.", dropped)
			}
			if errors.Is(err, errStationAudio) || errors.Is(err, errCleanup) {
				return err
			}
			_, callFailed := errors.AsType[voiceCallError](err)
			if callFailed {
				report.Print("Voice attempt failed: ", err)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil && !callFailed && !errors.Is(err, errAlicePriority) && !errors.Is(err, context.Canceled) {
				return err
			}
			if errors.Is(err, errAlicePriority) {
				report.Print("Voice yielded to native Alice.")
			} else if err == nil {
				report.Print("Voice call ended normally.")
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-stateDone:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("native Alice state transport ended: %w", err)
		case <-gate.changed:
		case <-commandPoll:
		}
	}
}

func runStationEpoch(e *stationEpoch, gate *stationGate, auth credential, settings sessionSettings) error {
	if settings.ActivationCommand != "" {
		return runCommandEpoch(e, gate, auth, settings)
	}
	return runYandexEpoch(e, gate, auth, settings, yandexEpochDeps{
		detector: func() (wakeDetector, error) { return newNativeSpotter() },
		dial:     func(ctx context.Context) (net.Conn, error) { return dialStation(ctx, "127.0.0.1:9850") },
		call:     runCall,
	})
}
