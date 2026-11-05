package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"alisa-dreame-readonly/internal/bridge"
	"alisa-dreame-readonly/internal/cloud"
)

func main() {
	path := flag.String("session", "", "current-user-owned session.json in a private 0700 Linux directory")
	refresh := flag.Bool("refresh", false, "renew an imported refresh token; no password fallback")
	flag.Parse()
	if *path == "" || flag.NArg() != 0 {
		emit(nil, &bridge.Fault{Code: "provision_arguments_invalid"})
		return
	}
	unlock, lockErr := cloud.LockSession(*path)
	if lockErr != nil {
		emit(nil, lockErr)
		return
	}
	defer unlock()
	provisioner := cloud.NewProvisioner()
	defer provisioner.Close()
	var session *cloud.Session
	var err error
	if *refresh {
		var prior *cloud.Session
		prior, err = cloud.LoadSessionForRefresh(*path, time.Now())
		if err == nil {
			session, err = provisioner.Refresh(context.Background(), *prior)
		}
		if err == nil {
			err = cloud.SaveSessionFile(*path, *session, time.Now())
		}
	} else {
		var input *cloud.LoginInput
		input, err = cloud.ParseLoginInput(os.Stdin)
		if err == nil {
			session, err = provisioner.PasswordLoginAndSave(context.Background(), *input, *path)
		}
		if input != nil {
			input.Password = ""
			input.Username = ""
		}
	}
	if err != nil {
		emit(nil, err)
		return
	}
	emit(map[string]any{"session_saved": true, "refresh_available": session.RefreshToken != "", "password_stored": false, "resolved_server_region": session.ServerRegion, "routing_verified": session.RoutingProof != nil}, nil)
}

func emit(data any, err error) {
	r := bridge.Response{Schema: 1, Mode: "dreame_provision", ReadOnly: true, OK: err == nil, Data: data}
	if err != nil {
		var f *bridge.Fault
		if errors.As(err, &f) {
			r.Error = f
		} else {
			r.Error = &bridge.Fault{Code: "provision_failed"}
		}
	}
	json.NewEncoder(os.Stdout).Encode(r)
}
