package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"alisa-dreame-readonly/internal/bridge"
	"alisa-dreame-readonly/internal/cloud"
)

func main() {
	fixturePath := flag.String("fixture", "", "synthetic offline fixture file")
	sessionPath := flag.String("session", "", "Linux current-user-owned protected issued-session file")
	sessionStdin := flag.Bool("session-stdin", false, "read {request,session} from protected stdin")
	mapProfilePath := flag.String("map-profile", "", "trusted deployment map profile; rooms only")
	control := flag.Bool("control", false, "explicit protected single-room control protocol")
	importPath := flag.String("import-session", "", "save an issued session from protected stdin; no cloud calls")
	tokenImportPath := flag.String("import-token", "", "assemble and save an issued-token export from protected stdin; no cloud calls")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected arguments")
		os.Exit(2)
	}
	if *control {
		if *sessionPath == "" || *mapProfilePath == "" || *sessionStdin || *fixturePath != "" || *importPath != "" || *tokenImportPath != "" {
			emitControlError("control_mode_denied")
			return
		}
		req, err := cloud.DecodeControlRequest(os.Stdin)
		if err != nil {
			emitControlError("invalid_control_request")
			return
		}
		session, err := cloud.LoadSessionFile(*sessionPath, time.Now())
		if err != nil {
			emitControlError("control_session_unavailable")
			return
		}
		client, err := cloud.NewClient(*session)
		if err != nil {
			emitControlError("control_session_unavailable")
			return
		}
		defer client.Close()
		profile, err := cloud.LoadMapReadProfile(*mapProfilePath)
		if err != nil {
			emitControlError("control_profile_unavailable")
			return
		}
		if client.SetMapReadProfile(profile) != nil || client.EnableControlState(*sessionPath) != nil {
			emitControlError("control_state_unavailable")
			return
		}
		json.NewEncoder(os.Stdout).Encode(client.ExecuteControl(context.Background(), req))
		return
	}
	modes := 0
	if *fixturePath != "" {
		modes++
	}
	if *sessionPath != "" {
		modes++
	}
	if *sessionStdin {
		modes++
	}
	if *importPath != "" {
		modes++
	}
	if *tokenImportPath != "" {
		modes++
	}
	if modes > 1 {
		emitError("cli", "conflicting_modes")
		return
	}
	if *mapProfilePath != "" && (*sessionPath == "" && !*sessionStdin || *fixturePath != "" || *importPath != "" || *tokenImportPath != "") {
		emitError("cli", "map_profile_denied")
		return
	}
	if *importPath != "" || *tokenImportPath != "" {
		destination := *importPath
		if *tokenImportPath != "" {
			destination = *tokenImportPath
		}
		unlock, lockErr := cloud.LockSession(destination)
		if lockErr != nil {
			emitError("session_import", "session_save_failed")
			return
		}
		defer unlock()
		var session *cloud.Session
		var err error
		if *tokenImportPath != "" {
			session, err = cloud.AssembleIssuedSession(os.Stdin, time.Now())
			importPath = tokenImportPath
		} else {
			session, err = cloud.ParseSession(os.Stdin, time.Now())
		}
		if err != nil {
			emitError("session_import", "session_invalid_or_expired")
			return
		}
		if err := cloud.SaveSessionFile(*importPath, *session, time.Now()); err != nil {
			emitError("session_import", "session_save_failed")
			return
		}
		json.NewEncoder(os.Stdout).Encode(bridge.Response{Schema: 1, Mode: "session_import", ReadOnly: true, OK: true, Data: map[string]bool{"session_saved": true, "cloud_calls": false}})
		return
	}
	var req bridge.Request
	var session *cloud.Session
	if *sessionStdin {
		var envelope struct {
			Request bridge.Request `json:"request"`
			Session cloud.Session  `json:"session"`
		}
		if bridge.DecodeOne(os.Stdin, cloud.MaxSession+bridge.MaxRequest, &envelope, true) != nil {
			emitError("dreame_cloud", "invalid_protected_input")
			return
		}
		req = envelope.Request
		session = &envelope.Session
	} else {
		if err := bridge.DecodeOne(os.Stdin, bridge.MaxRequest, &req, true); err != nil {
			emitError("cli", "invalid_request")
			return
		}
	}
	if *sessionPath != "" {
		var err error
		session, err = cloud.LoadSessionFile(*sessionPath, time.Now())
		if err != nil {
			emitSessionError(err)
			return
		}
	}
	if session != nil {
		if *mapProfilePath != "" && req.Op != "rooms" {
			emitError("dreame_cloud", "map_profile_denied")
			return
		}
		client, err := cloud.NewClient(*session)
		if err != nil {
			emitSessionError(err)
			return
		}
		defer client.Close()
		if *mapProfilePath != "" {
			profile, err := cloud.LoadMapReadProfile(*mapProfilePath)
			if err != nil {
				emitSessionError(err)
				return
			}
			if err := client.SetMapReadProfile(profile); err != nil {
				emitSessionError(err)
				return
			}
			if *sessionPath != "" {
				_ = client.EnableControlState(*sessionPath)
			}
		}
		json.NewEncoder(os.Stdout).Encode(client.Execute(context.Background(), req))
		return
	}
	if *mapProfilePath != "" {
		emitError("cli", "map_profile_denied")
		return
	}
	var fixture *bridge.Fixture
	if *fixturePath != "" {
		file, err := os.Open(*fixturePath)
		if err != nil {
			emitError("offline_fixture", "fixture_unavailable")
			return
		}
		defer file.Close()
		fixture, err = bridge.LoadFixture(file)
		if err != nil {
			emitError("offline_fixture", "fixture_invalid")
			return
		}
	}
	json.NewEncoder(os.Stdout).Encode(bridge.Execute(fixture, req))
}

func emitControlError(code string) {
	json.NewEncoder(os.Stdout).Encode(bridge.Response{Schema: 1, Mode: "dreame_control", ReadOnly: false, Error: &bridge.Fault{Code: code}})
}

func emitError(mode, code string) {
	json.NewEncoder(os.Stdout).Encode(bridge.Response{Schema: 1, Mode: mode, ReadOnly: true, Error: &bridge.Fault{Code: code}})
}

func emitSessionError(err error) {
	var fault *bridge.Fault
	if errors.As(err, &fault) {
		emitError("dreame_cloud", fault.Code)
		return
	}
	emitError("dreame_cloud", "session_invalid")
}
