package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var stopped = errors.New("stop requested")
var unsafeStop = errors.New("unsafe stop metadata")

type dependencies struct {
	open func(string) (detector, bool)
	dial func(context.Context) (net.Conn, error)
}

func execute(ctx context.Context, root string, out io.Writer, deps dependencies) (code int) {
	started := time.Now()
	var frames, matches uint64
	reason := "ERROR"
	defer func() {
		fmt.Fprintf(out, "SUMMARY elapsed_ms=%d pcm_frames=%d matches=%d reason=%s\n", time.Since(started).Milliseconds(), frames, matches, reason)
	}()
	if state := stopState(root); state != 0 {
		if state == 1 {
			reason = "STOP_FILE"
			return 0
		}
		fmt.Fprintln(out, "PRIVATE_PATH_REJECTED")
		return 2
	}
	lock := filepath.Join(root, "lock")
	if os.Mkdir(lock, 0700) != nil {
		fmt.Fprintln(out, "LOCK_UNAVAILABLE")
		return 2
	}
	defer func() {
		if os.Remove(lock) != nil {
			fmt.Fprintln(out, "CLEANUP_FAILED")
			code = 1
		}
	}()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				switch stopState(root) {
				case 1:
					cancel(stopped)
					return
				case -1:
					cancel(unsafeStop)
					return
				}
			}
		}
	}()
	defer func() { cancel(context.Canceled); <-monitorDone }()
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
	conn, err := deps.dial(ctx)
	if err != nil {
		fmt.Fprintln(out, "CAPTURE_CONNECT_FAILED")
		return 1
	}
	defer conn.Close()
	stopConn := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopConn()
	if conn.SetWriteDeadline(time.Now().Add(3*time.Second)) != nil {
		fmt.Fprintln(out, "CAPTURE_REQUEST_FAILED")
		return 1
	}
	request := subscription()
	for len(request) > 0 {
		n, err := conn.Write(request)
		if err != nil || n == 0 {
			fmt.Fprintln(out, "CAPTURE_REQUEST_FAILED")
			return 1
		}
		request = request[n:]
	}
	var partial [320]int16
	defer clear(partial[:])
	used := 0
	captureActive := false
	lastMatch := time.Time{}
	printed := 0
	for {
		if ctx.Err() != nil {
			break
		}
		if conn.SetReadDeadline(time.Now().Add(3*time.Second)) != nil {
			fmt.Fprintln(out, "CAPTURE_READ_FAILED")
			return 1
		}
		kind, payload, body, err := readFrame(conn)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			fmt.Fprintln(out, "CAPTURE_READ_FAILED")
			return 1
		}
		valid := true
		if kind == 158 {
			valid = decodePCM(payload, func(pcm []byte) bool {
				for len(pcm) > 0 {
					partial[used] = int16(binary.LittleEndian.Uint16(pcm))
					pcm = pcm[2:]
					used++
					if used != len(partial) {
						continue
					}
					hit, ok := d.feed(partial[:])
					clear(partial[:])
					used = 0
					if !ok {
						return false
					}
					frames++
					if !captureActive {
						captureActive = true
						fmt.Fprintln(out, "CAPTURE_ACTIVE")
					}
					if hit && (lastMatch.IsZero() || time.Since(lastMatch) >= 500*time.Millisecond) {
						matches++
						lastMatch = time.Now()
						if printed < 64 {
							fmt.Fprintf(out, "KEYWORD_MATCH elapsed_ms=%d count=%d\n", time.Since(started).Milliseconds(), matches)
							printed++
						}
					}
				}
				return true
			})
		}
		clear(body)
		if !valid {
			fmt.Fprintln(out, "CAPTURE_OR_SPOTTER_REJECTED")
			return 1
		}
	}
	switch context.Cause(ctx) {
	case stopped:
		reason = "STOP_FILE"
	case unsafeStop:
		fmt.Fprintln(out, "PRIVATE_PATH_REJECTED")
		return 2
	case context.DeadlineExceeded:
		reason = "TIME_LIMIT"
	default:
		reason = "SIGNAL"
	}
	return 0
}

func cli(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("kws-probe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	run := fs.Bool("run", false, "")
	check := fs.Bool("check", false, "")
	seconds := fs.Int("seconds", 60, "")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *seconds < 5 || *seconds > 600 || *run && *check {
		fmt.Fprintln(out, "INVALID_OPTIONS")
		return 2
	}
	if !preflight(probeRoot) {
		fmt.Fprintln(out, "PREFLIGHT_FAILED")
		return 2
	}
	if !*run {
		fmt.Fprintln(out, "PREFLIGHT_OK")
		return 0
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(out, "ROOT_REQUIRED")
		return 2
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
	watchdog := time.AfterFunc(time.Duration(*seconds)*time.Second, func() { fmt.Fprintln(structured, "WATCHDOG_TIMEOUT"); os.Exit(124) })
	defer watchdog.Stop()
	parent, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Reserve two seconds inside the total process budget for native cleanup.
	ctx, cancel := context.WithTimeout(parent, time.Duration(*seconds-2)*time.Second)
	defer cancel()
	return execute(ctx, probeRoot, structured, dependencies{open: openNative, dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp4", "127.0.0.1:9850")
	}})
}

func main() { os.Exit(cli(os.Args[1:], os.Stdout)) }
