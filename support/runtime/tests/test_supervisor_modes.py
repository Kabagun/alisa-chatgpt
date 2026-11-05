"""Offline supervisor tests: real fixtures/children, synthetic owner/mode metadata.

No device, account, network, /data or system-service access. DrvFS does not model
Linux chmod/chown, so only ls owner/mode output is substituted. File type,
symlink rejection, contents, shell argument boundaries and dispatch are real.
"""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


RUNTIME = Path(__file__).resolve().parent
TEMPLATE = RUNTIME.parent / "supervisor-template.sh"
SYNTHETIC_PROJECT = "g-p-" + "0" * 32


class SupervisorModes(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = TEMPLATE.read_text(encoding="utf-8")
        cls.helpers = cls.source[: cls.source.index("read_proc() {")]
        cls.loop = cls.source[
            cls.source.index("run_supervisor() {") : cls.source.index(
                '\nsafe_absolute "$SELF"'
            )
        ]
        cls.dispatch = cls.source[cls.source.index('case "${1:-status}" in') :]
        (RUNTIME / "temp").mkdir(exist_ok=True)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="mode-", dir=RUNTIME / "temp")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.app = self.root / "app"
        self.app.mkdir()

    def shell(self, commands, *, phrase="synthetic launch", file_meta=None,
              parent_meta=None, extra_env=None, extra_source=""):
        env = {k: v for k, v in os.environ.items() if not k.startswith("ALISA_")}
        env.update(
            FIXTURE_ROOT=str(self.app),
            ALISA_ACTIVATION_COMMAND=phrase,
            TEST_FILE_META=file_meta or "-rw------- 1 0 0 20 ignored path",
            TEST_PARENT_META=parent_meta or "drwx------ 1 0 0 20 ignored path",
        )
        env.update(extra_env or {})
        setup = r'''
APP_ROOT=$FIXTURE_ROOT
PROJECT_FILE=$APP_ROOT/project.id
LED_FEEDBACK_FILE=$APP_ROOT/led-feedback.enabled
ACTIVATION_MODE_FILE=$APP_ROOT/activation.mode
ls() {
    for tested_node do :; done
    if [ "$tested_node" = "$APP_ROOT" ]; then
        printf '%s\n' "$TEST_PARENT_META"
    else
        printf '%s\n' "$TEST_FILE_META"
    fi
}
'''
        return subprocess.run(
            ["sh"], input=self.helpers + setup + extra_source + commands,
            text=True, capture_output=True, env=env, cwd=self.root, timeout=5,
        )

    def mode(self, contents):
        (self.app / "activation.mode").write_bytes(contents)

    def load(self, **kwargs):
        result = self.shell(
            'load_activation_mode; result=$?; '
            'printf "%s\\0%s" "$result" "$ACTIVATION_COMMAND"\n', **kwargs
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.split("\0", 1)

    def test_missing_mode_preserves_existing_environment(self):
        for phrase in ["", "synthetic launch", "  launch 'quoted' $(touch forbidden)  "]:
            with self.subTest(phrase=phrase):
                self.assertEqual(self.load(phrase=phrase), ["0", phrase])
        self.assertFalse((self.root / "forbidden").exists())

    def test_native_mode_clears_inherited_scene_command(self):
        for data in [b"native-yandex\n", b"native-yandex"]:
            with self.subTest(data=data):
                self.mode(data)
                self.assertEqual(self.load(phrase="existing scene phrase"), ["0", ""])

    def test_command_mode_uses_existing_phrase_unchanged(self):
        phrase = "  launch 'quoted' $(touch forbidden) ; value  "
        for data in [b"alice-command\n", b"alice-command"]:
            with self.subTest(data=data):
                self.mode(data)
                self.assertEqual(self.load(phrase=phrase), ["0", phrase])
        self.assertFalse((self.root / "forbidden").exists())

    def test_invalid_or_multiline_selector_is_rejected(self):
        for data in [b"", b"other\n", b"NATIVE-YANDEX\n", b" native-yandex\n",
                     b"native-yandex \n", b"native-yandex\r\n",
                     b"native-yandex\n\n", b"native-yandex\nalice-command\n",
                     b"native-yandex\0", b"native-yandex;touch forbidden\n"]:
            with self.subTest(data=data):
                self.mode(data)
                self.assertEqual(self.load()[0], "1")
        self.assertFalse((self.root / "forbidden").exists())

    def test_selector_requires_exact_root_0600_metadata(self):
        self.mode(b"native-yandex\n")
        for metadata in ["-rw-r--r-- 1 0 0 20 ignored path",
                         "-rw-r----- 1 0 0 20 ignored path",
                         "-r-------- 1 0 0 20 ignored path",
                         "-rwx------ 1 0 0 20 ignored path",
                         "-rw------- 1 1000 1000 20 ignored path",
                         "-rw-------+ 1 0 0 20 ignored path"]:
            with self.subTest(metadata=metadata):
                self.assertEqual(self.load(file_meta=metadata)[0], "1")

    def test_selector_symlink_and_directory_are_rejected(self):
        target = self.root / "target"
        target.write_text("native-yandex\n")
        mode = self.app / "activation.mode"
        mode.symlink_to(target)
        self.assertEqual(self.load()[0], "1")
        mode.unlink()
        mode.mkdir()
        self.assertEqual(self.load()[0], "1")

    def test_present_selector_requires_private_root_owned_parent(self):
        self.mode(b"native-yandex\n")
        for metadata in ["drwxr-xr-x 1 0 0 20 ignored path",
                         "drwxr-x--- 1 0 0 20 ignored path",
                         "drwx------ 1 1000 1000 20 ignored path"]:
            with self.subTest(metadata=metadata):
                self.assertEqual(self.load(parent_meta=metadata)[0], "1")
        real = self.app
        alias = self.root / "parent-link"
        alias.symlink_to(real, target_is_directory=True)
        self.app = alias
        self.assertEqual(self.load()[0], "1")

    def test_command_mode_requires_usable_existing_command(self):
        self.mode(b"alice-command\n")
        for phrase in ["", "   ", ";!?", "x" * 257, "one\ntwo", "one\rtwo", "one\ttwo"]:
            with self.subTest(phrase=repr(phrase)):
                self.assertEqual(self.load(phrase=phrase)[0], "1")

    def test_bad_mode_blocks_start_and_run_before_runtime_writes(self):
        self.mode(b"bad selector\n")
        stubs = r'''
ensure_state() { printf 'UNEXPECTED_STATE_WRITE\n'; exit 80; }
'''
        for action in ["start", "run"]:
            with self.subTest(action=action):
                result = self.shell(
                    'set -- ' + action + '\n' + self.dispatch,
                    extra_source=self.loop + stubs,
                )
                self.assertEqual(result.returncode, 1)
                self.assertEqual(result.stderr, "ALISA_INVALID_ACTIVATION_MODE\n")
                self.assertEqual(result.stdout, "")

    def test_stop_and_status_do_not_load_bad_selector(self):
        self.mode(b"bad selector\n")
        stubs = r'''
load_activation_mode() { printf 'UNEXPECTED_MODE_READ\n'; exit 80; }
read_supervisor() { return 1; }
'''
        for action, code, output in [("stop", 0, "ALISA_NOT_RUNNING\n"),
                                     ("status", 3, "ALISA_NOT_RUNNING\n")]:
            with self.subTest(action=action):
                result = self.shell('set -- ' + action + '\n' + self.dispatch, extra_source=stubs)
                self.assertEqual((result.returncode, result.stdout, result.stderr), (code, output, ""))
        running = r'''
load_activation_mode() { printf 'UNEXPECTED_MODE_READ\n'; exit 80; }
read_supervisor() { SUP_PID=777; SUP_START=1; return 0; }
verify_count=0
verify_supervisor() { verify_count=$((verify_count+1)); [ "$verify_count" = 1 ]; }
kill() { [ "$1:$2" = '-TERM:777' ]; }
'''
        result = self.shell('set -- stop\n' + self.dispatch, extra_source=running)
        self.assertEqual((result.returncode, result.stdout, result.stderr), (0, "ALISA_STOPPED\n", ""))
        result = self.shell('set -- status\n' + self.dispatch, extra_source=running)
        self.assertEqual((result.returncode, result.stdout, result.stderr), (0, "ALISA_SUPERVISOR_RUNNING\n", ""))

    def test_valid_start_validates_mode_without_changing_running_service(self):
        self.mode(b"native-yandex\n")
        stubs = r'''
ensure_state() { :; }
read_supervisor() { return 0; }
'''
        result = self.shell('set -- start\n' + self.dispatch, extra_source=stubs)
        self.assertEqual((result.returncode, result.stdout, result.stderr), (0, "ALISA_ALREADY_RUNNING\n", ""))

    def test_real_inert_child_preserves_flags_libraries_and_mode(self):
        client = self.app / "client"
        client.write_text('#!/bin/sh\nprintf "%s\\0" "$@" > "$CAPTURE"\n'
                          'printf "%s" "$LD_LIBRARY_PATH" > "$LIB_CAPTURE"\n')
        client.chmod(0o700)
        (self.app / "project.id").write_text(SYNTHETIC_PROJECT + "\n")
        (self.app / "led-feedback.enabled").write_text("1\n")
        phrase = "synthetic 'quoted' $(touch forbidden) ; launch"
        stubs = r'''
STATE=$APP_ROOT/state
PIDFILE=$STATE/supervisor.pid
LOCK=$STATE/lock
CLIENT=$APP_ROOT/client
ensure_state() { mkdir "$STATE"; }
cleanup() { :; }
request_stop() { stopping=1; }
read_proc() { PROC_START=1; }
ready() { return 0; }
owned_child() { :; }
stop_child() { :; }
diagnostic() { :; }
pause() { stopping=1; }
'''
        for mode in [None, "native-yandex", "alice-command"]:
            with self.subTest(mode=mode):
                if mode is not None:
                    self.mode((mode + "\n").encode())
                else:
                    (self.app / "activation.mode").unlink(missing_ok=True)
                state = self.app / "state"
                if state.exists():
                    (state / "supervisor.pid").unlink()
                    (state / "lock").rmdir()
                    state.rmdir()
                capture, libs = self.root / "captured-argv", self.root / "captured-libs"
                result = self.shell(
                    "run_supervisor\n", phrase=phrase,
                    extra_source=self.loop + stubs,
                    extra_env={"CAPTURE": str(capture), "LIB_CAPTURE": str(libs),
                               "ALISA_VOICE": "synthetic voice", "ALISA_BACKEND_MODEL": "synthetic-model"},
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                wanted = ["run", "--voice=synthetic voice", "--backend-model=synthetic-model",
                          "--backend-thinking-effort=disabled"]
                if mode != "native-yandex":
                    wanted.append("--activation-command=" + phrase)
                wanted += ["--project-id=" + SYNTHETIC_PROJECT, "--led-feedback", ""]
                self.assertEqual(capture.read_bytes().decode().split("\0"), wanted)
                self.assertEqual(libs.read_text(), "/system/vendor/quasar/libs:/system/vendor/lib:/system/lib:/usr/lib:/lib")
                self.assertFalse((self.root / "forbidden").exists())

    def test_project_and_led_validation_still_fail_closed(self):
        for path, contents, loader, variable, fallback in [
            ("project.id", "bad project\n", "load_project", "PROJECT_ID", ""),
            ("led-feedback.enabled", "true\n", "load_led_feedback", "LED_FEEDBACK", "0"),
        ]:
            with self.subTest(path=path):
                (self.app / path).write_text(contents)
                result = self.shell(loader + '; result=$?; printf "%s:%s" "$result" "$' + variable + '"\n')
                self.assertEqual((result.returncode, result.stdout), (0, "1:" + fallback))

    def test_template_shell_syntax(self):
        result = subprocess.run(["sh", "-n", str(TEMPLATE)], capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
