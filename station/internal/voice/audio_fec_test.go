package voice

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/hraban/opus"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestRemoteAudioRecoversLatestGapWithFEC(t *testing.T) {
	for _, tc := range []struct {
		name string
		gap  int
		fec  bool
	}{
		{"one missing frame", 1, true},
		{"two missing frames", 2, true},
		{"three missing frames and clock wrap", 3, true},
		{"no redundancy falls back to PLC", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			application := opus.AppVoIP
			if !tc.fec {
				application = opus.AppRestrictedLowdelay // CELT has no in-band FEC.
			}
			encoder, err := opus.NewEncoder(16000, 1, application)
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range []error{encoder.SetBitrate(20000), encoder.SetMaxBandwidth(opus.Wideband), encoder.SetInBandFEC(tc.fec), encoder.SetPacketLossPerc(30)} {
				if err != nil {
					t.Fatal(err)
				}
			}
			const missingAt = 4
			next := missingAt + tc.gap
			packets := make([][]byte, next+2)
			for frame := range packets {
				var pcm [captureSamples]int16
				for i := range pcm {
					at := float64(frame*captureSamples+i) / 16000
					// Deterministic voiced speech-like harmonics and envelope,
					// bounded to the ordinary unmodified 12000-peak input range.
					envelope := 0.65 + 0.35*math.Sin(2*math.Pi*3*at)
					pcm[i] = int16(12000 * envelope * (0.6*math.Sin(2*math.Pi*150*at) + 0.25*math.Sin(2*math.Pi*900*at) + 0.15*math.Sin(2*math.Pi*2400*at)))
				}
				var data [4000]byte
				n, err := encoder.Encode(pcm[:], data[:])
				if err != nil {
					t.Fatal(err)
				}
				packets[frame] = slices.Clone(data[:n])
			}
			decoder, err := opus.NewDecoder(48000, 2)
			if err != nil {
				t.Fatal(err)
			}
			plcDecoder, err := opus.NewDecoder(48000, 2)
			if err != nil {
				t.Fatal(err)
			}
			want := make([][]int16, len(packets))
			for frame := range packets {
				want[frame] = make([]int16, samplesPerFrame*2)
				other := make([]int16, samplesPerFrame*2)
				switch {
				case frame >= missingAt && frame < next-1:
					err = decoder.DecodePLC(want[frame])
					if err == nil {
						err = plcDecoder.DecodePLC(other)
					}
				case frame == next-1:
					err = decoder.DecodeFEC(packets[next], want[frame])
					if err == nil {
						err = plcDecoder.DecodePLC(other)
					}
					if err == nil && slices.Equal(want[frame], other) == tc.fec {
						t.Fatal("codec fixture did not distinguish genuine FEC from PLC fallback")
					}
				default:
					var n int
					n, err = decoder.Decode(packets[frame], want[frame])
					if err == nil && n != samplesPerFrame {
						t.Fatal("fixture changed media duration")
					}
					if err == nil {
						_, err = plcDecoder.Decode(packets[frame], other)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			a, b := localPeer(t, ctx), localPeer(t, ctx)
			raw, err := webrtc.NewTrackLocalStaticRTP(b.mic.Codec(), b.mic.ID(), b.mic.StreamID())
			if err != nil {
				t.Fatal(err)
			}
			if err := b.pc.GetSenders()[0].ReplaceTrack(raw); err != nil {
				t.Fatal(err)
			}
			connectLocalPeers(t, ctx, a, b)
			e := newStationEpoch(ctx, 1)
			done := make(chan error, 1)
			go func() {
				select {
				case track := <-a.tracks:
					done <- e.output.receive(e.ctx, track)
				case <-ctx.Done():
					done <- ctx.Err()
				}
			}()
			t.Cleanup(func() { e.revoke(context.Canceled); cancel(); _ = a.close(); _ = b.close(); <-done })
			sequence, timestamp := uint16(100), uint32(1000)
			if tc.gap == 3 {
				sequence, timestamp = 65532, ^uint32(0)-2*samplesPerFrame
			}
			for frame, packet := range packets {
				if frame >= missingAt && frame < next {
					continue
				}
				if err := raw.WriteRTP(&rtp.Packet{Version: 2, SequenceNumber: sequence + uint16(frame), Timestamp: timestamp + uint32(frame)*samplesPerFrame, Payload: packet}); err != nil {
					t.Fatal(err)
				}
			}
			for frame, pcm := range want {
				select {
				case block := <-e.output.playback:
					if block.used != samplesPerFrame || !slices.Equal(block.pcm[:samplesPerFrame*2], pcm) {
						t.Fatalf("frame %d did not preserve PLC/FEC/current-packet decode order", frame)
					}
				case err := <-done:
					done <- err
					t.Fatal("receiver ended", err)
				case <-ctx.Done():
					t.Fatal("gap recovery failed to release the media tail")
				}
			}
			h := e.output.health
			if h.count[audioRemoteFECAttempts].Load() != 1 || h.count[audioRemotePLCFrames].Load() != uint64((tc.gap-1)*samplesPerFrame) || h.count[audioRemoteDecoderResets].Load() != 0 {
				t.Fatal("FEC attempts, explicit PLC, and decoder resets were misreported")
			}
		})
	}
}
