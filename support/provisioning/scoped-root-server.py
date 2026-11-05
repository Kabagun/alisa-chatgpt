#!/usr/bin/env python3
"""Explicitly armed, address-scoped adapter for the unmodified eQuasar kit."""
import argparse
from dataclasses import dataclass
import hashlib
import http.server
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import re
import socket
import socketserver
import ssl
import threading
from urllib.parse import parse_qs, unquote, urlsplit
import zlib

NAMES = frozenset(("quasar.yandex.net", "quasar.yandex.ru", "scbh.yandex.net"))
TIMEOUT = 5
MAX_BODY = 1 << 20
_EVENT_LOCK = threading.Lock()


def event(category):
    # Fixed categories only: never request strings, addresses or exception text.
    with _EVENT_LOCK:
        print(json.dumps({"event": category}), flush=True)


class CheckError(ValueError):
    pass


@dataclass(frozen=True, repr=False)
class Config:
    enabled: bool
    bind_address: str
    allowed_station_address: str
    gateway_address: str
    platform: str
    station_device_id: str
    target_version: str
    http_port: int
    https_port: int
    dns_port: int

    @classmethod
    def load(cls, path):
        try:
            data = json.loads(Path(path).read_text(encoding="utf-8-sig"))
            value = cls(**{field: data[field] for field in cls.__dataclass_fields__})
            if type(value.enabled) is not bool:
                raise ValueError
            for name in ("bind_address", "allowed_station_address", "gateway_address"):
                if not isinstance(getattr(value, name), str):
                    raise ValueError
                address = ipaddress.IPv4Address(getattr(value, name))
                if address.is_unspecified or address.is_multicast or int(address) == 0xffffffff:
                    raise ValueError
            for name in ("http_port", "https_port", "dns_port"):
                port = getattr(value, name)
                if type(port) is not int or not 1 <= port <= 65535:
                    raise ValueError
            for name in ("platform", "station_device_id", "target_version"):
                if not re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", getattr(value, name)):
                    raise ValueError
            if value.platform != "yandexmicro" or not value.target_version.startswith("0.369.1.46."):
                raise ValueError
            if len({value.http_port, value.https_port, value.dns_port}) != 3:
                raise ValueError
            return value
        except (OSError, ValueError, KeyError, TypeError):
            raise CheckError("config_invalid") from None

    def http_allowed(self, address):
        return address == self.allowed_station_address

    def dns_allowed(self, address):
        return self.dns_source_class(address) is not None

    def dns_source_class(self, address):
        if address == self.allowed_station_address:
            return "station"
        if address == self.gateway_address:
            return "gateway"
        try:
            if address == self.bind_address or ipaddress.ip_address(address).is_loopback:
                return "local"
        except ValueError:
            pass
        return None


def load_upstream(kit, bind_address):
    # Prevent the upstream import-time _local_ip() routing probe and env mode switches.
    values = {"LOCAL_IP": bind_address, "SYNC_LOCAL": "0", "SYNC_RETRIES": "2",
              "SYNC_RETRY_DELAY": "0.5", "UPSTREAM_TIMEOUT": "8"}
    previous = {key: os.environ.get(key) for key in values}
    try:
        os.environ.update(values)
        spec = importlib.util.spec_from_file_location("scoped_equasar", Path(kit) / "scripts/equasar_server.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    except Exception:
        raise CheckError("upstream_import_failed") from None
    finally:
        for key, value in previous.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
    # Do not use upstream dispatch/cache/dump/fallback logging, including retry prints.
    module.print = lambda *args, **kwargs: None
    module.log_local = module.log_yandex = module.log_dns = lambda *args, **kwargs: None
    module.SYNC_LOCAL = False
    return module


class Runtime:
    def __init__(self, config, kit, offer=False):
        self.config, self.kit, self.offer = config, Path(kit).resolve(), offer
        try:
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            context.minimum_version = ssl.TLSVersion.TLSv1_2
            context.load_cert_chain(self.kit / "certs/fullchain.pem", self.kit / "certs/quasar-all.key")
            self.tls_context = context
        except (OSError, ssl.SSLError):
            raise CheckError("certificate_invalid") from None
        try:
            folder = self.kit / "firmware" / config.platform
            meta = json.loads((folder / "manifest.json").read_text(encoding="utf-8-sig"))
            name = meta["filename"]
            if (meta["platform"] != config.platform or meta["version"] != config.target_version
                    or name != config.target_version + ".zip" or Path(name).name != name):
                raise ValueError
            self.firmware = folder / name
            if self.firmware.resolve().parent != folder.resolve() or self.firmware.is_symlink():
                raise ValueError
            sha, crc, size = hashlib.sha256(), 0, 0
            with self.firmware.open("rb") as stream:
                for block in iter(lambda: stream.read(1 << 20), b""):
                    sha.update(block)
                    crc, size = zlib.crc32(block, crc), size + len(block)
            if (size <= 0 or size != meta["size"] or (crc & 0xffffffff) != meta["crc32"]
                    or sha.hexdigest() != meta["sha256"]):
                raise ValueError
            stat = self.firmware.stat()
            self.firmware_stamp = (stat.st_size, stat.st_mtime_ns)
            self.fw = {"path": str(self.firmware), "name": name, "version": config.target_version,
                       "platform": config.platform, "crc32": crc & 0xffffffff}
            self.firmware_url = "/" + config.platform + "/ota/" + name
        except (OSError, ValueError, KeyError, TypeError):
            raise CheckError("firmware_invalid") from None
        self.upstream = load_upstream(self.kit, config.bind_address)
        # Pin every upstream firmware helper to the one checked platform/artifact.
        self.upstream.OTA_FILE = str(self.firmware)
        self.upstream.fw_manifest = lambda platform: dict(self.fw) if platform == config.platform else None
        self.forward_dns = self.upstream._dns_forward

    def query_allowed(self, target):
        try:
            url = urlsplit(target)
            if url.scheme or url.netloc or not url.path.startswith("/"):
                return False
            query = parse_qs(url.query, keep_blank_values=True, errors="strict", max_num_fields=64)
            return all(all(value == expected for value in query.get(key, []))
                       for key, expected in (("platform", self.config.platform),
                                             ("device_id", self.config.station_device_id)))
        except (ValueError, UnicodeError):
            return False

    def dns_reply(self, query, address):
        if not self.config.dns_allowed(address):
            event("dns_source_denied")
            return None
        event("dns_query_received")
        name, kind = self.upstream._dns_qname(query)
        # One ordinary question, with complete QTYPE/QCLASS; no compressed query name.
        if (name is None or len(query) < 12 or query[2] & 0x80
                or query[4:6] != b"\x00\x01"):
            event("dns_parse_rejected")
            return None
        end = 12
        while end < len(query) and query[end]:
            length = query[end]
            if length > 63 or end + length + 1 >= len(query):
                event("dns_parse_rejected")
                return None
            end += length + 1
        if end + 5 > len(query):
            event("dns_parse_rejected")
            return None
        if name.decode("ascii", "replace") in NAMES:
            event("dns_override")
            return self.upstream._dns_reply(query, self.config.bind_address if kind == 1 and query[end+3:end+5] == b"\x00\x01" else None)
        event("dns_forward")
        reply = self.forward_dns(query)
        event("dns_forwarded" if reply else "dns_forward_failed")
        return reply

    def handler(self):
        runtime = self

        class Handler(runtime.upstream.EquasarHandler):
            def log_message(self, *args):
                event("http_response")

            def log_request(self, code="-", size="-"):
                known = {
                    200: "http_status_ok", 204: "http_status_no_content",
                    206: "http_status_partial", 400: "http_status_bad_request",
                    403: "http_status_forbidden", 404: "http_status_not_found",
                    416: "http_status_range_rejected", 500: "http_status_server_error",
                    502: "http_status_bad_gateway", 503: "http_status_unavailable",
                }
                try:
                    status = int(code)
                except (ValueError, TypeError):
                    status = None
                event(known.get(status, "http_status_other"))

            def do_HEAD(self):
                self.handle_request()

            def _reply(self, status, data=b"", content_type="application/json", extra=()):
                self.send_response(status)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(data)))
                self.send_header("Connection", "close")
                for key, value in extra:
                    self.send_header(key, value)
                self.end_headers()
                if self.command != "HEAD":
                    self.wfile.write(data)

            def _read_body(self):
                def exact(size):
                    data = self.rfile.read(size)
                    if len(data) != size:
                        raise ValueError
                    return data
                transfer = self.headers.get("Transfer-Encoding")
                if transfer:
                    if transfer.lower() != "chunked" or self.headers.get("Content-Length"):
                        raise ValueError
                    body = bytearray()
                    while True:
                        line = self.rfile.readline(130)
                        if len(line) > 128 or not line.endswith(b"\r\n"):
                            raise ValueError
                        size = int(line.strip().split(b";", 1)[0], 16)
                        if size < 0 or len(body) + size > MAX_BODY:
                            raise ValueError
                        if size == 0:
                            # Trailers are not needed for Station API requests.
                            if exact(2) != b"\r\n":
                                raise ValueError
                            return bytes(body)
                        body.extend(exact(size))
                        if exact(2) != b"\r\n":
                            raise ValueError
                size = int(self.headers.get("Content-Length", "0"))
                if size < 0 or size > MAX_BODY:
                    raise ValueError
                return exact(size)

            def handle_request(self):
                self.close_connection = True
                if not runtime.config.http_allowed(self.client_address[0]):
                    event("http_source_denied")
                    self._reply(403)
                    return
                if not runtime.query_allowed(self.path):
                    event("http_query_denied")
                    self._reply(403)
                    return
                host = self.headers.get("Host", runtime.upstream.UPSTREAM_HOST).lower()
                if not re.fullmatch(r"(?:quasar\.yandex\.(?:net|ru)|scbh\.yandex\.net)(?::[0-9]{1,5})?", host):
                    event("http_host_denied")
                    self._reply(403)
                    return
                host = host.split(":", 1)[0]
                path = unquote(urlsplit(self.path).path).lower()
                if path == "/generate_204":
                    event("http_connectivity_check")
                    self._reply(204, extra=(("X-Start-Time", str(__import__("time").time_ns() // 1000)),))
                    return
                if path in ("/", "/favicon.ico"):
                    self._reply(404)
                    return
                if any(part in ("check_updates", "allow_update") for part in path.split("/")):
                    event("firmware_metadata_requested")
                    payload = runtime.upstream.NO_UPDATE_PAYLOAD
                    if runtime.offer and parse_qs(urlsplit(self.path).query).get("version") != [runtime.config.target_version]:
                        payload = runtime.upstream._downgrade_payload(runtime.fw)
                        event("firmware_offer")
                    self._reply(200, payload)
                    return
                if path.endswith(".zip") or "/ota/" in path:
                    if not runtime.offer or path != runtime.firmware_url or self.command not in ("GET", "HEAD"):
                        self._reply(404)
                        return
                    stat = runtime.firmware.stat()
                    if (stat.st_size, stat.st_mtime_ns) != runtime.firmware_stamp:
                        self._reply(503)
                        return
                    value = self.headers.get("Range")
                    if value:
                        match = re.fullmatch(r"bytes=([0-9]*)-([0-9]*)", value) if len(value) <= 128 else None
                        size = stat.st_size
                        if not match or not any(match.groups()):
                            self._reply(416, extra=(("Content-Range", f"bytes */{size}"),))
                            return
                        left, right = match.groups()
                        start = int(left) if left else max(0, size - int(right))
                        end = min(int(right), size - 1) if left and right else size - 1
                        if start >= size or start > end or (not left and int(right) == 0):
                            self._reply(416, extra=(("Content-Range", f"bytes */{size}"),))
                            return
                        self.headers.replace_header("Range", f"bytes={start}-{end}")
                    event("firmware_download_started")
                    super().serve_ota(runtime.config.platform, runtime.firmware.name)
                    return
                try:
                    body = self._read_body() if self.command == "POST" else None
                except (ValueError, OSError):
                    self._reply(400)
                    return
                skip = {"host", "connection", "content-length", "accept-encoding", "transfer-encoding"}
                headers = {key: value for key, value in self.headers.items() if key.lower() not in skip}
                try:
                    response, data = self._upstream(host, runtime.upstream.real_ip(host), body, headers)
                    if runtime.upstream.is_sync_path(path):
                        event("sync_config_requested")
                        source = json.loads(data) if response.status == 200 else None
                        if (not isinstance(source, dict) or source.get("status") != "ok"
                                or not isinstance(source.get("config"), dict)):
                            raise ValueError
                        data = runtime.upstream.sync_payload(source=source)
                    self._reply(response.status, data, response.getheader("Content-Type") or "application/json")
                except Exception:
                    # Fail visibly. Never call upstream local template/cache fallbacks.
                    event("upstream_failed")
                    self._reply(502, b'{"error":"upstream_failed"}')

        return Handler


class HTTPServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, runtime, port, tls=False, timeout=TIMEOUT):
        self.runtime, self.tls, self.io_timeout = runtime, tls, timeout
        super().__init__((runtime.config.bind_address, port), runtime.handler())

    def verify_request(self, request, address):
        return self.runtime.config.http_allowed(address[0])

    def process_request_thread(self, request, address):
        # The listening socket is plain TCP. Slow TLS handshakes happen in workers.
        connection = request
        try:
            connection.settimeout(self.io_timeout)
            if self.tls:
                connection = self.runtime.tls_context.wrap_socket(connection, server_side=True, do_handshake_on_connect=False)
                connection.do_handshake()
            self.finish_request(connection, address)
        except ssl.SSLError as error:
            reasons = {
                "TLSV1_ALERT_UNKNOWN_CA": "tls_client_unknown_ca",
                "SSLV3_ALERT_CERTIFICATE_EXPIRED": "tls_client_certificate_expired",
                "SSLV3_ALERT_BAD_CERTIFICATE": "tls_client_bad_certificate",
                "TLSV1_ALERT_CERTIFICATE_UNKNOWN": "tls_client_certificate_unknown",
                "UNEXPECTED_EOF_WHILE_READING": "tls_client_eof",
            }
            event(reasons.get(error.reason, "tls_connection_failed"))
        except (OSError, ValueError):
            event("connection_failed")
        finally:
            self.shutdown_request(connection)

    def handle_error(self, request, address):
        event("http_failed")


def read_exact(connection, size):
    result = bytearray()
    while len(result) < size:
        part = connection.recv(size - len(result))
        if not part:
            return None
        result.extend(part)
    return bytes(result)


def dns_servers(runtime, port):
    class UDP(socketserver.BaseRequestHandler):
        def handle(self):
            data, sock = self.request
            reply = runtime.dns_reply(data, self.client_address[0])
            if reply:
                sock.sendto(reply, self.client_address)
                event("dns_response_sent_udp")

    class TCP(socketserver.BaseRequestHandler):
        def handle(self):
            self.request.settimeout(TIMEOUT)
            try:
                head = read_exact(self.request, 2)
                if not head or not 12 <= int.from_bytes(head, "big") <= 4096:
                    event("dns_parse_rejected")
                    return
                query = read_exact(self.request, int.from_bytes(head, "big"))
                if not query:
                    event("dns_parse_rejected")
                    return
                reply = runtime.dns_reply(query, self.client_address[0]) if query else None
                if reply:
                    self.request.sendall(len(reply).to_bytes(2, "big") + reply)
                    event("dns_response_sent_tcp")
            except OSError:
                event("dns_connection_failed")

    class Scoped:
        daemon_threads = True
        allow_reuse_address = True

        def verify_request(self, request, address):
            source = runtime.config.dns_source_class(address[0])
            category = {"station": "dns_accepted_station", "gateway": "dns_accepted_gateway",
                        "local": "dns_accepted_local"}.get(source, "dns_source_denied")
            event(category)
            return source is not None

        def handle_error(self, request, address):
            event("dns_failed")

    class UDPServer(Scoped, socketserver.ThreadingUDPServer):
        pass

    class TCPServer(Scoped, socketserver.ThreadingTCPServer):
        pass

    address = (runtime.config.bind_address, port)
    udp = UDPServer(address, UDP)
    try:
        tcp = TCPServer((address[0], udp.server_address[1]), TCP)
    except Exception:
        udp.server_close()
        raise
    return udp, tcp


def main(argv=None):
    base = Path(__file__).resolve().parent
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, default=base / "root-config.local.json")
    parser.add_argument("--kit", type=Path, default=base / "lite-kit")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--check", action="store_true")
    mode.add_argument("--serve", action="store_true")
    parser.add_argument("--offer-firmware", action="store_true")
    args = parser.parse_args(argv)
    servers = []
    try:
        config = Config.load(args.config)
        if args.serve and not config.enabled:
            raise CheckError("server_disabled")
        if args.offer_firmware and not args.serve:
            raise CheckError("firmware_offer_requires_serve")
        runtime = Runtime(config, args.kit, args.offer_firmware)
        if not args.serve:
            event("check_ok")
            return 0
        servers.append(HTTPServer(runtime, config.http_port))
        servers.append(HTTPServer(runtime, config.https_port, tls=True))
        servers.extend(dns_servers(runtime, config.dns_port))
        for server in servers:
            threading.Thread(target=server.serve_forever, daemon=True).start()
        event("server_started_firmware_armed" if runtime.offer else "server_started_no_firmware")
        threading.Event().wait()
    except CheckError as error:
        event(str(error))
        return 1
    except (OSError, ValueError):
        event("server_setup_failed")
        return 1
    except KeyboardInterrupt:
        event("server_stopped")
    finally:
        for server in servers:
            server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
