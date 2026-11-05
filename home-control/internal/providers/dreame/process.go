package dreame

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"sync"
	"time"

	"smart-home-plugin/internal/provider"
)

type boundedOutput struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	remaining int
	discard   bool
	overflow  bool
	cancel    context.CancelFunc
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if n > b.remaining {
		b.overflow = true
		b.cancel()
		return n, nil
	}
	b.remaining -= n
	if !b.discard {
		_, _ = b.buffer.Write(p)
	}
	return n, nil
}
func (b *boundedOutput) exceeded() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.overflow }

func (a *Adapter) run(parent context.Context, path string, args []string, input []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, a.timeout)
	defer cancel()
	stdout := &boundedOutput{remaining: provider.MaxResultBytes, cancel: cancel}
	stderr := &boundedOutput{remaining: 64 << 10, discard: true, cancel: cancel}
	cmd := exec.CommandContext(ctx, path, args...)
	// No inherited proxy, token, password or other application environment.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = time.Second
	configureProcess(cmd)
	err := cmd.Run() // Always waits; cancellation kills the process group on Linux.
	if stdout.exceeded() || stderr.exceeded() {
		return nil, provider.Err("provider_unavailable")
	}
	if ctx.Err() != nil {
		return nil, provider.Err("upstream_unavailable")
	}
	// CLI failures have a safe JSON envelope even on nonzero exit. Never include
	// exec errors, stderr, paths or raw stdout in the public error.
	if err != nil {
		var failed struct {
			OK *bool `json:"ok"`
		}
		if json.Unmarshal(stdout.buffer.Bytes(), &failed) != nil || failed.OK == nil || *failed.OK {
			return nil, provider.Err("provider_unavailable")
		}
	}
	return stdout.buffer.Bytes(), nil
}
