//go:build latencyprobe

package voice

// This test-only entry point sends one public PCM fixture over a fresh website
// Voice peer. It never enters station capture, native IPC, KWS or playback code.
import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hraban/opus"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"golang.org/x/sys/unix"
)

const syntheticRoot = "/tmp/alisa-synthetic-latency"
const syntheticFixtureLimit = 1 << 20
const syntheticTotalLimit = 60 * time.Second
const syntheticRuntimeLimit = 58 * time.Second
const syntheticFrameSamples = 320 // PCM16 mono 16 kHz, 20 ms.

var syntheticLive = flag.Bool("synthetic-live", false, "run one explicitly authorized synthetic Voice case on the speaker")
var syntheticCase = flag.String("synthetic-case", "default_selection", "default_selection or forced_thinking")

type syntheticReport struct {
	Case              string            `json:"case"`
	StartUTC          int64             `json:"start_utc_ms"`
	Elapsed           map[string]int64  `json:"elapsed_ms"`
	Errors            map[string]uint64 `json:"error_counts"`
	FramesSent        uint64            `json:"opus_frames_sent"`
	RemotePackets     uint64            `json:"remote_rtp_packets"`
	DecodedFrames     uint64            `json:"remote_decoded_frames"`
	Ready             bool              `json:"media_ready"`
	UtteranceComplete bool              `json:"utterance_complete"`
	RemoteNonzero     bool              `json:"remote_nonzero_after_utterance"`
	PeerClosed        bool              `json:"peer_closed"`
	Ended             string            `json:"termination"`
}

var syntheticErrorNames = [...]string{"config", "fixture", "credential", "peer", "offer", "session", "signaling", "accept", "media_timeout", "send_encode", "send_write", "remote_read", "remote_decode", "control_decode", "remote_session", "deadline", "cleanup"}

type syntheticLedger struct {
	mu     sync.Mutex
	start  time.Time
	report syntheticReport
}

func newSyntheticLedger(caseName string, start time.Time) *syntheticLedger {
	if caseName != "default_selection" && caseName != "forced_thinking" {
		caseName = "rejected"
	}
	return &syntheticLedger{start: start, report: syntheticReport{Case: caseName, StartUTC: start.UnixMilli(), Elapsed: map[string]int64{}, Errors: map[string]uint64{}, Ended: "incomplete"}}
}
func (l *syntheticLedger) stage(name string) {
	switch name {
	case "offer_ready", "answer_ready", "media_ready", "utterance_start", "utterance_end", "first_remote_nonzero_after_utterance", "ended":
	default:
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.report.Elapsed[name]; !exists {
		l.report.Elapsed[name] = max(0, time.Since(l.start).Milliseconds())
	}
	if name == "media_ready" {
		l.report.Ready = true
	}
	if name == "utterance_end" {
		l.report.UtteranceComplete = true
	}
}
func (l *syntheticLedger) failure(name string) {
	for _, allowed := range syntheticErrorNames {
		if name == allowed {
			l.mu.Lock()
			l.report.Errors[name]++
			l.mu.Unlock()
			return
		}
	}
}
func (l *syntheticLedger) remote(pcm []int16, frames int) {
	nonzero := false
	for _, sample := range pcm {
		if sample != 0 {
			nonzero = true
			break
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.report.DecodedFrames += uint64(frames)
	if nonzero && l.report.UtteranceComplete && !l.report.RemoteNonzero {
		l.report.RemoteNonzero = true
		l.report.Elapsed["first_remote_nonzero_after_utterance"] = max(0, time.Since(l.start).Milliseconds())
	}
}
func (l *syntheticLedger) snapshot(ended string) syntheticReport {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.report
	r.Ended = ended
	r.Elapsed = make(map[string]int64, len(l.report.Elapsed)+1)
	for k, v := range l.report.Elapsed {
		r.Elapsed[k] = v
	}
	r.Elapsed["ended"] = max(0, time.Since(l.start).Milliseconds())
	r.Errors = make(map[string]uint64, len(l.report.Errors))
	for k, v := range l.report.Errors {
		r.Errors[k] = v
	}
	return r
}

func syntheticSettings(caseName string) (sessionSettings, error) {
	s := sessionSettings{BackendModel: "disabled", BackendThinkingEffort: "disabled"}
	switch caseName {
	case "default_selection":
	case "forced_thinking":
		s.BackendModel = "gpt-6-thinking"
	default:
		return sessionSettings{}, errors.New("case")
	}
	return s, nil
}

// Open by directory FD, refusing symlinks, links, public/private mode changes,
// unexpected owners, nonregular streams and oversized data before reading.
func syntheticRead(parent, leaf string, limit int64, private bool) ([]byte, error) {
	if filepath.Base(leaf) != leaf || leaf == "." || leaf == ".." {
		return nil, errors.New("file")
	}
	dir, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("file")
	}
	defer unix.Close(dir)
	var ds unix.Stat_t
	if unix.Fstat(dir, &ds) != nil || !privateTimingDirectory(&ds) {
		return nil, errors.New("file")
	}
	fd, err := unix.Openat(dir, leaf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("file")
	}
	f := os.NewFile(uintptr(fd), "synthetic-input")
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Size < 1 || st.Size > limit {
		return nil, errors.New("file")
	}
	if private && st.Mode&07777 != 0600 {
		return nil, errors.New("file")
	}
	if !private && st.Mode&07777 != 0600 && st.Mode&07777 != 0644 {
		return nil, errors.New("file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		clear(data)
		return nil, errors.New("file")
	}
	return data, nil
}

func syntheticContext(data []byte, settings sessionSettings, now time.Time) (sessionSettings, error) {
	var preview voiceContextProbe
	if json.Unmarshal(data, &preview) != nil {
		return sessionSettings{}, errContextProbeRejected
	}
	settings.ProjectID = preview.ProjectID
	c, err := parseContextProbe(data, settings, now)
	if err != nil {
		return sessionSettings{}, err
	}
	c.claimed = true // Logical claim only; no production context file is read/renamed.
	settings.contextProbe = c
	return settings, nil
}

func syntheticWAV(data []byte) ([]int16, error) {
	bad := func() ([]int16, error) { return nil, errors.New("fixture") }
	if len(data) < 44 || len(data) > syntheticFixtureLimit || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return bad()
	}
	formatOK := false
	var audio []byte
	for offset := 12; offset < len(data); {
		if offset+8 > len(data) {
			return bad()
		}
		size := uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		begin := uint64(offset + 8)
		end := begin + size
		if end > uint64(len(data)) {
			return bad()
		}
		chunk := data[begin:end]
		switch string(data[offset : offset+4]) {
		case "fmt ":
			if formatOK || len(chunk) < 16 || len(chunk) > 40 || binary.LittleEndian.Uint16(chunk) != 1 || binary.LittleEndian.Uint16(chunk[2:]) != 1 || binary.LittleEndian.Uint32(chunk[4:]) != 16000 || binary.LittleEndian.Uint32(chunk[8:]) != 32000 || binary.LittleEndian.Uint16(chunk[12:]) != 2 || binary.LittleEndian.Uint16(chunk[14:]) != 16 {
				return bad()
			}
			formatOK = true
		case "data":
			if audio != nil {
				return bad()
			}
			audio = chunk
		}
		offset = int(end + (size & 1))
		if offset > len(data) {
			return bad()
		}
	}
	if !formatOK || len(audio) == 0 || len(audio)%2 != 0 || len(audio) > 16000*2*20 {
		return bad()
	}
	pcm := make([]int16, len(audio)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(audio[i*2:]))
	}
	return pcm, nil
}

// The first 25 frames are silence; after the fixture only silence is sent.
// At most one 320-sample frame is handed to the encoder at a time.
func syntheticFrame(pcm []int16, index int, frame []int16) (start, end bool) {
	clear(frame)
	const leadFrames = 25
	fixtureFrames := (len(pcm) + syntheticFrameSamples - 1) / syntheticFrameSamples
	start = index == leadFrames
	end = index == leadFrames+fixtureFrames
	if index >= leadFrames && index < leadFrames+fixtureFrames {
		at := (index - leadFrames) * syntheticFrameSamples
		copy(frame, pcm[at:min(at+syntheticFrameSamples, len(pcm))])
	}
	return start, end
}

func syntheticPace(ctx context.Context, pcm []int16, encodeWrite func([]int16) string, ledger *syntheticLedger) string {
	base := time.Now()
	timer := time.NewTimer(0)
	defer timer.Stop()
	frame := make([]int16, syntheticFrameSamples)
	defer clear(frame)
	for index := 0; ; index++ {
		wait := time.Until(base.Add(time.Duration(index) * 20 * time.Millisecond))
		if wait > 0 {
			timer.Reset(wait)
			select {
			case <-ctx.Done():
				return "canceled"
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return "canceled"
		}
		start, end := syntheticFrame(pcm, index, frame)
		if start {
			ledger.stage("utterance_start")
		}
		if end {
			ledger.stage("utterance_end")
		}
		if failure := encodeWrite(frame); failure != "" {
			ledger.failure(failure)
			return "send_error"
		}
		ledger.mu.Lock()
		ledger.report.FramesSent++
		ledger.mu.Unlock()
	}
}

func syntheticDrain(ctx context.Context, track *webrtc.TrackRemote, ledger *syntheticLedger) {
	codec := track.Codec()
	if codec.MimeType != webrtc.MimeTypeOpus || codec.ClockRate != 48000 || codec.Channels != 2 {
		ledger.failure("remote_decode")
		return
	}
	decoder, err := opus.NewDecoder(48000, 2)
	if err != nil {
		ledger.failure("remote_decode")
		return
	}
	pcm := make([]int16, 5760*2)
	defer clear(pcm)
	stop := context.AfterFunc(ctx, func() { _ = track.SetReadDeadline(time.Now()) })
	defer stop()
	for ctx.Err() == nil {
		packet, _, err := track.ReadRTP()
		if err != nil {
			if ctx.Err() == nil && !closedTransport(err) {
				ledger.failure("remote_read")
			}
			return
		}
		ledger.mu.Lock()
		ledger.report.RemotePackets++
		ledger.mu.Unlock()
		if len(packet.Payload) == 0 {
			continue
		}
		n, err := decoder.Decode(packet.Payload, pcm)
		if err != nil {
			ledger.failure("remote_decode")
			continue
		}
		ledger.remote(pcm[:n*2], n)
		clear(pcm)
	}
}

func syntheticExecute(ctx context.Context, settings sessionSettings, auth credential, pcm []int16, ledger *syntheticLedger) (ended string) {
	report := log.New(io.Discard, "", 0)
	p, err := newPeer(ctx, webrtc.SettingEngine{}, []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302", "stun:stun4.l.google.com:19302"}}}, report)
	if err != nil {
		ledger.failure("peer")
		return "setup_error"
	}
	var workers sync.WaitGroup
	var packet []byte
	defer func() {
		p.cancel(context.Canceled)
		done := make(chan bool, 1)
		go func() { clean := p.close() == nil; workers.Wait(); done <- clean }()
		select {
		case clean := <-done:
			clear(packet) // Sender has exited; never race its encoder/WriteSample.
			ledger.mu.Lock()
			ledger.report.PeerClosed = clean
			ledger.mu.Unlock()
			if !clean {
				ledger.failure("cleanup")
			}
		case <-time.After(1500 * time.Millisecond):
			ledger.failure("cleanup")
		}
	}()
	// Keep the existing event/termination semantics, but no metadata sink or raw
	// event logging. Only the decoder's known failure classes are counted.
	p.dc.OnMessage(func(message webrtc.DataChannelMessage) {
		if !message.IsString {
			ledger.failure("control_decode")
			p.cancel(context.Canceled)
			return
		}
		ev, err := decodeEvent(message.Data)
		if err != nil {
			ledger.failure("control_decode")
			p.cancel(context.Canceled)
		} else if ev.err != nil {
			ledger.failure("remote_session")
			p.cancel(context.Canceled)
		} else if ev.ended {
			p.cancel(errProtocolHangup)
		}
	})
	setup, finishSetup := context.WithTimeout(ctx, 8*time.Second)
	defer finishSetup()
	offer, err := p.offer(setup)
	if err != nil {
		ledger.failure("offer")
		return "setup_error"
	}
	ledger.stage("offer_ready")
	session, err := encodeSession(time.Now(), settings)
	if err != nil {
		ledger.failure("session")
		return "setup_error"
	}
	defer clear(session)
	client := signalingClient()
	defer client.CloseIdleConnections()
	answer, err := exchangeOffer(setup, client, auth, offer, session)
	if err != nil {
		ledger.failure("signaling")
		return "setup_error"
	}
	ledger.stage("answer_ready")
	if p.accept(answer) != nil {
		ledger.failure("accept")
		return "setup_error"
	}
	select {
	case <-p.ready:
	case <-setup.Done():
		ledger.failure("media_timeout")
		return "setup_error"
	case <-p.ctx.Done():
		return "remote_closed"
	}
	if p.ctx.Err() != nil {
		return "remote_closed"
	}
	ledger.stage("media_ready")
	finishSetup()
	encoder, err := opus.NewEncoder(16000, 1, opus.AppVoIP)
	if err != nil || encoder.SetBitrate(24000) != nil {
		ledger.failure("send_encode")
		return "send_error"
	}
	packet = make([]byte, 4000)
	sent := make(chan string, 1)
	workers.Go(func() {
		sent <- syntheticPace(p.ctx, pcm, func(frame []int16) string {
			n, err := encoder.Encode(frame, packet)
			if err != nil {
				return "send_encode"
			}
			if p.mic.WriteSample(media.Sample{Data: packet[:n], Duration: 20 * time.Millisecond}) != nil {
				return "send_write"
			}
			return ""
		}, ledger)
	})
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				ledger.failure("deadline")
				return "deadline"
			}
			return "signal"
		case <-p.ctx.Done():
			if ctx.Err() != nil {
				continue
			}
			return "remote_closed"
		case result := <-sent:
			if ctx.Err() != nil {
				continue
			}
			if p.ctx.Err() != nil {
				return "remote_closed"
			}
			return result
		case track := <-p.tracks:
			workers.Go(func() { syntheticDrain(p.ctx, track, ledger) })
		}
	}
}

func syntheticLiveRun(output io.Writer, ledger *syntheticLedger) string {
	settings, err := syntheticSettings(*syntheticCase)
	if err != nil || os.Geteuid() != 0 || toolMetadataEnabled(setupTimingDirectory) {
		ledger.failure("config")
		return "config_rejected"
	}
	// This helper never imports/exports credentials and accepts no credential path.
	contextData, err := syntheticRead(syntheticRoot, contextProbeLeaf, maxContextProbeBytes, true)
	if err != nil {
		ledger.failure("config")
		return "config_rejected"
	}
	defer clear(contextData)
	settings, err = syntheticContext(contextData, settings, time.Now())
	if err != nil {
		ledger.failure("config")
		return "config_rejected"
	}
	fixture, err := syntheticRead(syntheticRoot, "fixture.wav", syntheticFixtureLimit, false)
	if err != nil {
		ledger.failure("fixture")
		return "config_rejected"
	}
	defer clear(fixture)
	pcm, err := syntheticWAV(fixture)
	if err != nil {
		ledger.failure("fixture")
		return "config_rejected"
	}
	defer clear(pcm)
	auth, err := loadCredential(credentialPath())
	if err != nil {
		ledger.failure("credential")
		return "config_rejected"
	}
	parent, stopSignal := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignal()
	ctx, cancel := context.WithDeadline(parent, ledger.start.Add(syntheticRuntimeLimit))
	defer cancel()
	return syntheticExecute(ctx, settings, auth, pcm, ledger)
}

// The explicit live entry point exits directly so the Go test harness cannot
// print names, flags, failures or extra event text around the one safe JSON.
func TestSyntheticLatencyProbe(t *testing.T) {
	if !*syntheticLive {
		t.Skip("explicit on-speaker synthetic case only")
	}
	ledger := newSyntheticLedger(*syntheticCase, time.Now())
	fd, err := unix.Dup(1)
	if err != nil {
		os.Exit(2)
	}
	out := os.NewFile(uintptr(fd), "safe-report")
	devNull, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		os.Exit(2)
	}
	if unix.Dup2(int(devNull.Fd()), 1) != nil || unix.Dup2(int(devNull.Fd()), 2) != nil {
		os.Exit(2)
	}
	_ = devNull.Close()
	var once sync.Once
	finish := func(ended string) {
		once.Do(func() {
			_ = json.MarshalWrite(out, ledger.snapshot(ended))
			_, _ = out.Write([]byte("\n"))
			_ = out.Close()
		})
	}
	watchdog := time.AfterFunc(syntheticTotalLimit, func() { ledger.failure("deadline"); finish("hard_deadline"); os.Exit(1) })
	if unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}) != nil {
		ledger.failure("config")
		finish("config_rejected")
		os.Exit(2)
	}
	ended := syntheticLiveRun(out, ledger)
	watchdog.Stop()
	finish(ended)
	if ended == "config_rejected" || ended == "setup_error" || ended == "send_error" {
		os.Exit(1)
	}
	os.Exit(0)
}

func syntheticTestContext(now time.Time) []byte {
	b, _ := json.Marshal(map[string]any{"version": 1, "project_id": "g-p-0123456789abcdef0123456789abcdef", "conversation_id": "10000000-0000-4000-8000-000000000001", "parent_message_id": "20000000-0000-4000-8000-000000000002", "expires_at_unix": now.Unix() + 180})
	return b
}
func syntheticTestWAV(pcm []int16) []byte {
	b := make([]byte, 44+len(pcm)*2)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(len(pcm)*2))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[44+i*2:], uint16(v))
	}
	return b
}

func TestSyntheticProbeCasePayloadAndFreshIDs(t *testing.T) {
	now := time.Now()
	var previous string
	for _, name := range []string{"default_selection", "forced_thinking", "default_selection"} {
		s, err := syntheticSettings(name)
		if err != nil {
			t.Fatal("settings")
		}
		s, err = syntheticContext(syntheticTestContext(now), s, now)
		if err != nil {
			t.Fatal("context")
		}
		b, err := encodeSession(now, s)
		if err != nil {
			t.Fatal("session")
		}
		var p map[string]any
		if json.Unmarshal(b, &p) != nil {
			t.Fatal("payload")
		}
		if p["backend_reasoning_effort"] != "instant" || p["backend_thinking_effort"] != nil {
			t.Fatal("effort changed")
		}
		if name == "default_selection" && p["backend_model"] != nil {
			t.Fatal("model omission")
		}
		if name == "forced_thinking" && p["backend_model"] != "gpt-6-thinking" {
			t.Fatal("known model")
		}
		if p["conversation_id"] == nil || p["parent_message_id"] == nil || p["conversation_mode"].(map[string]any)["kind"] != "gizmo_interaction" {
			t.Fatal("context lost")
		}
		id := p["voice_session_id"].(string)
		if id == previous || id == "" || p["voice_status_request_id"] != id {
			t.Fatal("not fresh")
		}
		previous = id
	}
	if _, err := syntheticSettings("SYNTHETIC_PRIVATE_INVALID_CASE"); err == nil {
		t.Fatal("unsupported case")
	}
}

func TestSyntheticProbeContextAndWAVGuards(t *testing.T) {
	now := time.Now()
	s, _ := syntheticSettings("default_selection")
	for _, b := range [][]byte{nil, []byte(`{}`), append(syntheticTestContext(now), []byte(`{}`)...), bytes.Replace(syntheticTestContext(now), []byte(`"version":1`), []byte(`"version":2`), 1), bytes.Replace(syntheticTestContext(now), []byte(`"project_id":`), []byte(`"unknown":`), 1)} {
		if _, err := syntheticContext(b, s, now); err == nil {
			t.Fatal("bad context accepted")
		}
	}
	valid := syntheticTestWAV([]int16{0, 1, -1, 32767, -32768})
	pcm, err := syntheticWAV(valid)
	if err != nil || len(pcm) != 5 || pcm[4] != -32768 {
		t.Fatal("valid fixture")
	}
	for _, mutate := range []func([]byte) []byte{
		func(b []byte) []byte { return b[:20] }, func(b []byte) []byte { b[0] = 'X'; return b }, func(b []byte) []byte { b[20] = 3; return b }, func(b []byte) []byte { b[22] = 2; return b }, func(b []byte) []byte { b[24] = 1; return b }, func(b []byte) []byte { b[34] = 8; return b }, func(b []byte) []byte { b[40] = 255; return b }, func(b []byte) []byte { return append(b, 0) },
	} {
		if _, err := syntheticWAV(mutate(bytes.Clone(valid))); err == nil {
			t.Fatal("bad fixture accepted")
		}
	}
}

func TestSyntheticProbePrivateFiles(t *testing.T) {
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("directory")
	}
	name := filepath.Join(dir, "context-probe.json")
	if os.WriteFile(name, []byte("synthetic"), 0600) != nil {
		t.Fatal("file")
	}
	if _, err := syntheticRead(dir, "context-probe.json", 1024, true); err != nil {
		var directory, file unix.Stat_t
		_ = unix.Stat(dir, &directory)
		_ = unix.Stat(name, &file)
		t.Fatalf("synthetic guards: directory=%t regular=%t owner=%t private=%t singlelink=%t size=%t", privateTimingDirectory(&directory), file.Mode&unix.S_IFMT == unix.S_IFREG, file.Uid == uint32(os.Geteuid()), file.Mode&07777 == 0600, file.Nlink == 1, file.Size > 0 && file.Size <= 1024)
	}
	if os.Chmod(name, 0644) != nil {
		t.Fatal("mode")
	}
	if _, err := syntheticRead(dir, "context-probe.json", 1024, true); err == nil {
		t.Fatal("public accepted")
	}
	if _, err := syntheticRead(dir, "../context-probe.json", 1024, true); err == nil {
		t.Fatal("escape accepted")
	}
	link := filepath.Join(dir, "link")
	if os.Symlink(name, link) != nil {
		t.Fatal("symlink")
	}
	if _, err := syntheticRead(dir, "link", 1024, false); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := syntheticRead(dir, "context-probe.json", 2, false); err == nil {
		t.Fatal("oversize accepted")
	}
	if os.Chmod(dir, 0755) != nil {
		t.Fatal("mode")
	}
	if _, err := syntheticRead(dir, "context-probe.json", 1024, false); err == nil {
		t.Fatal("public parent accepted")
	}
}

func TestSyntheticProbePacingAndDeadline(t *testing.T) {
	pcm := make([]int16, syntheticFrameSamples+1)
	for i := range pcm {
		pcm[i] = 1234
	}
	frame := make([]int16, syntheticFrameSamples)
	for i := 0; i < 25; i++ {
		start, end := syntheticFrame(pcm, i, frame)
		if start || end {
			t.Fatal("lead")
		}
		for _, v := range frame {
			if v != 0 {
				t.Fatal("not silence")
			}
		}
	}
	if start, _ := syntheticFrame(pcm, 25, frame); !start || frame[0] != 1234 {
		t.Fatal("start")
	}
	syntheticFrame(pcm, 26, frame)
	if frame[0] != 1234 || frame[1] != 0 {
		t.Fatal("padding")
	}
	if _, end := syntheticFrame(pcm, 27, frame); !end || frame[0] != 0 {
		t.Fatal("end")
	}
	ledger := newSyntheticLedger("default_selection", time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Millisecond)
	defer cancel()
	if ended := syntheticPace(ctx, pcm, func([]int16) string { return "" }, ledger); ended != "canceled" {
		t.Fatal("deadline")
	}
	if count := ledger.snapshot("deadline").FramesSent; count < 2 || count > 4 {
		t.Fatal("unpaced or unbounded")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	calls := 0
	syntheticPace(ctx2, pcm, func([]int16) string { calls++; return "" }, ledger)
	if calls != 0 {
		t.Fatal("send after cancellation")
	}
}

func TestSyntheticProbePrivacyAndConcurrentLedger(t *testing.T) {
	l := newSyntheticLedger("SYNTHETIC_PRIVATE_INVALID_CASE", time.Now())
	l.stage("SYNTHETIC_PRIVATE_UNKNOWN_STAGE")
	l.failure("SYNTHETIC_PRIVATE_UNKNOWN_ERROR")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				l.stage("utterance_end")
				l.remote([]int16{1}, 1)
				l.failure("remote_decode")
				_ = l.snapshot("deadline")
			}
		})
	}
	wg.Wait()
	b, err := json.Marshal(l.snapshot("deadline"))
	if err != nil || len(b) > 2048 || strings.Contains(string(b), "SYNTHETIC_PRIVATE") || strings.Contains(string(b), "conversation") || strings.Contains(string(b), "token") {
		t.Fatal("unsafe report")
	}
	if !bytes.Contains(b, []byte(`"remote_nonzero_after_utterance":true`)) {
		t.Fatal("energy marker")
	}
}
