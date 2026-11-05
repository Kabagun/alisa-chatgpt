package voice

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

const setupTimingDirectory = "/tmp/alisa-chatgpt-supervisor"
const setupTimingLeaf = "last-setup.json"
const callEndedLeaf = "last-call-ended.json"
const setupStageCount = 7

type setupSnapshot struct {
	elapsed [setupStageCount]int64
	seen    uint8
	ready   bool
}

type setupTrace struct {
	setupSnapshot
	finished bool
}

func (t *setupTrace) observe(phase feedbackPhase, elapsed int64) (setupSnapshot, bool) {
	if t.finished {
		return setupSnapshot{}, false
	}
	if phase >= feedbackTargetMatched && phase <= feedbackMediaReady {
		i := int(phase - feedbackTargetMatched)
		if t.seen&(1<<i) == 0 {
			t.elapsed[i] = max(0, elapsed)
			t.seen |= 1 << i
		}
		if phase != feedbackMediaReady {
			return setupSnapshot{}, false
		}
		t.ready = true
	} else if phase != feedbackStopped {
		return setupSnapshot{}, false
	}
	t.finished = true
	return t.setupSnapshot, true
}

func (s setupSnapshot) json() []byte {
	wire := struct {
		Status     string           `json:"status"`
		ErrorStage string           `json:"error_stage,omitempty"`
		Elapsed    map[string]int64 `json:"elapsed_ms"`
	}{Status: "ready", Elapsed: make(map[string]int64, setupStageCount)}
	for i := 0; i < setupStageCount; i++ {
		name := (feedbackTargetMatched + feedbackPhase(i)).name()
		if s.seen&(1<<i) != 0 {
			wire.Elapsed[name] = s.elapsed[i]
		} else if !s.ready && wire.ErrorStage == "" {
			wire.ErrorStage = name
		}
	}
	if !s.ready {
		wire.Status = "incomplete"
	}
	data, _ := json.Marshal(wire)
	return append(data, '\n')
}

// One lazy worker and one pending snapshot: filesystem trouble never blocks
// Voice or creates unbounded worker/file growth. Later pending setups replace old.
type snapshotSink[T any] struct {
	once    sync.Once
	pending chan T
	write   func(T) bool
	done    chan struct{}
}

func newSnapshotSink[T any](write func(T) bool) *snapshotSink[T] {
	return &snapshotSink[T]{pending: make(chan T, 1), write: write, done: make(chan struct{})}
}

func newSetupTimingSink(write func(setupSnapshot) bool) *snapshotSink[setupSnapshot] {
	return newSnapshotSink(write)
}

var lastSetupSink = newSetupTimingSink(func(s setupSnapshot) bool {
	return writeSetupTiming(setupTimingDirectory, s)
})

func (s *snapshotSink[T]) offer(snapshot T) {
	s.once.Do(func() {
		go func() {
			defer close(s.done)
			for snapshot := range s.pending {
				_ = s.write(snapshot) // Deliberately no raw errors or additional logging.
			}
		}()
	})
	select {
	case s.pending <- snapshot:
		return
	default:
	}
	select {
	case <-s.pending:
	default:
	}
	select {
	case s.pending <- snapshot:
	default:
	}
}

func privateTimingDirectory(st *unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Mode&07777 == 0700 && st.Uid == uint32(os.Geteuid())
}

func privateTimingFile(st *unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Mode&07777 == 0600 && st.Uid == uint32(os.Geteuid()) && st.Nlink == 1 && st.Size <= 2048
}

func safeTimingLeaf(dir int, leaf string) bool {
	var st unix.Stat_t
	err := unix.Fstatat(dir, leaf, &st, unix.AT_SYMLINK_NOFOLLOW)
	if err == unix.ENOENT {
		return true
	}
	return err == nil && privateTimingFile(&st)
}

func writeSetupTiming(parent string, snapshot setupSnapshot) bool {
	return writePrivateSnapshot(parent, setupTimingLeaf, snapshot.json())
}

type callEndReason uint8

const callEndedNativeRed callEndReason = 1

var lastCallEndedSink = newSnapshotSink(func(reason callEndReason) bool {
	if reason != callEndedNativeRed {
		return false
	}
	return writePrivateSnapshot(setupTimingDirectory, callEndedLeaf, []byte("{\"reason\":\"native_red\"}\n"))
})

func writePrivateSnapshot(parent, leaf string, data []byte) bool {
	if (leaf != setupTimingLeaf && leaf != callEndedLeaf && leaf != callStateLeaf && leaf != audioHealthLeaf && leaf != toolMetadataLeaf) || len(data) == 0 || len(data) > 2048 {
		return false
	}
	// Parent must already be the supervisor's private real directory. All leaf
	// operations are anchored to its fd; never create/chmod the parent or old leaf.
	dir, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(dir)
	var original unix.Stat_t
	if unix.Fstat(dir, &original) != nil || !privateTimingDirectory(&original) || !safeTimingLeaf(dir, leaf) {
		return false
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return false
	}
	temporary := ".snapshot-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(dir, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return false
	}
	defer unix.Unlinkat(dir, temporary, 0)
	if unix.Fchmod(fd, 0600) != nil {
		_ = unix.Close(fd)
		return false
	}
	var staged unix.Stat_t
	if unix.Fstat(fd, &staged) != nil || !privateTimingFile(&staged) {
		_ = unix.Close(fd)
		return false
	}
	f := os.NewFile(uintptr(fd), "setup-timing")
	n, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil || n != len(data) {
		return false
	}
	var current, named, temporaryFile unix.Stat_t
	if unix.Fstat(dir, &current) != nil || !privateTimingDirectory(&current) ||
		unix.Fstatat(unix.AT_FDCWD, parent, &named, unix.AT_SYMLINK_NOFOLLOW) != nil ||
		!privateTimingDirectory(&named) || original.Dev != named.Dev || original.Ino != named.Ino || !safeTimingLeaf(dir, leaf) ||
		unix.Fstatat(dir, temporary, &temporaryFile, unix.AT_SYMLINK_NOFOLLOW) != nil || !privateTimingFile(&temporaryFile) ||
		temporaryFile.Dev != staged.Dev || temporaryFile.Ino != staged.Ino {
		return false
	}
	return unix.Renameat(dir, temporary, dir, leaf) == nil
}
