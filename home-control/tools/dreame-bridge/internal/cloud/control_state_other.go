//go:build !linux

package cloud

func newControlStore(string) (controlStore, error) {
	return nil, sessionError("control_state_unavailable")
}
