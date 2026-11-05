package main

import "encoding/binary"

const sampleRate = 16000
const frameSamples = 320
const maximumFixtureSamples = 20 * sampleRate

// Only RIFF/WAVE, integer PCM16, 16 kHz, mono. Unknown metadata chunks are
// skipped within the existing file bound; none of their contents are logged.
func parseWave(b []byte) ([]int16, bool) {
	if len(b) < 44 || len(b) > maximumFixtureBytes || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" || uint64(binary.LittleEndian.Uint32(b[4:8]))+8 != uint64(len(b)) {
		return nil, false
	}
	var data []byte
	foundFormat := false
	for offset := 12; offset < len(b); {
		if len(b)-offset < 8 {
			return nil, false
		}
		size := uint64(binary.LittleEndian.Uint32(b[offset+4 : offset+8]))
		start := offset + 8
		if size > uint64(len(b)-start) {
			return nil, false
		}
		end := start + int(size)
		switch string(b[offset : offset+4]) {
		case "fmt ":
			if foundFormat || size != 16 && size != 18 {
				return nil, false
			}
			f := b[start:end]
			if binary.LittleEndian.Uint16(f[0:2]) != 1 || binary.LittleEndian.Uint16(f[2:4]) != 1 || binary.LittleEndian.Uint32(f[4:8]) != sampleRate || binary.LittleEndian.Uint32(f[8:12]) != sampleRate*2 || binary.LittleEndian.Uint16(f[12:14]) != 2 || binary.LittleEndian.Uint16(f[14:16]) != 16 || size == 18 && binary.LittleEndian.Uint16(f[16:18]) != 0 {
				return nil, false
			}
			foundFormat = true
		case "data":
			if data != nil || size == 0 || size%2 != 0 || size/2 > maximumFixtureSamples {
				return nil, false
			}
			data = b[start:end]
		}
		offset = end + int(size%2)
		if offset > len(b) {
			return nil, false
		}
	}
	if !foundFormat || data == nil {
		return nil, false
	}
	pcm := make([]int16, len(data)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(data[2*i : 2*i+2]))
	}
	return pcm, true
}
