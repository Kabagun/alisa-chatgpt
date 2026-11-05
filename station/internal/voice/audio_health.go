package voice

import (
	"encoding/binary"
	"encoding/json/v2"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const audioHealthLeaf = "last-audio.json"

type audioCounter uint8

const (
	audioPCMChunks audioCounter = iota
	audioPCMSamples
	audioConnectingDropped
	audioStreamingSamples
	audioQueued
	audioEncoded
	audioSampleWrites
	audioRTPWrites
	audioRTPBytes
	audioRemoteRTP
	audioDecodedFrames
	audioPlaybackFrames
	audioPlaybackNonzero
	audioPlaybackDrops
	audioControlMessages
	audioKnownStates
	audioKnownErrors
	audioDecodeErrors
	audioPlaybackUnderruns
	audioPlaybackUnderrunFrames
	audioRemotePadding
	audioRemoteLate
	audioRemoteReordered
	audioRemoteSequenceHoles
	audioRemoteTimestampGaps
	audioRemotePLCFrames
	audioRemoteDecoderResets
	audioRemoteFECAttempts
	audioCounterCount
)

var audioCounterNames = [audioCounterCount]string{"pcm_chunks", "pcm_samples", "connecting_dropped_samples", "streaming_samples", "streaming_frames_queued", "opus_frames_encoded", "write_sample_ok", "local_rtp_writes_ok", "local_rtp_bytes", "remote_rtp_packets", "decoded_pcm_frames", "playback_pcm_frames", "playback_nonzero_frames", "playback_drops", "control_messages", "known_state_events", "known_error_events", "decode_error_events", "playback_underruns", "playback_underrun_frames", "remote_padding_packets", "remote_late_packets", "remote_reordered_packets", "remote_sequence_holes", "remote_timestamp_gaps", "remote_plc_frames", "remote_decoder_resets", "remote_fec_attempts"}

type audioActivity uint8

const (
	audioLastPCM audioActivity = iota
	audioLastStreaming
	audioLastSampleWrite
	audioLastRTPWrite
	audioLastRemoteRTP
	audioLastPlayback
	audioActivityCount
)

var audioActivityNames = [audioActivityCount]string{"last_pcm_ms", "last_stream_pcm_ms", "last_write_sample_ms", "last_local_rtp_ms", "last_remote_rtp_ms", "last_playback_ms"}

// Each accumulator has one processed-PCM producer; snapshots only read atomics.
// The weighted mean avoids square-sum overflow. Samples are never retained.
type audioEnergy struct {
	samples, nonzero, meanBits atomic.Uint64
	peak                       atomic.Uint32
}

func (e *audioEnergy) record(pcm []byte) {
	var squares float64
	var nonzero uint64
	var peak uint32
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int32(int16(binary.LittleEndian.Uint16(pcm[i:])))
		if v != 0 {
			nonzero++
		}
		if v < 0 {
			v = -v
		}
		peak = max(peak, uint32(v))
		squares += float64(v) * float64(v)
	}
	n := uint64(len(pcm) / 2)
	if n == 0 {
		return
	}
	previous := e.samples.Load()
	mean := math.Float64frombits(e.meanBits.Load())
	mean += (squares/float64(n) - mean) * float64(n) / float64(previous+n)
	e.meanBits.Store(math.Float64bits(mean))
	e.samples.Add(n)
	e.nonzero.Add(nonzero)
	for old := e.peak.Load(); peak > old && !e.peak.CompareAndSwap(old, peak); old = e.peak.Load() {
	}
}
func (e *audioEnergy) rms() uint32 {
	return uint32(math.Round(math.Sqrt(max(0, math.Float64frombits(e.meanBits.Load())))))
}

type audioHealth struct {
	serial                                         uint64 // Ordering only, never serialized.
	originOnce, publishOnce, endOnce               sync.Once
	origin                                         time.Time
	started, publishing, active, ended, peerClosed atomic.Bool
	stop                                           chan struct{}
	persist                                        func(audioHealthSnapshot)
	revision                                       atomic.Uint64
	count                                          [audioCounterCount]atomic.Uint64
	last                                           [audioActivityCount]atomic.Int64
	energy                                         [2]audioEnergy // Connecting, streaming.
	bindings                                       atomic.Int64
	serverState                                    atomic.Uint32
	playbackQueued, playbackPeak                   atomic.Int64 // FIFO frames; excludes the callback's current block.
}

var audioHealthSerial atomic.Uint64

func newAudioHealth() *audioHealth {
	h := &audioHealth{serial: audioHealthSerial.Add(1), stop: make(chan struct{})}
	for i := range h.last {
		h.last[i].Store(-1)
	}
	return h
}
func (h *audioHealth) start() {
	if h != nil {
		h.originOnce.Do(func() { h.origin = time.Now(); h.started.Store(true) })
	}
}
func (h *audioHealth) elapsed() int64 {
	if h == nil || !h.started.Load() {
		return 0
	}
	return max(0, time.Since(h.origin).Milliseconds())
}
func (h *audioHealth) add(c audioCounter, n uint64) {
	if h != nil {
		h.count[c].Add(n)
	}
}
func (h *audioHealth) activity(a audioActivity) {
	if h != nil && h.started.Load() {
		h.last[a].Store(h.elapsed())
	}
}
func (h *audioHealth) pcm(pcm []byte, connecting bool) {
	if h == nil || !h.started.Load() || len(pcm) == 0 {
		return
	}
	n := uint64(len(pcm) / 2)
	h.add(audioPCMChunks, 1)
	h.add(audioPCMSamples, n)
	h.activity(audioLastPCM)
	if connecting {
		h.add(audioConnectingDropped, n)
		h.energy[0].record(pcm)
		return
	}
	h.add(audioStreamingSamples, n)
	h.energy[1].record(pcm)
	h.activity(audioLastStreaming)
}
func (h *audioHealth) control(ev event, decodeFailed bool) {
	if h == nil {
		return
	}
	h.add(audioControlMessages, 1)
	if decodeFailed {
		h.add(audioDecodeErrors, 1)
	} else if ev.err != nil {
		h.add(audioKnownErrors, 1)
	}
	if ev.phase >= feedbackServerIdle && ev.phase <= feedbackServerResponding {
		h.add(audioKnownStates, 1)
		h.serverState.Store(uint32(ev.phase))
	}
}
func (h *audioHealth) begin(persist func(audioHealthSnapshot)) {
	if h == nil || persist == nil {
		return
	}
	h.publishOnce.Do(func() {
		h.start()
		h.persist = persist
		h.publishing.Store(true)
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			h.persist(h.snapshot(false))
			for {
				select {
				case <-h.stop:
					return
				case <-ticker.C:
					h.persist(h.snapshot(false))
				}
			}
		}()
	})
}
func (h *audioHealth) finish(peerClosed bool) {
	if h == nil || !h.publishing.Load() {
		return
	}
	h.endOnce.Do(func() {
		h.peerClosed.Store(peerClosed)
		h.ended.Store(true)
		close(h.stop)
		h.persist(h.snapshot(true))
	})
}

type audioHealthSnapshot struct {
	serial, revision                 uint64
	final, active, peerClosed, bound bool
	elapsed                          int64
	count                            [audioCounterCount]uint64
	last                             [audioActivityCount]int64
	nonzero                          [2]uint64
	peak, rms                        [2]uint32
	server                           feedbackPhase
	playbackQueued, playbackPeak     int64
}

func (h *audioHealth) snapshot(final bool) audioHealthSnapshot {
	s := audioHealthSnapshot{serial: h.serial, revision: h.revision.Add(1), final: final || h.ended.Load(), active: h.active.Load(), peerClosed: h.peerClosed.Load(), bound: h.bindings.Load() > 0, elapsed: h.elapsed(), server: feedbackPhase(h.serverState.Load())}
	s.playbackQueued, s.playbackPeak = h.playbackQueued.Load(), h.playbackPeak.Load()
	for i := range s.count {
		s.count[i] = h.count[i].Load()
	}
	for i := range s.last {
		s.last[i] = h.last[i].Load()
	}
	for i := range s.peak {
		s.nonzero[i] = h.energy[i].nonzero.Load()
		s.peak[i] = h.energy[i].peak.Load()
		s.rms[i] = h.energy[i].rms()
	}
	return s
}
func (s audioHealthSnapshot) json() []byte {
	state := "connecting"
	if s.active {
		state = "active"
	}
	if s.final {
		state = "ended"
		if !s.peerClosed {
			state = "unknown"
		}
	}
	server := "unknown"
	if s.server >= feedbackServerIdle && s.server <= feedbackServerResponding {
		server = s.server.name()
	}
	wire := map[string]any{"state": state, "elapsed_ms": s.elapsed, "peer_closed": s.peerClosed, "mic_bound": s.bound, "server_state": server,
		"playback_queue_frames": s.playbackQueued, "playback_queue_peak_frames": s.playbackPeak,
		"connecting_nonzero_samples": s.nonzero[0], "connecting_peak_abs": s.peak[0], "connecting_rms": s.rms[0],
		"streaming_nonzero_samples": s.nonzero[1], "streaming_peak_abs": s.peak[1], "streaming_rms": s.rms[1]}
	for i, name := range audioCounterNames {
		wire[name] = s.count[i]
	}
	for i, name := range audioActivityNames {
		wire[name] = s.last[i]
	}
	b, _ := json.Marshal(wire)
	if len(b) > 2047 {
		return nil
	}
	return append(b, '\n')
}

// One I/O worker and one pending value. Private ordering tickets reject older
// calls and delayed periodic callbacks after a final snapshot.
type audioHealthSink struct {
	mu     sync.Mutex
	once   sync.Once
	wake   chan struct{}
	done   chan struct{}
	latest audioHealthSnapshot
	write  func(audioHealthSnapshot) bool
}

func newAudioHealthSink(write func(audioHealthSnapshot) bool) *audioHealthSink {
	return &audioHealthSink{wake: make(chan struct{}, 1), done: make(chan struct{}), write: write}
}
func (s *audioHealthSink) offer(value audioHealthSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value.serial == 0 || value.serial < s.latest.serial || value.serial == s.latest.serial && (s.latest.final || value.revision <= s.latest.revision) {
		return
	}
	s.latest = value
	s.once.Do(func() {
		go func() {
			defer close(s.done)
			for range s.wake {
				s.mu.Lock()
				v := s.latest
				s.mu.Unlock()
				_ = s.write(v)
			}
		}()
	})
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

var lastAudioHealthSink = newAudioHealthSink(func(s audioHealthSnapshot) bool {
	return writePrivateSnapshot(setupTimingDirectory, audioHealthLeaf, s.json())
})
