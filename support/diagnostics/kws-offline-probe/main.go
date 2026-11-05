package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const processBudget = 30 * time.Second
const cleanupReserve = 2 * time.Second
const frameDuration = 20 * time.Millisecond
const leadingFrames = 50
const trailingFrames = 100

type dependencies struct {
	open func(string) (detector, bool)
	pace func(context.Context) bool
}

func execute(ctx context.Context, root string, out io.Writer, deps dependencies) (code int) {
	started := time.Now()
	var frames, matches uint64
	reason := "ERROR"
	defer func() {
		fmt.Fprintf(out, "SUMMARY elapsed_ms=%d pcm_frames=%d matches=%d reason=%s\n", time.Since(started).Milliseconds(), frames, matches, reason)
	}()
	pcm, ok := loadFixture(root)
	if !ok {
		fmt.Fprintln(out, "FIXTURE_REJECTED")
		return 2
	}
	defer clear(pcm)
	lock := filepath.Join(root, "lock")
	if os.Mkdir(lock, 0700) != nil {
		fmt.Fprintln(out, "LOCK_UNAVAILABLE")
		return 2
	}
	defer func() {
		if !privateDirectory(lock) || os.Remove(lock) != nil {
			fmt.Fprintln(out, "CLEANUP_FAILED")
			code = 1
		}
	}()
	d, ok := deps.open(root)
	if !ok {
		fmt.Fprintln(out, "SPOTTER_INIT_FAILED")
		return 1
	}
	defer func() {
		if !d.close() {
			fmt.Fprintln(out, "CLEANUP_FAILED")
			code = 1
		} else {
			fmt.Fprintln(out, "SPOTTER_CLOSED")
		}
	}()
	fmt.Fprintln(out, "NATIVE_SPOTTER_READY")
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()
	pace := deps.pace
	if pace == nil {
		pace = func(ctx context.Context) bool {
			select {
			case <-ctx.Done():
				return false
			case <-ticker.C:
				return ctx.Err() == nil
			}
		}
	}
	var frame [frameSamples]int16
	defer clear(frame[:])
	lastMatch := time.Time{}
	printed := 0
	feedFrame := func() bool {
		defer clear(frame[:])
		if ctx.Err() != nil || !pace(ctx) {
			return false
		}
		hit, valid := d.feed(frame[:])
		if !valid {
			fmt.Fprintln(out, "SPOTTER_FEED_FAILED")
			return false
		}
		frames++
		if hit && (lastMatch.IsZero() || time.Since(lastMatch) >= 500*time.Millisecond) {
			matches++
			lastMatch = time.Now()
			if printed < 64 {
				fmt.Fprintf(out, "KEYWORD_MATCH elapsed_ms=%d count=%d\n", time.Since(started).Milliseconds(), matches)
				printed++
			}
		}
		return true
	}
	finishInterrupted := func() int {
		switch ctx.Err() {
		case context.DeadlineExceeded:
			reason = "TIME_LIMIT"
			return 0
		case context.Canceled:
			reason = "SIGNAL"
			return 0
		default:
			return 1
		}
	}
	for i := 0; i < leadingFrames; i++ {
		if !feedFrame() {
			return finishInterrupted()
		}
	}
	for offset := 0; offset < len(pcm); offset += frameSamples {
		copy(frame[:], pcm[offset:min(offset+frameSamples, len(pcm))])
		if !feedFrame() {
			return finishInterrupted()
		}
		if offset == 0 {
			fmt.Fprintln(out, "FIXTURE_ACTIVE")
		}
	}
	for i := 0; i < trailingFrames; i++ {
		if !feedFrame() {
			return finishInterrupted()
		}
	}
	reason = "FIXTURE_COMPLETE"
	return 0
}

func cli(args []string, out io.Writer) int {
	started := time.Now()
	fs := flag.NewFlagSet("kws-offline-probe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	run := fs.Bool("run", false, "")
	check := fs.Bool("check", false, "")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *run && *check {
		fmt.Fprintln(out, "INVALID_OPTIONS")
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(out, "ROOT_REQUIRED")
		return 2
	}
	if !*run {
		pcm, ok := loadFixture(probeRoot)
		clear(pcm)
		if !ok || !preflightModel(probeRoot) {
			fmt.Fprintln(out, "PREFLIGHT_FAILED")
			return 2
		}
		fmt.Fprintln(out, "PREFLIGHT_OK")
		return 0
	}
	structured, ok := quietNativeOutput()
	if !ok {
		fmt.Fprintln(out, "OUTPUT_ISOLATION_FAILED")
		return 2
	}
	defer structured.Close()
	if !protectOwnProcess() {
		fmt.Fprintln(structured, "PROCESS_PROTECTION_FAILED")
		return 2
	}
	fmt.Fprintln(structured, "PROCESS_PROTECTED")
	remaining := processBudget - time.Since(started)
	if remaining <= cleanupReserve {
		fmt.Fprintln(structured, "TIME_LIMIT")
		return 2
	}
	watchdog := time.AfterFunc(remaining, func() { fmt.Fprintln(structured, "WATCHDOG_TIMEOUT"); os.Exit(124) })
	defer watchdog.Stop()
	parent, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(parent, remaining-cleanupReserve)
	defer cancel()
	if !preflightModel(probeRoot) {
		fmt.Fprintln(structured, "PREFLIGHT_FAILED")
		return 2
	}
	return execute(ctx, probeRoot, structured, dependencies{open: openNative})
}

func main() { os.Exit(cli(os.Args[1:], os.Stdout)) }
