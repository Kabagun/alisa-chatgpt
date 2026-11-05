package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func waveFixture(samples int) []byte {
	f := make([]byte, 16)
	binary.LittleEndian.PutUint16(f[0:2], 1)
	binary.LittleEndian.PutUint16(f[2:4], 1)
	binary.LittleEndian.PutUint32(f[4:8], 16000)
	binary.LittleEndian.PutUint32(f[8:12], 32000)
	binary.LittleEndian.PutUint16(f[12:14], 2)
	binary.LittleEndian.PutUint16(f[14:16], 16)
	pcm := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], 1234)
	}
	b := append([]byte("RIFF\x00\x00\x00\x00WAVE"), chunk("fmt ", f)...)
	b = append(b, chunk("data", pcm)...)
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
	return b
}

func chunk(name string, data []byte) []byte {
	b := append([]byte(name), binary.LittleEndian.AppendUint32(nil, uint32(len(data)))...)
	b = append(b, data...)
	if len(data)%2 != 0 {
		b = append(b, 0)
	}
	return b
}

func privateRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "private")
	if os.Mkdir(root, 0700) != nil {
		t.Fatal("fixture setup")
	}
	return root
}

func stageFixture(t *testing.T, root string, samples int) []byte {
	t.Helper()
	b := waveFixture(samples)
	if os.WriteFile(filepath.Join(root, fixtureName), b, 0600) != nil {
		t.Fatal("fixture setup")
	}
	return b
}

func TestStrictWaveFormatAndChunkBounds(t *testing.T) {
	good := waveFixture(321)
	pcm, ok := parseWave(good)
	if !ok || len(pcm) != 321 || pcm[0] != 1234 || pcm[320] != 1234 {
		t.Fatal("valid PCM rejected")
	}
	clear(pcm)
	for _, mutate := range []func([]byte) []byte{
		func(b []byte) []byte { b[0] = 'X'; return b },
		func(b []byte) []byte { b[4]++; return b },
		func(b []byte) []byte { b[20] = 3; return b },
		func(b []byte) []byte { b[22] = 2; return b },
		func(b []byte) []byte { b[24] = 0; return b },
		func(b []byte) []byte { b[28] = 1; return b },
		func(b []byte) []byte { b[32] = 4; return b },
		func(b []byte) []byte { b[34] = 8; return b },
		func(b []byte) []byte { b[40] = 255; return b },
		func(b []byte) []byte { return b[:len(b)-1] },
		func(b []byte) []byte {
			b = append(b, chunk("data", []byte{0, 0})...)
			binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
			return b
		},
		func(b []byte) []byte {
			b = append(b, chunk("fmt ", b[20:36])...)
			binary.LittleEndian.PutUint32(b[4:8], uint32(len(b)-8))
			return b
		},
	} {
		if _, ok := parseWave(mutate(bytes.Clone(good))); ok {
			t.Fatal("invalid WAV accepted")
		}
	}
	if _, ok := parseWave(waveFixture(0)); ok {
		t.Fatal("empty data accepted")
	}
	if _, ok := parseWave(waveFixture(maximumFixtureSamples + 1)); ok {
		t.Fatal("unbounded duration accepted")
	}
	withMetadata := append(bytes.Clone(good), chunk("JUNK", []byte("synthetic-private-marker"))...)
	binary.LittleEndian.PutUint32(withMetadata[4:8], uint32(len(withMetadata)-8))
	if pcm, ok := parseWave(withMetadata); !ok || len(pcm) != 321 {
		t.Fatal("bounded odd metadata rejected")
	}
	// SAPI may emit the ordinary PCM fmt extension with cbSize=0.
	format18 := append(bytes.Clone(good[:36]), 0, 0)
	binary.LittleEndian.PutUint32(format18[16:20], 18)
	format18 = append(format18, good[36:]...)
	binary.LittleEndian.PutUint32(format18[4:8], uint32(len(format18)-8))
	if pcm, ok := parseWave(format18); !ok || len(pcm) != 321 {
		t.Fatal("plain PCM extension rejected")
	}
}

func TestFixedFixturePrivateMetadataGuards(t *testing.T) {
	root := privateRoot(t)
	stageFixture(t, root, 320)
	path := filepath.Join(root, fixtureName)
	if pcm, ok := loadFixture(root); !ok || len(pcm) != 320 {
		t.Fatal("private fixture rejected")
	}
	if os.Chmod(path, 0644) != nil {
		t.Fatal("fixture setup")
	}
	if _, ok := loadFixture(root); ok {
		t.Fatal("public fixture accepted")
	}
	if os.Chmod(path, 0600) != nil || os.Link(path, filepath.Join(root, "second-link")) != nil {
		t.Fatal("fixture setup")
	}
	if _, ok := loadFixture(root); ok {
		t.Fatal("hard-linked fixture accepted")
	}
	if os.Remove(filepath.Join(root, "second-link")) != nil || os.Rename(path, filepath.Join(root, "actual")) != nil || os.Symlink("actual", path) != nil {
		t.Fatal("fixture setup")
	}
	if _, ok := loadFixture(root); ok {
		t.Fatal("symlink fixture accepted")
	}
	if os.Remove(path) != nil || syscall.Mkfifo(path, 0600) != nil {
		t.Fatal("fixture setup")
	}
	if _, ok := loadFixture(root); ok {
		t.Fatal("FIFO fixture accepted")
	}
	if os.Remove(path) != nil {
		t.Fatal("fixture setup")
	}
	stageFixture(t, root, 320)
	if os.Truncate(path, maximumFixtureBytes+1) != nil {
		t.Fatal("fixture setup")
	}
	if _, ok := loadFixture(root); ok {
		t.Fatal("oversized fixture accepted")
	}
	stageFixture(t, root, 320)
	if os.Chmod(root, 0755) != nil {
		t.Fatal("fixture setup")
	}
	if _, ok := loadFixture(root); ok {
		t.Fatal("public directory accepted")
	}
	if os.Chmod(root, 0700) != nil {
		t.Fatal("fixture setup")
	}
	link := filepath.Join(t.TempDir(), "root-link")
	if os.Symlink(root, link) != nil {
		t.Fatal("fixture setup")
	}
	if _, ok := loadFixture(link); ok {
		t.Fatal("symlink directory accepted")
	}
}

type fakeDetector struct {
	feeds, closed int
	failAt        int
	closeOK       bool
	validFrames   bool
	retained      []int16
}

func (d *fakeDetector) feed(pcm []int16) (bool, bool) {
	d.feeds++
	d.retained = pcm
	if d.feeds == d.failAt {
		return false, false
	}
	nonzero := 0
	for _, sample := range pcm {
		if sample != 0 {
			if sample != 1234 {
				d.validFrames = false
			}
			nonzero++
		}
	}
	expected := 0
	if d.feeds == leadingFrames+1 {
		expected = 320
	} else if d.feeds == leadingFrames+2 {
		expected = 1
	}
	if len(pcm) != 320 || nonzero != expected {
		d.validFrames = false
	}
	return nonzero > 0, true
}
func (d *fakeDetector) close() bool { d.closed++; return d.closeOK }

func TestOfflineFramesPaddingCountsAndCleanup(t *testing.T) {
	root := privateRoot(t)
	original := stageFixture(t, root, 321)
	d := &fakeDetector{closeOK: true, validFrames: true}
	var out bytes.Buffer
	paces := 0
	code := execute(t.Context(), root, &out, dependencies{
		open: func(string) (detector, bool) { return d, true },
		pace: func(context.Context) bool { paces++; return true },
	})
	if code != 0 || d.feeds != 152 || paces != d.feeds || d.closed != 1 || !d.validFrames {
		t.Fatal("offline feed lifecycle failed")
	}
	for _, sample := range d.retained {
		if sample != 0 {
			t.Fatal("frame memory not cleared")
		}
	}
	text := out.String()
	for _, category := range []string{"NATIVE_SPOTTER_READY", "FIXTURE_ACTIVE", "KEYWORD_MATCH elapsed_ms=", "SPOTTER_CLOSED", "pcm_frames=152 matches=1 reason=FIXTURE_COMPLETE"} {
		if !strings.Contains(text, category) {
			t.Fatal("fixed output missing")
		}
	}
	if strings.Contains(text, root) || strings.Contains(text, "1234") || strings.Contains(text, "synthetic-private-marker") {
		t.Fatal("fixture details reached output")
	}
	current, err := os.ReadFile(filepath.Join(root, fixtureName))
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("input fixture changed")
	}
	if _, err := os.Stat(filepath.Join(root, "lock")); !os.IsNotExist(err) {
		t.Fatal("owned lock remained")
	}
}

func TestOfflineInterruptedAndFailureCleanup(t *testing.T) {
	for _, scenario := range []string{"cancel", "feed-failure", "init-failure", "close-failure"} {
		t.Run(scenario, func(t *testing.T) {
			root := privateRoot(t)
			stageFixture(t, root, 321)
			d := &fakeDetector{closeOK: scenario != "close-failure", validFrames: true}
			if scenario == "feed-failure" {
				d.failAt = 2
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			paces := 0
			var out bytes.Buffer
			code := execute(ctx, root, &out, dependencies{
				open: func(string) (detector, bool) { return d, scenario != "init-failure" },
				pace: func(context.Context) bool {
					paces++
					if scenario == "cancel" && paces == 2 {
						cancel()
						return false
					}
					return true
				},
			})
			if scenario == "cancel" {
				if code != 0 || !strings.Contains(out.String(), "reason=SIGNAL") {
					t.Fatal("cancel failed")
				}
			} else if code != 1 {
				t.Fatal("failure not reported")
			}
			wantClosed := 1
			if scenario == "init-failure" {
				wantClosed = 0
			}
			if d.closed != wantClosed {
				t.Fatal("detector lifecycle failed")
			}
			if _, err := os.Stat(filepath.Join(root, "lock")); !os.IsNotExist(err) {
				t.Fatal("failure left lock")
			}
		})
	}
}

func TestUnsafeFixtureNeverLoadsNativeAndOverlayIsPrivate(t *testing.T) {
	root := privateRoot(t)
	stageFixture(t, root, 321)
	if os.Chmod(filepath.Join(root, fixtureName), 0644) != nil {
		t.Fatal("fixture setup")
	}
	called := false
	var out bytes.Buffer
	if code := execute(t.Context(), root, &out, dependencies{open: func(string) (detector, bool) { called = true; return nil, false }}); code != 2 || called {
		t.Fatal("unsafe fixture reached native load")
	}
	model := filepath.Join(t.TempDir(), "model")
	if os.Mkdir(model, 0700) != nil {
		t.Fatal("fixture setup")
	}
	for name, value := range map[string]string{"flags.txt": "freq-filter-enabled true\nuse-tts-blocker true\n", "words.txt": "synthetic", "acoustic_model.nnet": "synthetic"} {
		if os.WriteFile(filepath.Join(model, name), []byte(value), 0600) != nil {
			t.Fatal("fixture setup")
		}
	}
	dir, ok := makeOverlay(root, model)
	if !ok || !privateDirectory(dir) {
		t.Fatal("overlay creation failed")
	}
	flags, _ := os.ReadFile(filepath.Join(dir, "flags.txt"))
	stock, _ := os.ReadFile(filepath.Join(model, "flags.txt"))
	if strings.Contains(string(flags), " true") || !strings.Contains(string(stock), " true") || !removeOverlay(root, dir, model) || removeOverlay(root, model, model) {
		t.Fatal("overlay scope/cleanup failed")
	}
}

func TestOptionsNeverEchoInputAndBudgetIsFixed(t *testing.T) {
	for _, args := range [][]string{{"--file=synthetic-private-marker"}, {"--seconds=600"}, {"--run", "--check"}, {"synthetic-private-marker"}} {
		var out bytes.Buffer
		if cli(args, &out) != 2 || out.String() != "INVALID_OPTIONS\n" {
			t.Fatal("options reached runtime or echoed input")
		}
	}
	if processBudget.Seconds() != 30 || cleanupReserve.Seconds() != 2 || leadingFrames != 50 || trailingFrames != 100 || frameDuration.Milliseconds() != 20 {
		t.Fatal("bounded pacing contract changed")
	}
}
