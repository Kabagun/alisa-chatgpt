package voice

import "time"

const audibleMicThreshold = 256
const audiblePlaybackThreshold = 128

// Observe real processed microphone speech without retaining or changing PCM.
// Local spoken cues and periodic presence messages are intentionally absent.
func (i *voiceIdle) microphone(pcm []int16, now time.Time) {
	if i == nil || i.ctx.Err() != nil {
		return
	}
	for _, sample := range pcm {
		if sample > audibleMicThreshold || sample < -audibleMicThreshold {
			i.activityAt(now)
			return
		}
	}
}
