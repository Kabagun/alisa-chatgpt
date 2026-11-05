//go:build !linux

package dreame

import (
	"os/exec"
	"smart-home-plugin/internal/provider"
)

// Production session ownership guarantees require Linux. Other builds fail
// closed instead of approximating POSIX permissions on another filesystem.
func trustedFile(string) bool               { return false }
func executable(string) bool                { return false }
func privateSession(string) bool            { return false }
func accountSession(string) (string, error) { return "", provider.Err("provider_unavailable") }
func configureProcess(*exec.Cmd)            {}
