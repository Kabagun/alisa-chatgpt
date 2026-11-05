package voice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type observerTestWriter struct {
	mu      sync.Mutex
	n       int
	err     error
	header  *rtp.Header
	payload []byte
}

type observerTestReader struct{}

func (*observerTestReader) Read(_ []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
	return 0, a, nil
}

func (w *observerTestWriter) WriteRTP(h *rtp.Header, b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.header = h
	w.payload = b
	return w.n, w.err
}
func (w *observerTestWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.payload = b
	return w.n, w.err
}

type observerTestContext struct {
	id         string
	writer     webrtc.TrackLocalWriter
	codecs     []webrtc.RTPCodecParameters
	extensions []webrtc.RTPHeaderExtensionParameter
	reader     interceptor.RTCPReader
}

func (c observerTestContext) ID() string                                   { return c.id }
func (c observerTestContext) WriteStream() webrtc.TrackLocalWriter         { return c.writer }
func (c observerTestContext) CodecParameters() []webrtc.RTPCodecParameters { return c.codecs }
func (c observerTestContext) HeaderExtensions() []webrtc.RTPHeaderExtensionParameter {
	return c.extensions
}
func (c observerTestContext) SSRC() webrtc.SSRC                       { return 123 }
func (c observerTestContext) SSRCRetransmission() webrtc.SSRC         { return 124 }
func (c observerTestContext) SSRCForwardErrorCorrection() webrtc.SSRC { return 125 }
func (c observerTestContext) RTCPReader() interceptor.RTCPReader      { return c.reader }
func observerContext(id string, w webrtc.TrackLocalWriter) observerTestContext {
	return observerTestContext{id: id, writer: w, codecs: []webrtc.RTPCodecParameters{{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, PayloadType: 111}}}
}

func TestFeedbackMicObserverPreservesContextAndWriterResults(t *testing.T) {
	h := newAudioHealth()
	h.start()
	sentinel := errors.New("synthetic-error")
	w := &observerTestWriter{n: 17, err: sentinel}
	ctx := observerContext("synthetic-context", w)
	ctx.extensions = []webrtc.RTPHeaderExtensionParameter{{URI: "synthetic-extension", ID: 7}}
	ctx.reader = &observerTestReader{}
	c := observedMicContext{TrackLocalContext: ctx, health: h}
	if c.ID() != ctx.ID() || c.SSRC() != ctx.SSRC() || c.SSRCRetransmission() != ctx.SSRCRetransmission() || c.SSRCForwardErrorCorrection() != ctx.SSRCForwardErrorCorrection() || !reflect.DeepEqual(c.CodecParameters(), ctx.CodecParameters()) || !reflect.DeepEqual(c.HeaderExtensions(), ctx.HeaderExtensions()) || c.RTCPReader() != ctx.RTCPReader() {
		t.Fatal("negotiated context changed")
	}
	writer := c.WriteStream()
	header := &rtp.Header{Version: 2, SSRC: 123}
	payload := []byte{1, 2, 3}
	if n, err := writer.WriteRTP(header, payload); n != 17 || err != sentinel || w.header != header || &w.payload[0] != &payload[0] {
		t.Fatal("RTP delegation changed pointer/return semantics")
	}
	if h.count[audioRTPWrites].Load() != 0 {
		t.Fatal("failed local write counted as success")
	}
	w.err = nil
	w.n = 0
	_, _ = writer.Write(payload)
	if h.count[audioRTPWrites].Load() != 0 {
		t.Fatal("empty local write counted as sent")
	}
	w.n = 17
	n, err := writer.Write(payload)
	if n != 17 || err != nil || !bytes.Equal(w.payload, payload) || h.count[audioRTPWrites].Load() != 1 || h.count[audioRTPBytes].Load() != 17 {
		t.Fatal("raw RTP delegation/counter changed")
	}
}

func TestFeedbackMicObserverBindUnbindAndConcurrentLifecycle(t *testing.T) {
	raw, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "synthetic-track", "synthetic-stream")
	if err != nil {
		t.Fatal(err)
	}
	h := newAudioHealth()
	track := &observedMicTrack{TrackLocal: raw, health: h}
	writer := &observerTestWriter{n: 12}
	if track.ID() != raw.ID() || track.RID() != raw.RID() || track.StreamID() != raw.StreamID() || track.Kind() != raw.Kind() {
		t.Fatal("track identity changed")
	}
	bad := observerContext("synthetic-bad", writer)
	bad.codecs = nil
	if _, err := track.Bind(bad); err == nil || h.bindings.Load() != 0 {
		t.Fatal("failed bind marked microphone bound")
	}
	good := observerContext("synthetic-good", writer)
	codec, err := track.Bind(good)
	if err != nil || codec.PayloadType != 111 || h.bindings.Load() != 1 {
		t.Fatal("bind delegation failed")
	}
	if err := track.Unbind(observerContext("synthetic-missing", writer)); err == nil || h.bindings.Load() != 1 {
		t.Fatal("failed unbind changed binding proof")
	}
	if track.Unbind(good) != nil || h.bindings.Load() != 0 {
		t.Fatal("successful unbind not delegated")
	}
	var group sync.WaitGroup
	for i := range 40 {
		group.Go(func() {
			ctx := observerContext(fmt.Sprintf("synthetic-%d", i), writer)
			if _, err := track.Bind(ctx); err != nil {
				t.Error(err)
				return
			}
			if err := track.Unbind(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if h.bindings.Load() != 0 {
		t.Fatal("concurrent lifecycle left stale binding proof")
	}
}

func TestFeedbackAudioHealthLoopbackPipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	a, b := localPeer(t, ctx), localPeer(t, ctx)
	sendEpoch, receiveEpoch := newStationEpoch(ctx, 801), newStationEpoch(ctx, 802)
	defer sendEpoch.revoke(context.Canceled)
	defer receiveEpoch.revoke(context.Canceled)
	h := sendEpoch.output.health
	h.start()
	receiveEpoch.output.health.start()
	if err := a.pc.GetSenders()[0].ReplaceTrack(&observedMicTrack{TrackLocal: a.mic, health: h}); err != nil {
		t.Fatal(err)
	}
	connectLocalPeers(t, ctx, a, b)
	input := make(chan captureFrame, 10)
	for i := range cap(input) {
		input <- toneFrame(801, i, 440)
	}
	sent, received := make(chan error, 1), make(chan error, 1)
	go func() { sent <- sendEpoch.output.send(ctx, a.mic, input) }()
	select {
	case track := <-b.tracks:
		go func() { received <- receiveEpoch.output.receive(ctx, track) }()
	case <-ctx.Done():
		t.Fatal("no loopback remote RTP")
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for receiveEpoch.output.health.playbackQueued.Load() < playbackBufferFrames {
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("no decoded loopback audio")
		}
	}
	output := make([]byte, samplesPerFrame*4)
	receiveEpoch.output.callback(output, nil, samplesPerFrame)
	if h.count[audioEncoded].Load() == 0 || h.count[audioSampleWrites].Load() == 0 || h.count[audioRTPWrites].Load() == 0 || h.bindings.Load() != 1 {
		t.Fatal("microphone pipeline counters missing")
	}
	r := receiveEpoch.output.health
	if r.count[audioRemoteRTP].Load() == 0 || r.count[audioDecodedFrames].Load() == 0 || r.count[audioPlaybackFrames].Load() == 0 || r.count[audioPlaybackNonzero].Load() == 0 {
		t.Fatal("receive/render counters missing")
	}
	cancel()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("sender cleanup blocked")
	}
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("receiver cleanup blocked")
	}
}
