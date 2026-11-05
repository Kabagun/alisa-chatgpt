//go:build !linux

package cloud

import "time"

func LockSession(string) (func(), error) { return nil, sessionError("protected_file_requires_linux") }

func CheckSessionDestination(string) error { return sessionError("protected_file_requires_linux") }

func LoadSessionForRefresh(string, time.Time) (*Session, error) {
	return nil, sessionError("protected_file_requires_linux")
}

func LoadSessionFile(string, time.Time) (*Session, error) {
	return nil, sessionError("protected_file_requires_linux")
}
func SaveSessionFile(string, Session, time.Time) error {
	return sessionError("protected_file_requires_linux")
}
