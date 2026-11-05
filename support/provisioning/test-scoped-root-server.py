"""Synthetic artifacts and loopback listeners only; never calls a Station or cloud."""
import contextlib
from dataclasses import replace
import hashlib
import http.client
import importlib.util
import io
import os
import json
from pathlib import Path
import re
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock
import zlib

BASE = Path(__file__).resolve().parent
SCRATCH = BASE / "temp" / "scoped-server-tests"
SCRATCH.mkdir(parents=True, exist_ok=True)
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("scoped_root_server", BASE / "scoped-root-server.py")
server = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = server
spec.loader.exec_module(server)


class Response:
    status = 200

    def getheader(self, name):
        return "application/json" if name == "Content-Type" else None


def question(name, kind=1):
    return (b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00"
            + b"".join(bytes([len(part)]) + part.encode() for part in name.split("."))
            + b"\x00" + kind.to_bytes(2, "big") + b"\x00\x01")


@contextlib.contextmanager
def running(listener):
    thread = threading.Thread(target=listener.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True)
    thread.start()
    try:
        yield listener.server_address[1]
    finally:
        listener.shutdown()
        listener.server_close()
        thread.join(timeout=2)


class ScopedTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        upstream = Path(os.environ.get("EQUASAR_KIT_PATH", str(BASE / "lite-kit"))) / "scripts/equasar_server.py"
        if not upstream.is_file():
            raise unittest.SkipTest("Set EQUASAR_KIT_PATH to an independently obtained lite-kit checkout")
        cls.area = tempfile.TemporaryDirectory(prefix="synthetic-", dir=SCRATCH)
        cls.kit = Path(cls.area.name)
        (cls.kit / "scripts").mkdir()
        shutil.copyfile(upstream, cls.kit / "scripts/equasar_server.py")
        (cls.kit / "certs").mkdir()
        openssl = shutil.which("openssl")
        if not openssl:
            git = shutil.which("git")
            candidate = Path(git).resolve().parent.parent / "usr/bin/openssl.exe" if git else Path()
            openssl = str(candidate) if candidate.is_file() else None
        if not openssl:
            raise unittest.SkipTest("An existing openssl is required to generate synthetic loopback TLS fixtures")
        subprocess.run([openssl, "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                        "-subj", "/CN=localhost", "-keyout", str(cls.kit / "certs/quasar-all.key"),
                        "-out", str(cls.kit / "certs/fullchain.pem")], check=True, capture_output=True)
        cls.version = "0.369.1.46.100.20260101.1"
        cls.bytes = b"synthetic firmware bytes: no real firmware"
        cls.folder = cls.kit / "firmware/yandexmicro"
        cls.folder.mkdir(parents=True)
        (cls.folder / (cls.version + ".zip")).write_bytes(cls.bytes)
        cls.meta = {"platform": "yandexmicro", "version": cls.version, "filename": cls.version + ".zip",
                    "size": len(cls.bytes), "crc32": zlib.crc32(cls.bytes) & 0xffffffff,
                    "sha256": hashlib.sha256(cls.bytes).hexdigest()}
        (cls.folder / "manifest.json").write_text(json.dumps(cls.meta))

    @classmethod
    def tearDownClass(cls):
        cls.area.cleanup()

    def setUp(self):
        self.events = []
        self.log = mock.patch.object(server, "event", self.events.append)
        self.log.start()
        self.addCleanup(self.log.stop)
        self.config = server.Config(True, "127.0.0.1", "127.0.0.1", "192.0.2.1", "yandexmicro",
                                    "synthetic-device", self.version, 18080, 18443, 15353)
        # Import/check must never perform even a routing-only connect probe.
        with mock.patch.object(socket.socket, "connect", side_effect=AssertionError("unexpected network call")):
            self.runtime = server.Runtime(self.config, self.kit)
        self.runtime.upstream.real_ip = lambda host: "127.0.0.1"
        self.calls = []
        self.source = {"status": "ok", "config": {"account_config": {"synthetic": True},
                       "device_config": {"retained": 42}, "system_config": {"retained": "stock"}},
                       "subscription": {"mode": "retained"}, "unknown": [1, 2]}

        def fetch(handler, host, ip, body, headers):
            self.calls.append((host, body))
            return Response(), json.dumps(self.source).encode()
        self.fetch = fetch
        self.handler_patch = mock.patch.object(self.runtime.upstream.EquasarHandler, "_upstream", fetch)
        self.handler_patch.start()
        self.addCleanup(self.handler_patch.stop)

    def request(self, port, path, method="GET", headers=None, tls=False, body=None):
        cls = http.client.HTTPSConnection if tls else http.client.HTTPConnection
        kwargs = {"timeout": 2}
        if tls:
            kwargs["context"] = ssl._create_unverified_context()
        connection = cls("127.0.0.1", port, **kwargs)
        try:
            connection.request(method, path, body=body, headers={"Host": "quasar.yandex.net", **(headers or {})})
            response = connection.getresponse()
            return response.status, dict(response.getheaders()), response.read()
        finally:
            connection.close()

    def test_config_validation_and_check_default_never_bind(self):
        path = self.kit / "synthetic-config.json"
        fields = {key: getattr(self.config, key) for key in server.Config.__dataclass_fields__}
        for key, value in (("bind_address", "0.0.0.0"), ("bind_address", "224.0.0.1"),
                           ("http_port", True), ("enabled", "yes"), ("platform", "wrong-model")):
            with self.subTest(field=key, value=value):
                path.write_text(json.dumps({**fields, key: value}))
                with self.assertRaises(server.CheckError):
                    server.Config.load(path)
        path.write_text(json.dumps(fields))
        with mock.patch.object(socket.socket, "bind", side_effect=AssertionError("unexpected bind")), \
             mock.patch.object(socket.socket, "connect", side_effect=AssertionError("unexpected connect")):
            self.assertEqual(server.main(["--config", str(path), "--kit", str(self.kit)]), 0)
            self.assertEqual(server.main(["--config", str(path), "--kit", str(self.kit), "--check"]), 0)
            self.assertEqual(server.main(["--config", str(path), "--kit", str(self.kit), "--offer-firmware"]), 1)
            path.write_text(json.dumps({**fields, "enabled": False}))
            self.assertEqual(server.main(["--config", str(path), "--kit", str(self.kit), "--serve"]), 1)

    def test_artifact_integrity_and_certificate_required(self):
        manifest = self.folder / "manifest.json"
        try:
            manifest.write_text(json.dumps({**self.meta, "sha256": "0" * 64}))
            with self.assertRaisesRegex(server.CheckError, "firmware_invalid"):
                server.Runtime(self.config, self.kit)
        finally:
            manifest.write_text(json.dumps(self.meta))
        with mock.patch.object(ssl.SSLContext, "load_cert_chain", side_effect=ssl.SSLError()):
            with self.assertRaisesRegex(server.CheckError, "certificate_invalid"):
                server.Runtime(self.config, self.kit)

    def test_http_bind_acl_model_device_and_host(self):
        listener = server.HTTPServer(self.runtime, 0)
        self.assertEqual(listener.server_address[0], "127.0.0.1")
        self.assertFalse(listener.verify_request(None, ("127.0.0.2", 1)))
        with running(listener) as port:
            for path in ("/get_sync_info?platform=wrong", "/get_sync_info?device_id=wrong",
                         "/get_sync_info?device_id=synthetic-device&device_id=wrong",
                         "/get_sync_info?platform=", "/get_sync_info?platform=yandexmicro&platform=wrong"):
                self.assertEqual(self.request(port, path)[0], 403)
            self.assertEqual(self.request(port, "/get_sync_info", headers={"Host": "example.invalid"})[0], 403)
            self.assertEqual(self.calls, [])
            status, _, data = self.request(port, "/get_sync_info?platform=yandexmicro&device_id=synthetic-device")
            self.assertEqual(status, 200)
            result = json.loads(data)
            self.assertEqual(result["unknown"], self.source["unknown"])
            self.assertEqual(result["subscription"], self.source["subscription"])
            self.assertEqual(result["config"]["account_config"], self.source["config"]["account_config"])
            self.assertEqual(result["config"]["device_config"]["retained"], 42)
            self.assertTrue(result["config"]["system_config"]["adbd"]["remoteActivation"])
            self.assertNotIn("adbd", self.source)

    def test_network_source_acl_rejects_other_loopback_peer(self):
        self.runtime.config = replace(self.config, allowed_station_address="127.0.0.2")
        with running(server.HTTPServer(self.runtime, 0)) as port:
            with self.assertRaises((http.client.RemoteDisconnected, ConnectionError, OSError)):
                self.request(port, "/generate_204")
        self.assertEqual(self.calls, [])

    def test_no_firmware_by_default_then_explicit_arming_and_ranges(self):
        with running(server.HTTPServer(self.runtime, 0)) as port:
            for path in ("/check_updates", "/quasar/check_updates/v2", "/allow_update", "/check%5fupdates"):
                self.assertEqual(json.loads(self.request(port, path)[2]), {"hasUpdate": False})
            self.assertEqual(self.request(port, self.runtime.firmware_url)[0], 404)
            self.runtime.offer = True
            payload = json.loads(self.request(port, "/check_updates?platform=yandexmicro")[2])
            self.assertTrue(payload["hasUpdate"])
            self.assertEqual(payload["version"], self.version)
            self.assertEqual(json.loads(self.request(port, "/check_updates?version=" + self.version)[2]), {"hasUpdate": False})
            self.assertEqual(self.request(port, "/yandexmini_2/ota/other.zip")[0], 404)
            status, _, data = self.request(port, self.runtime.firmware_url)
            self.assertEqual((status, data), (200, self.bytes))
            self.assertEqual(self.request(port, self.runtime.firmware_url, headers={"Range": "bytes=2-5"})[2], self.bytes[2:6])
            self.assertEqual(self.request(port, self.runtime.firmware_url, headers={"Range": "bytes=-4"})[2], self.bytes[-4:])
            status, headers, data = self.request(port, self.runtime.firmware_url, method="HEAD")
            self.assertEqual((status, int(headers["Content-Length"]), data), (200, len(self.bytes), b""))
            self.assertEqual(self.request(port, self.runtime.firmware_url, headers={"Range": "bytes=999-"})[0], 416)
            self.assertEqual(self.request(port, "/generate_204", method="HEAD")[0], 204)
        self.assertEqual(self.calls, [])

    def test_upstream_failures_never_use_local_template(self):
        with running(server.HTTPServer(self.runtime, 0)) as port:
            with mock.patch.object(self.runtime.upstream, "sync_payload", side_effect=AssertionError("fallback must not execute")):
                self.source = {"status": "error"}
                status, _, payload = self.request(port, "/get_sync_info")
                self.assertEqual(status, 502)
                self.assertEqual(json.loads(payload), {"error": "upstream_failed"})

    def test_tls_stalled_client_does_not_block_second_accept(self):
        listener = server.HTTPServer(self.runtime, 0, tls=True, timeout=0.6)
        with running(listener) as port:
            stalled = socket.create_connection(("127.0.0.1", port), timeout=1)
            try:
                time.sleep(0.05)
                before = time.monotonic()
                self.assertEqual(self.request(port, "/generate_204", tls=True)[0], 204)
                self.assertLess(time.monotonic() - before, 0.5)
            finally:
                stalled.close()

    def test_partial_post_never_forwards_and_logs_only_categories(self):
        with running(server.HTTPServer(self.runtime, 0, timeout=0.2)) as port:
            with socket.create_connection(("127.0.0.1", port), timeout=1) as client:
                client.sendall(b"POST /get_sync_info?secret=synthetic-secret HTTP/1.1\r\nHost: quasar.yandex.net\r\nContent-Length: 10\r\n\r\nx")
                client.shutdown(socket.SHUT_WR)
                self.assertIn(b" 400 ", client.recv(4096))
        self.assertEqual(self.calls, [])
        self.assertTrue(all(re.fullmatch(r"[a-z_]+", item) for item in self.events))

    def test_dns_only_three_domains_and_forward_other_names(self):
        forwarded = []
        self.runtime.forward_dns = lambda query: forwarded.append(query) or b"synthetic-forward-response"
        for name in server.NAMES:
            response = self.runtime.dns_reply(question(name), "192.0.2.1")
            self.assertEqual(response[6:8], b"\x00\x01")
            self.assertEqual(response[-4:], socket.inet_aton("127.0.0.1"))
            self.assertEqual(self.runtime.dns_reply(question(name, 28), "127.0.0.1")[6:8], b"\x00\x00")
        for name in ("example.invalid", "quasar.yandex.net.example.invalid", "uniproxy.alice.yandex.net"):
            self.assertEqual(self.runtime.dns_reply(question(name), "127.0.0.1"), b"synthetic-forward-response")
        self.assertEqual(len(forwarded), 3)
        self.assertIsNone(self.runtime.dns_reply(question("quasar.yandex.net"), "198.51.100.1"))
        self.assertIsNone(self.runtime.dns_reply(b"short", "127.0.0.1"))

    def test_dns_udp_and_fragmented_tcp_loopback(self):
        self.runtime.forward_dns = lambda query: (_ for _ in ()).throw(AssertionError("cloud DNS forbidden"))
        udp, tcp = server.dns_servers(self.runtime, 0)
        self.assertEqual(udp.server_address[0], "127.0.0.1")
        with running(udp) as port, running(tcp):
            query = question("quasar.yandex.net")
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as client:
                client.settimeout(1)
                client.sendto(query, ("127.0.0.1", port))
                self.assertEqual(client.recv(4096)[-4:], socket.inet_aton("127.0.0.1"))
            with socket.create_connection(("127.0.0.1", port), timeout=1) as client:
                frame = len(query).to_bytes(2, "big") + query
                for chunk in (frame[:1], frame[1:2], frame[2:8], frame[8:]):
                    client.sendall(chunk)
                head = server.read_exact(client, 2)
                reply = server.read_exact(client, int.from_bytes(head, "big"))
                self.assertEqual(reply[-4:], socket.inet_aton("127.0.0.1"))


if __name__ == "__main__":
    unittest.main()
