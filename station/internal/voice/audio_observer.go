package voice

import (
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Only the writer is wrapped. Negotiated parameters, IDs, readers, payload/header
// pointers, return byte counts and errors are delegated unchanged.
type observedMicTrack struct {
	webrtc.TrackLocal
	health *audioHealth
	mu     sync.Mutex
}

func (t *observedMicTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	codec, err := t.TrackLocal.Bind(observedMicContext{TrackLocalContext: ctx, health: t.health})
	if err == nil && t.health != nil {
		t.health.bindings.Add(1)
	}
	return codec, err
}
func (t *observedMicTrack) Unbind(ctx webrtc.TrackLocalContext) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	err := t.TrackLocal.Unbind(observedMicContext{TrackLocalContext: ctx, health: t.health})
	if err == nil && t.health != nil {
		t.health.bindings.Add(-1)
	}
	return err
}

type observedMicContext struct {
	webrtc.TrackLocalContext
	health *audioHealth
}

func (c observedMicContext) WriteStream() webrtc.TrackLocalWriter {
	return observedMicWriter{TrackLocalWriter: c.TrackLocalContext.WriteStream(), health: c.health}
}

type observedMicWriter struct {
	webrtc.TrackLocalWriter
	health *audioHealth
}

func (w observedMicWriter) written(n int, err error) {
	if err == nil && n > 0 {
		w.health.add(audioRTPWrites, 1)
		w.health.add(audioRTPBytes, uint64(n))
		w.health.activity(audioLastRTPWrite)
	}
}
func (w observedMicWriter) WriteRTP(header *rtp.Header, payload []byte) (int, error) {
	n, err := w.TrackLocalWriter.WriteRTP(header, payload)
	w.written(n, err)
	return n, err
}
func (w observedMicWriter) Write(b []byte) (int, error) {
	n, err := w.TrackLocalWriter.Write(b)
	w.written(n, err)
	return n, err
}
