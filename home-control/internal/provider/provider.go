// Package provider defines the fixed smart-home backend boundary.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Operation string

const (
	Devices        Operation = "devices"
	Status         Operation = "status"
	Rooms          Operation = "rooms"
	CleanRooms     Operation = "clean_rooms"
	MaxResultBytes           = 32 << 10
)

// Credentials exist only for the duration of Authenticate. Formatting and JSON
// deliberately redact the whole value, including under %#v formatting.
type Credentials struct {
	Username string `json:"-"`
	Password string `json:"-"`
	// An empty Region lets the provider establish and verify a working origin.
	Region string `json:"-"`
}

func (Credentials) String() string               { return "[credentials redacted]" }
func (Credentials) GoString() string             { return "[credentials redacted]" }
func (Credentials) MarshalJSON() ([]byte, error) { return []byte(`"[credentials redacted]"`), nil }
func (c Credentials) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte(c.String())) }

type Params struct {
	DeviceID string   `json:"device_id,omitempty"`
	MapID    string   `json:"map_id,omitempty"`
	RoomIDs  []string `json:"room_ids,omitempty"`
}

func identifier(s string) bool {
	return s != "" && len(s) <= 128 && !strings.ContainsAny(s, "\x00\r\n")
}

// Validate limits shape only. Providers must prove that devices/maps/rooms belong
// to this account, and that a requested action is supported, before execution.
func (p Params) Validate(op Operation) error {
	switch op {
	case Devices:
		if p.DeviceID != "" || p.MapID != "" || len(p.RoomIDs) != 0 {
			return Err("invalid_params")
		}
	case Status, Rooms:
		if !identifier(p.DeviceID) || p.MapID != "" || len(p.RoomIDs) != 0 {
			return Err("invalid_params")
		}
	case CleanRooms:
		if !identifier(p.DeviceID) || !identifier(p.MapID) || len(p.RoomIDs) == 0 || len(p.RoomIDs) > 32 {
			return Err("invalid_params")
		}
		seen := make(map[string]bool)
		for _, id := range p.RoomIDs {
			if !identifier(id) || seen[id] {
				return Err("invalid_params")
			}
			seen[id] = true
		}
	default:
		return Err("operation_unavailable")
	}
	return nil
}

type Provider interface {
	ID() string
	Label() string
	Authenticate(context.Context, string, Credentials) error
	Execute(context.Context, string, Operation, Params) (json.RawMessage, error)
}

// Error carries only a public fixed code, never a vendor response or credential.
type Error struct{ Code string }

var codes = map[string]bool{
	"invalid_credentials": true, "invalid_region": true, "invalid_params": true,
	"provider_unavailable": true, "operation_unavailable": true,
	"room_control_unverified": true, "device_not_found": true,
	"account_connection_unverified": true,
	"upstream_unavailable":          true, "operation_failed": true, "busy": true,
}

func Err(code string) *Error {
	if !codes[code] {
		code = "provider_unavailable"
	}
	return &Error{Code: code}
}
func (e Error) Error() string {
	if codes[e.Code] {
		return e.Code
	}
	return "provider_unavailable"
}
func SafeCode(err error) string {
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e.Error()
	}
	return "provider_unavailable"
}
