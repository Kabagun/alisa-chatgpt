package voice

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"math/big"
	"sync/atomic"
	"time"
	"uuid"
)

var startupGreetingIntents = [...]string{
	"Коротко поздоровайся и спроси, чем можешь помочь.",
	"Дружелюбно спроси, с чего начнём.",
	"Коротко поприветствуй и предложи задать вопрос.",
	"Легко и непринуждённо спроси, что обсудим.",
}

type startupGreeting struct{ attempted atomic.Bool }

func randomGreetingIndex(n int) (int, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(value.Int64()), nil
}

// This is the website's ordinary user-text relay, not a tool/system message.
// The generated greeting therefore uses the current Voice and Project and adds
// one normal startup turn to the website conversation history.
func encodeStartupGreeting(now time.Time, intent int) ([]byte, error) {
	if intent < 0 || intent >= len(startupGreetingIntents) {
		return nil, errors.New("invalid startup greeting intent")
	}
	text := "Служебный запуск голосового сеанса. Ответь одной короткой естественной фразой на русском, без инструментов и действий. Вариант: " + startupGreetingIntents[intent] + " После этого молча жди пользователя."
	message := map[string]any{
		"id": uuid.New().String(), "author": map[string]any{"role": "user"},
		"create_time":    float64(now.UnixMilli()) / 1000,
		"content":        map[string]any{"content_type": "text", "parts": []string{text}},
		"metadata":       map[string]any{"serialization_metadata": map[string]any{"custom_symbol_offsets": []any{}}},
		"clientMetadata": map[string]any{"isOptimistic": true},
	}
	inner, err := json.Marshal(map[string]any{"type": "relay_message", "payload": map[string]any{"type": "relay_message", "message": message}})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"type": "data_message", "data": string(inner)})
}

func (g *startupGreeting) send(ctx context.Context, mediaReady bool, now time.Time, choose func(int) (int, error), send func([]byte) error) {
	if !mediaReady || ctx.Err() != nil || !g.attempted.CompareAndSwap(false, true) {
		return
	}
	intent, err := choose(len(startupGreetingIntents))
	if err != nil {
		return
	}
	data, err := encodeStartupGreeting(now, intent)
	if err != nil || ctx.Err() != nil {
		return
	}
	// Consume even an uncertain send outcome. A failure never retries, closes
	// a healthy call, or creates an unbounded queue of startup turns.
	_ = send(data)
}
