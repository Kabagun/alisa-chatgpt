package voice

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartupGreetingMatchesBoundedWebsiteTextEnvelope(t *testing.T) {
	now := time.UnixMilli(123456789)
	seen := make(map[string]bool)
	for intent := range startupGreetingIntents {
		data, err := encodeStartupGreeting(now, intent)
		if err != nil || len(data) > 2048 {
			t.Fatal("unbounded startup relay", err)
		}
		var outer struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		var inner struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string `json:"type"`
				Message struct {
					ID     string `json:"id"`
					Author struct {
						Role string `json:"role"`
					} `json:"author"`
					CreateTime float64 `json:"create_time"`
					Content    struct {
						Type  string   `json:"content_type"`
						Parts []string `json:"parts"`
					} `json:"content"`
					Metadata struct {
						Serialization struct {
							Offsets []any `json:"custom_symbol_offsets"`
						} `json:"serialization_metadata"`
					} `json:"metadata"`
					Client struct {
						Optimistic bool `json:"isOptimistic"`
					} `json:"clientMetadata"`
				} `json:"message"`
			} `json:"payload"`
		}
		if json.Unmarshal(data, &outer) != nil || outer.Type != "data_message" || json.Unmarshal([]byte(outer.Data), &inner) != nil || inner.Type != "relay_message" || inner.Payload.Type != "relay_message" {
			t.Fatal("website relay envelope changed")
		}
		m := inner.Payload.Message
		if m.Author.Role != "user" || m.CreateTime != float64(now.UnixMilli())/1000 || m.Content.Type != "text" || len(m.Content.Parts) != 1 || len(m.ID) != 36 || seen[m.ID] || !m.Client.Optimistic || m.Metadata.Serialization.Offsets == nil || len(m.Metadata.Serialization.Offsets) != 0 {
			t.Fatal("website base user-message shape changed")
		}
		seen[m.ID] = true
		text := m.Content.Parts[0]
		if !strings.HasPrefix(text, "Служебный запуск голосового сеанса.") || !strings.Contains(text, startupGreetingIntents[intent]) || !strings.Contains(text, "молча жди") {
			t.Fatal("startup intent or silence instruction missing")
		}
		for _, unsupported := range []string{"client_tool", "function_call", `"role":"system"`, `"role":"assistant"`} {
			if strings.Contains(outer.Data, unsupported) {
				t.Fatal("unsupported tool/role appeared in startup relay")
			}
		}
	}
}

func TestStartupGreetingOnlyOnceAfterAdmissionDespiteReconnect(t *testing.T) {
	g := &startupGreeting{}
	var sent atomic.Int32
	choose := func(int) (int, error) { return 2, nil }
	send := func([]byte) error { sent.Add(1); return nil }
	g.send(t.Context(), false, time.Now(), choose, send)
	if sent.Load() != 0 || g.attempted.Load() {
		t.Fatal("preparation created a greeting")
	}
	var workers sync.WaitGroup
	for range 20 {
		workers.Go(func() { g.send(t.Context(), true, time.Now(), choose, send) })
	}
	workers.Wait()
	// Reconnect/readiness repeats and later timer ticks reuse the consumed guard.
	for range 10 {
		g.send(t.Context(), true, time.Now().Add(time.Hour), choose, send)
	}
	if sent.Load() != 1 {
		t.Fatal("one admitted call sent repeated startup turns")
	}
	fresh := &startupGreeting{}
	fresh.send(t.Context(), true, time.Now(), choose, send)
	if sent.Load() != 2 {
		t.Fatal("next activation did not get its own greeting")
	}
}

func TestStartupGreetingCancellationAndFailureNeverRetry(t *testing.T) {
	for _, at := range []string{"before_ready", "during_choice", "send_failure", "random_failure"} {
		t.Run(at, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(context.Canceled)
			g := &startupGreeting{}
			sends := 0
			if at == "before_ready" {
				cancel(errAlicePriority)
			}
			choose := func(int) (int, error) {
				if at == "during_choice" {
					cancel(errAlicePriority)
				}
				if at == "random_failure" {
					return 0, errors.New("synthetic entropy failure")
				}
				return 0, nil
			}
			send := func([]byte) error { sends++; return errors.New("synthetic transport failure") }
			for range 3 {
				g.send(ctx, true, time.Now(), choose, send)
			}
			want := 0
			if at == "send_failure" {
				want = 1
			}
			if sends != want || at == "send_failure" && ctx.Err() != nil {
				t.Fatal("cancellation/failure retried a startup turn or killed a healthy call")
			}
		})
	}
}
