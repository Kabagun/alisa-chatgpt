package voice

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

const commandTTL = 10 * time.Second
const commandIdleStability = 400 * time.Millisecond

func normalizeActivationCommand(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) {
			return ' '
		}
		return r
	}, strings.ToLower(text))
	return strings.Join(strings.Fields(text), " ")
}

func validActivationCommand(text string) bool {
	return len(text) <= 256 && utf8.ValidString(text) && normalizeActivationCommand(text) != ""
}

// commandTrigger is protected by stationGate.mu. Its phrase is immutable.
type commandTrigger struct {
	phrase       string
	state        aliceState
	armedUntil   time.Time
	pending      bool
	deadline     time.Time
	idleSince    time.Time
	generation   uint64
	lastAccepted uint64
	matchedAt    time.Time
}

func (c *commandTrigger) observe(ev nativeEvent, now time.Time) {
	if ev.activation {
		c.generation++
		c.armedUntil = now.Add(commandTTL)
		c.pending, c.state, c.idleSince = false, aliceUnknown, time.Time{}
	}
	if ev.stateChanged {
		if ev.state == aliceListening && c.state != aliceListening {
			c.generation++
			c.armedUntil = now.Add(commandTTL)
			c.pending = false
		}
		if ev.state == aliceIdle {
			c.armedUntil = time.Time{}
			if c.state != aliceIdle {
				c.idleSince = now
			}
		} else {
			c.idleSince = time.Time{}
		}
		c.state = ev.state
	}
	if !ev.commandMatch || !now.Before(c.armedUntil) || c.lastAccepted == c.generation {
		return
	}
	// Consume this activation/listening generation once, including duplicate
	// final/partial callbacks with no usable request ID. A new native utterance
	// is required before accepting another command; cached startup text is inert.
	c.lastAccepted = c.generation
	c.armedUntil = time.Time{}
	c.pending, c.deadline, c.idleSince = true, now.Add(commandTTL), time.Time{}
	c.matchedAt = now
}

func (c *commandTrigger) take(now time.Time) bool {
	if !c.pending {
		return false
	}
	if !now.Before(c.deadline) {
		c.pending = false
		return false
	}
	if c.state != aliceIdle || c.idleSince.IsZero() || now.Sub(c.idleSince) < commandIdleStability {
		return false
	}
	c.pending = false
	return true
}

func decodeActivationCommand(e ipcEnvelope, command string) (match bool) {
	if e.kind != 3 {
		return
	}
	state, err := decodeAlice(e.payload)
	if err != nil || state != aliceBusy {
		return
	}
	var textSeen bool
	err = wireFields(e.payload, func(f wireField) error {
		switch f.number {
		case 2:
			if err := f.require(protowire.BytesType, &textSeen); err != nil {
				return err
			}
			if len(f.bytes) <= 4096 && utf8.Valid(f.bytes) {
				text := normalizeActivationCommand(string(f.bytes))
				match = text == command || text == "алиса "+command
			}
		}
		return nil
	})
	if err != nil {
		return false
	}
	return
}
