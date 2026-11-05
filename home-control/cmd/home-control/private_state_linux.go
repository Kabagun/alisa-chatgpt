//go:build linux

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const privateStateName = "private"
const privateStageName = ".home-control-private-migration"
const privateLockName = ".home-control-private.lock"
const privateJournalName = ".migration-entries"

var errPrivateMigration = errors.New("private state migration unavailable")
var legacyStateNames = []string{"oauth-state.json", "accounts"}

// The fixed staging directory is a filesystem journal: each legacy entry can be
// in exactly one of the outer root or staging. Publishing the complete directory
// is one rename. No account or OAuth file contents are read or rewritten.
func preparePrivateState(outer string) (string, error) {
	return preparePrivateStateWithHook(outer, nil)
}

type stateObject struct {
	name string
	info os.FileInfo
	file *os.File
}

func stateMetadata(info os.FileInfo, directory, strict bool) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	want := os.FileMode(0600)
	if directory {
		want = 0700
	}
	return ok && st.Uid == uint32(os.Geteuid()) && info.IsDir() == directory &&
		(directory || info.Mode().IsRegular() && st.Nlink == 1) &&
		info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 &&
		info.Mode().Perm()&0007 == 0 && (!strict || info.Mode().Perm() == want)
}

// Pin all leaves before any permission or namespace change. Opening each child
// relative to its already pinned parent prevents symlink races in ancestors.
func pinStateTree(root *os.Root, name string, directory, strict bool, objects *[]stateObject, depth int) error {
	if depth > 32 || len(*objects) >= 16384 {
		return errPrivateMigration
	}
	info, err := root.Lstat(name)
	if err != nil || !stateMetadata(info, directory, strict) {
		return errPrivateMigration
	}
	flags := os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if directory {
		flags |= syscall.O_DIRECTORY
	}
	f, err := root.OpenFile(name, flags, 0)
	if err != nil {
		return errPrivateMigration
	}
	pinned, err := f.Stat()
	if err != nil || !os.SameFile(info, pinned) || !stateMetadata(pinned, directory, strict) {
		f.Close()
		return errPrivateMigration
	}
	*objects = append(*objects, stateObject{name, pinned, f})
	if !directory {
		return nil
	}
	children, err := f.ReadDir(-1)
	if err != nil {
		return errPrivateMigration
	}
	// OpenRoot pins this same directory, not a potentially replaced path.
	childRoot, err := os.OpenRoot("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
	if err != nil {
		return errPrivateMigration
	}
	defer childRoot.Close()
	for _, child := range children {
		if err := pinStateTree(childRoot, child.Name(), child.IsDir(), strict, objects, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func closeStateObjects(objects []stateObject) {
	for _, object := range objects {
		object.file.Close()
	}
}

func stateExists(root *os.Root, name string) (bool, error) {
	_, err := root.Lstat(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, errPrivateMigration
}

func knownStateChildren(root *os.Root, journal bool) error {
	f, err := root.Open(".")
	if err != nil {
		return errPrivateMigration
	}
	defer f.Close()
	children, err := f.ReadDir(-1)
	if err != nil {
		return errPrivateMigration
	}
	for _, child := range children {
		if journal && child.Name() == privateJournalName {
			continue
		}
		if child.Name() != "oauth-state.json" && child.Name() != "accounts" {
			return errPrivateMigration
		}
		info, e := root.Lstat(child.Name())
		if e != nil || !stateMetadata(info, child.Name() == "accounts", false) {
			return errPrivateMigration
		}
	}
	return nil
}

func expectedStateEntries(root *os.Root) (map[string]bool, error) {
	f, err := root.OpenFile(privateJournalName, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errPrivateMigration
	}
	defer f.Close()
	if info, e := f.Stat(); e != nil || !stateMetadata(info, false, true) {
		return nil, errPrivateMigration
	}
	data, err := io.ReadAll(io.LimitReader(f, 128))
	if err != nil || len(data) >= 128 || !strings.HasPrefix(string(data), "v1\n") {
		return nil, errPrivateMigration
	}
	expected := map[string]bool{}
	for _, name := range strings.Split(strings.TrimPrefix(string(data), "v1\n"), "\n") {
		if name == "" {
			continue
		}
		if name != "oauth-state.json" && name != "accounts" || expected[name] {
			return nil, errPrivateMigration
		}
		expected[name] = true
	}
	return expected, nil
}

func writeStateJournal(root *os.Root, expected map[string]bool) error {
	f, err := root.OpenFile(privateJournalName, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errPrivateMigration
	}
	defer f.Close()
	data := "v1\n"
	for _, name := range legacyStateNames {
		if expected[name] {
			data += name + "\n"
		}
	}
	if _, err = f.WriteString(data); err != nil || f.Sync() != nil {
		return errPrivateMigration
	}
	return syncStateRoot(root)
}

func syncStateRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return errPrivateMigration
	}
	defer f.Close()
	if f.Sync() != nil {
		return errPrivateMigration
	}
	return nil
}

// hook is only used by synthetic tests to interrupt individual committed steps.
func preparePrivateStateWithHook(outer string, hook func(string) error) (string, error) {
	if !filepath.IsAbs(outer) || filepath.Clean(outer) != outer {
		return "", errPrivateMigration
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(outer, current), current) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errPrivateMigration
		}
	}
	info, err := os.Lstat(outer)
	if err != nil || !stateMetadata(info, true, false) {
		return "", errPrivateMigration
	}
	root, err := os.OpenRoot(outer)
	if err != nil {
		return "", errPrivateMigration
	}
	defer root.Close()
	pinned, err := root.Stat(".")
	if err != nil || !os.SameFile(info, pinned) {
		return "", errPrivateMigration
	}
	lock, err := root.OpenFile(privateLockName, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return "", errPrivateMigration
	}
	defer lock.Close()
	if li, e := lock.Stat(); e != nil || !stateMetadata(li, false, true) || syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return "", errPrivateMigration
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	objects := []stateObject{}
	defer func() { closeStateObjects(objects) }()
	private, err := stateExists(root, privateStateName)
	if err != nil {
		return "", err
	}
	stage, err := stateExists(root, privateStageName)
	if err != nil || private && stage {
		return "", errPrivateMigration
	}
	legacy := map[string]bool{}
	for _, name := range legacyStateNames {
		legacy[name], err = stateExists(root, name)
		if err != nil || private && legacy[name] {
			return "", errPrivateMigration
		}
	}
	if private {
		if err = pinStateTree(root, privateStateName, true, true, &objects, 0); err != nil {
			return "", err
		}
		pr, e := root.OpenRoot(privateStateName)
		if e != nil {
			return "", errPrivateMigration
		}
		defer pr.Close()
		if knownStateChildren(pr, true) != nil {
			return "", errPrivateMigration
		}
		journal, e := stateExists(pr, privateJournalName)
		if e != nil {
			return "", errPrivateMigration
		}
		if journal {
			expected, e := expectedStateEntries(pr)
			if e != nil {
				return "", errPrivateMigration
			}
			for _, name := range legacyStateNames {
				exists, e := stateExists(pr, name)
				if e != nil || exists != expected[name] {
					return "", errPrivateMigration
				}
			}
			if pr.Remove(privateJournalName) != nil || syncStateRoot(pr) != nil {
				return "", errPrivateMigration
			}
		}
		return filepath.Join(outer, privateStateName), nil
	}
	if stage {
		if err = pinStateTree(root, privateStageName, true, false, &objects, 0); err != nil {
			return "", err
		}
		stageRoot, e := root.OpenRoot(privateStageName)
		if e != nil {
			return "", errPrivateMigration
		}
		defer stageRoot.Close()
		if knownStateChildren(stageRoot, true) != nil {
			return "", errPrivateMigration
		}
		expected, e := expectedStateEntries(stageRoot)
		if e != nil {
			return "", errPrivateMigration
		}
		for _, name := range legacyStateNames {
			exists, e := stateExists(stageRoot, name)
			if e != nil || exists && legacy[name] || (exists || legacy[name]) != expected[name] {
				return "", errPrivateMigration
			}
		}
	}
	for _, name := range legacyStateNames {
		if legacy[name] {
			if err = pinStateTree(root, name, name == "accounts", false, &objects, 0); err != nil {
				return "", err
			}
		}
	}
	// Preflight is now complete. Every chmod uses an already verified descriptor.
	for _, object := range objects {
		want := os.FileMode(0600)
		if object.info.IsDir() {
			want = 0700
		}
		if object.file.Chmod(want) != nil || object.file.Sync() != nil {
			return "", errPrivateMigration
		}
	}
	if !stage {
		if root.Mkdir(privateStageName, 0700) != nil || syncStateRoot(root) != nil {
			return "", errPrivateMigration
		}
		journalRoot, e := root.OpenRoot(privateStageName)
		if e != nil {
			return "", errPrivateMigration
		}
		e = writeStateJournal(journalRoot, legacy)
		journalRoot.Close()
		if e != nil {
			return "", e
		}
	}
	if hook != nil {
		if err = hook("stage"); err != nil {
			return "", err
		}
	}
	stageRoot, err := root.OpenRoot(privateStageName)
	if err != nil {
		return "", errPrivateMigration
	}
	defer stageRoot.Close()
	for _, name := range legacyStateNames {
		if !legacy[name] {
			continue
		}
		before, e := root.Lstat(name)
		matched := false
		for _, object := range objects {
			if e == nil && object.name == name && os.SameFile(before, object.info) {
				matched = true
			}
		}
		if e != nil || !matched || root.Rename(name, filepath.Join(privateStageName, name)) != nil || syncStateRoot(root) != nil || syncStateRoot(stageRoot) != nil {
			return "", errPrivateMigration
		}
		if hook != nil {
			if err = hook(name); err != nil {
				return "", err
			}
		}
	}
	final := []stateObject{}
	defer func() { closeStateObjects(final) }()
	if pinStateTree(root, privateStageName, true, true, &final, 0) != nil || knownStateChildren(stageRoot, true) != nil {
		return "", errPrivateMigration
	}
	expected, err := expectedStateEntries(stageRoot)
	if err != nil {
		return "", errPrivateMigration
	}
	for _, name := range legacyStateNames {
		exists, e := stateExists(stageRoot, name)
		if e != nil || exists != expected[name] {
			return "", errPrivateMigration
		}
	}
	if exists, e := stateExists(root, privateStateName); e != nil || exists {
		return "", errPrivateMigration
	}
	if root.Rename(privateStageName, privateStateName) != nil || syncStateRoot(root) != nil {
		return "", errPrivateMigration
	}
	if hook != nil {
		if err = hook("published"); err != nil {
			return "", err
		}
	}
	if stageRoot.Remove(privateJournalName) != nil || syncStateRoot(stageRoot) != nil {
		return "", errPrivateMigration
	}
	return filepath.Join(outer, privateStateName), nil
}
