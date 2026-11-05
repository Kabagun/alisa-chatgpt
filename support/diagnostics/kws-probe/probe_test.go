package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

func number(b []byte, n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), v)
}
func frame(kind uint64, payload []byte) []byte {
	b := number(bytesField(nil, 252, payload), 253, kind)
	return append(binary.LittleEndian.AppendUint32(nil, uint32(len(b))), b...)
}
func audioFixture(name string, rate, format uint64, pcm []byte) []byte {
	b := bytesField(nil, 2, []byte(name))
	b = bytesField(b, 3, pcm)
	b = number(b, 7, rate)
	b = number(b, 8, format)
	return bytesField(nil, 1, b)
}
func privateRoot(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if os.Mkdir(dir, 0700) != nil {
		t.Fatal("fixture")
	}
	return dir
}

func TestSubscriptionIsOnlyKnownAudioRequest(t *testing.T) {
	k, p, b, err := readFrame(bytes.NewReader(subscription()))
	defer clear(b)
	if err != nil || k != 159 || !bytes.Equal(p, []byte{10, 0}) {
		t.Fatal("subscription differs from legacy audio request")
	}
}

func TestProcessedFormatAndBounds(t *testing.T) {
	pcm := make([]byte, 640)
	calls := 0
	if !decodePCM(audioFixture("vqe_0", 16000, 1, pcm), func(b []byte) bool { calls++; return len(b) == 640 }) || calls != 1 {
		t.Fatal("valid processed capture rejected")
	}
	for _, p := range [][]byte{audioFixture("vqe_0", 48000, 1, pcm), audioFixture("vqe_0", 16000, 2, pcm), audioFixture("vqe_0", 16000, 1, pcm[:3]), {10, 255}} {
		if decodePCM(p, func([]byte) bool { return true }) {
			t.Fatal("malformed capture accepted")
		}
	}
	calls = 0
	if !decodePCM(audioFixture("other-private-name", 16000, 1, pcm), func([]byte) bool { calls++; return true }) || calls != 0 {
		t.Fatal("unselected channel reached detector")
	}
	for _, input := range [][]byte{{0, 0, 0, 0}, {255, 255, 255, 255}, frame(158, []byte{10})[:5]} {
		if _, _, _, err := readFrame(bytes.NewReader(input)); err == nil {
			t.Fatal("truncated/oversized frame accepted")
		}
	}
}

func TestPrivateStopAndOverlayGuards(t *testing.T) {
	root := privateRoot(t)
	model := filepath.Join(t.TempDir(), "model")
	if os.Mkdir(model, 0700) != nil {
		t.Fatal("fixture")
	}
	for n, v := range map[string]string{"flags.txt": "freq-filter-enabled true\nuse-tts-blocker true\n", "words.txt": "fixture", "acoustic_model.nnet": "fixture"} {
		if os.WriteFile(filepath.Join(model, n), []byte(v), 0600) != nil {
			t.Fatal("fixture")
		}
	}
	dir, ok := makeOverlay(root, model)
	if !ok || !privateDirectory(dir) {
		t.Fatal("private model overlay failed")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "flags.txt"))
	if strings.Contains(string(b), " true") {
		t.Fatal("legacy flag changes missing")
	}
	stock, _ := os.ReadFile(filepath.Join(model, "flags.txt"))
	if !strings.Contains(string(stock), " true") {
		t.Fatal("stock model changed")
	}
	if !removeOverlay(root, dir, model) {
		t.Fatal("overlay cleanup failed")
	}
	if removeOverlay(root, model, model) {
		t.Fatal("cleanup escaped own overlay")
	}
	if stopState(root) != 0 {
		t.Fatal("absent stop failed")
	}
	stop := filepath.Join(root, "stop")
	if os.WriteFile(stop, nil, 0600) != nil || stopState(root) != 1 {
		t.Fatal("safe stop failed")
	}
	if os.Chmod(stop, 0644) != nil || stopState(root) != -1 {
		t.Fatal("public stop accepted")
	}
	if os.Remove(stop) != nil || os.Symlink(filepath.Join(model, "words.txt"), stop) != nil || stopState(root) != -1 {
		t.Fatal("symlink stop accepted")
	}
	if os.Chmod(root, 0755) != nil || stopState(root) != -1 {
		t.Fatal("public root accepted")
	}
}

type fakeDetector struct{ feeds, closed atomic.Int32 }

func (d *fakeDetector) feed(pcm []int16) (bool, bool) { d.feeds.Add(1); return true, len(pcm) == 320 }
func (d *fakeDetector) close() bool                   { d.closed.Add(1); return true }

func TestBoundedLoopOnlyCountsAndCleansUp(t *testing.T) {
	root := privateRoot(t)
	d := &fakeDetector{}
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var out bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- execute(ctx, root, &out, dependencies{open: func(string) (detector, bool) { return d, true }, dial: func(context.Context) (net.Conn, error) { return client, nil }})
	}()
	request := make([]byte, len(subscription()))
	if _, err := io.ReadFull(server, request); err != nil || !bytes.Equal(request, subscription()) {
		t.Fatal("unexpected IPC write")
	}
	pcm := make([]byte, 640)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], 1234)
	}
	if _, err := server.Write(frame(158, audioFixture("vqe_0", 16000, 1, pcm))); err != nil {
		t.Fatal("fixture send")
	}
	deadline := time.Now().Add(time.Second)
	for d.feeds.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if os.WriteFile(filepath.Join(root, "stop"), nil, 0600) != nil {
		t.Fatal("fixture stop")
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal("probe stop failed")
		}
	case <-ctx.Done():
		t.Fatal("probe did not stop")
	}
	if d.feeds.Load() != 1 || d.closed.Load() != 1 {
		t.Fatal("native lifecycle counts changed")
	}
	text := out.String()
	for _, want := range []string{"NATIVE_SPOTTER_READY", "CAPTURE_ACTIVE", "KEYWORD_MATCH elapsed_ms=", "matches=1 reason=STOP_FILE", "SPOTTER_CLOSED"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing fixed category")
		}
	}
	if strings.Contains(text, "vqe_0") || strings.Contains(text, "1234") || strings.Contains(text, root) {
		t.Fatal("raw data/path reached output")
	}
	if _, err := os.Stat(filepath.Join(root, "lock")); !os.IsNotExist(err) {
		t.Fatal("owned lock remained")
	}
}

func TestUnsafeStopNeverOpensNativeOrCapture(t *testing.T) {
	root := privateRoot(t)
	if os.Symlink("/not-used", filepath.Join(root, "stop")) != nil {
		t.Fatal("fixture")
	}
	called := false
	var out bytes.Buffer
	code := execute(t.Context(), root, &out, dependencies{open: func(string) (detector, bool) { called = true; return nil, false }, dial: func(context.Context) (net.Conn, error) { called = true; return nil, nil }})
	if code != 2 || called || !strings.Contains(out.String(), "PRIVATE_PATH_REJECTED") {
		t.Fatal("unsafe metadata crossed native/capture boundary")
	}
}

func TestInvalidOptionsAreFixedAndDoNotEcho(t *testing.T) {
	for _, args := range [][]string{{"--unknown=private-marker"}, {"--seconds=601"}, {"--run", "--check"}, {"--seconds=0"}} {
		var out bytes.Buffer
		if cli(args, &out) != 2 || out.String() != "INVALID_OPTIONS\n" {
			t.Fatal("invalid options leaked or reached runtime")
		}
	}
}
