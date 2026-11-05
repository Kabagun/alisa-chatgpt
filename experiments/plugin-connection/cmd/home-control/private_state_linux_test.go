//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func migrationFixture(t *testing.T) string {
	t.Helper()
	outer := filepath.Join(t.TempDir(), "outer")
	if err := os.Mkdir(outer, 0770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outer, 0770); err != nil {
		t.Fatal(err)
	}
	return outer
}

func legacyFixture(t *testing.T, outer string) map[string]os.FileInfo {
	t.Helper()
	for _, name := range []string{"accounts", "accounts/synthetic-account"} {
		path := filepath.Join(outer, name)
		if err := os.Mkdir(path, 0770); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0770); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string]os.FileInfo{}
	for _, name := range []string{"oauth-state.json", "accounts/synthetic-account/session.json"} {
		path := filepath.Join(outer, name)
		if err := os.WriteFile(path, []byte("synthetic opaque bytes\x00\n"), 0660); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0660); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = info
	}
	return before
}

func assertMigrated(t *testing.T, outer, private string, before map[string]os.FileInfo) {
	t.Helper()
	if private != filepath.Join(outer, privateStateName) {
		t.Fatal("wrong private path")
	}
	outerInfo, err := os.Stat(outer)
	if err != nil || outerInfo.Mode().Perm() != 0770 {
		t.Fatal("outer platform mode changed")
	}
	if err := filepath.Walk(private, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		want := os.FileMode(0600)
		if info.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Fatal("private mode not restored")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for name, original := range before {
		path := filepath.Join(private, name)
		info, err := os.Stat(path)
		if err != nil || !os.SameFile(original, info) {
			t.Fatal("state inode was replaced")
		}
		content, err := os.ReadFile(path)
		if err != nil || string(content) != "synthetic opaque bytes\x00\n" {
			t.Fatal("state contents changed")
		}
	}
	for _, name := range append(append([]string{}, legacyStateNames...), privateStageName) {
		if _, err := os.Lstat(filepath.Join(outer, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("legacy or staging entry remains")
		}
	}
}

func TestPrivateStateFreshMigrationAndIdempotence(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "legacy"}[existing], func(t *testing.T) {
			outer := migrationFixture(t)
			before := map[string]os.FileInfo{}
			if existing {
				before = legacyFixture(t, outer)
			}
			for i := 0; i < 2; i++ {
				private, err := preparePrivateState(outer)
				if err != nil {
					t.Fatal(err)
				}
				assertMigrated(t, outer, private, before)
			}
		})
	}
}

func TestPrivateStateInterruptedMigrationRecovers(t *testing.T) {
	for _, stop := range []string{"stage", "oauth-state.json", "accounts", "published"} {
		t.Run(stop, func(t *testing.T) {
			outer := migrationFixture(t)
			before := legacyFixture(t, outer)
			interrupted := errors.New("synthetic interruption")
			_, err := preparePrivateStateWithHook(outer, func(step string) error {
				if step == stop {
					return interrupted
				}
				return nil
			})
			if !errors.Is(err, interrupted) {
				t.Fatal("expected interruption")
			}
			private, err := preparePrivateState(outer)
			if err != nil {
				t.Fatal(err)
			}
			assertMigrated(t, outer, private, before)
		})
	}
}

func TestPrivateStateRejectsAmbiguityAndUnsafeStateBeforeMutation(t *testing.T) {
	cases := map[string]func(string) error{
		"private_and_legacy": func(p string) error { return os.Mkdir(filepath.Join(p, privateStateName), 0700) },
		"stage_duplicate": func(p string) error {
			if err := os.Mkdir(filepath.Join(p, privateStageName), 0700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(p, privateStageName, "oauth-state.json"), []byte("synthetic"), 0600)
		},
		"stage_unknown": func(p string) error {
			if err := os.Mkdir(filepath.Join(p, privateStageName), 0700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(p, privateStageName, "unknown"), nil, 0600)
		},
		"symlink": func(p string) error {
			return os.Symlink("session.json", filepath.Join(p, "accounts/synthetic-account/link"))
		},
		"hardlink": func(p string) error {
			return os.Link(filepath.Join(p, "oauth-state.json"), filepath.Join(p, "accounts/linked"))
		},
		"world_readable": func(p string) error {
			return os.Chmod(filepath.Join(p, "accounts/synthetic-account/session.json"), 0664)
		},
		"fifo": func(p string) error { return syscall.Mkfifo(filepath.Join(p, "accounts/fifo"), 0600) },
		"wrong_accounts_type": func(p string) error {
			if err := os.Rename(filepath.Join(p, "accounts"), filepath.Join(p, "original-accounts")); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(p, "accounts"), nil, 0600)
		},
	}
	if os.Geteuid() == 0 {
		cases["wrong_owner"] = func(p string) error {
			return os.Chown(filepath.Join(p, "accounts/synthetic-account/session.json"), 65534, -1)
		}
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			outer := migrationFixture(t)
			before := legacyFixture(t, outer)
			if err := mutate(outer); err != nil {
				t.Fatal(err)
			}
			if _, err := preparePrivateState(outer); err == nil {
				t.Fatal("unsafe state accepted")
			}
			for name, original := range before {
				if name != "oauth-state.json" {
					continue
				}
				info, err := os.Stat(filepath.Join(outer, name))
				if err != nil || !os.SameFile(info, original) || info.Mode().Perm() != 0660 {
					t.Fatal("preflight failure mutated original OAuth state")
				}
			}
		})
	}
}

func TestPrivateStateRejectsOuterSymlinkAndPrivatePermissionDrift(t *testing.T) {
	outer := migrationFixture(t)
	alias := filepath.Join(filepath.Dir(outer), "alias")
	if err := os.Symlink(outer, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := preparePrivateState(alias); err == nil {
		t.Fatal("outer symlink accepted")
	}
	private, err := preparePrivateState(outer)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0770); err != nil {
		t.Fatal(err)
	}
	if _, err := preparePrivateState(outer); err == nil {
		t.Fatal("permission drift accepted")
	}
}

func TestPrivateStateMetadataPolicy(t *testing.T) {
	outer := migrationFixture(t)
	info, err := os.Stat(outer)
	if err != nil || !stateMetadata(info, true, false) || stateMetadata(info, true, true) {
		t.Fatal("outer versus strict permission policy")
	}
}

func TestPrivateStateSurvivesOperatorRootACLReconciliation(t *testing.T) {
	outer := migrationFixture(t)
	before := legacyFixture(t, outer)
	// Linux UAPI posix_acl_xattr.h: version2 and five 8-byte entries.
	// A named synthetic operator plus ACL mask reproduces platform root mode0770.
	acl := make([]byte, 4+5*8)
	binary.LittleEndian.PutUint32(acl, 2)
	for i, row := range [][3]uint32{{1, 7, ^uint32(0)}, {2, 7, 1234}, {4, 0, ^uint32(0)}, {16, 7, ^uint32(0)}, {32, 0, ^uint32(0)}} {
		binary.LittleEndian.PutUint16(acl[4+i*8:], uint16(row[0]))
		binary.LittleEndian.PutUint16(acl[6+i*8:], uint16(row[1]))
		binary.LittleEndian.PutUint32(acl[8+i*8:], row[2])
	}
	for _, kind := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		if err := syscall.Setxattr(outer, kind, acl, 0); err != nil {
			t.Fatalf("synthetic ACL setup: %v", err)
		}
	}
	private, err := preparePrivateState(outer)
	if err != nil {
		t.Fatal(err)
	}
	assertMigrated(t, outer, private, before)
	// Platform lifecycle reconciliation refreshes the outer root/default ACLs,
	// leaving descendants unchanged. Re-run with that same real ACL operation.
	if err := syscall.Setxattr(outer, "system.posix_acl_access", acl, 0); err != nil {
		t.Fatalf("synthetic reconciliation: %v", err)
	}
	private, err = preparePrivateState(outer)
	if err != nil {
		t.Fatal(err)
	}
	assertMigrated(t, outer, private, before)
}

func TestPrivateStateIncompleteJournalFailsClosed(t *testing.T) {
	outer := migrationFixture(t)
	before := legacyFixture(t, outer)
	stop := errors.New("synthetic interruption")
	if _, err := preparePrivateStateWithHook(outer, func(step string) error {
		if step == "oauth-state.json" {
			return stop
		}
		return nil
	}); !errors.Is(err, stop) {
		t.Fatal("expected interrupted migration")
	}
	staged := filepath.Join(outer, privateStageName, "oauth-state.json")
	if err := os.Rename(staged, filepath.Join(outer, "held-original")); err != nil {
		t.Fatal(err)
	}
	if _, err := preparePrivateState(outer); err == nil {
		t.Fatal("missing journal entry accepted")
	}
	info, err := os.Stat(filepath.Join(outer, "held-original"))
	if err != nil || !os.SameFile(info, before["oauth-state.json"]) {
		t.Fatal("original state lost")
	}
}
