#!/usr/bin/env python3
"""Inspect Git index content; never echo a suspicious value."""
import argparse
import json
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parents[1]
BLOCKED_DIRS = {"temp", ".cache", ".venv", "node_modules", "__pycache__", "build", "dist", "bin"}
BLOCKED_NAMES = {"session.json", "config.mk", "public-config.json", "vendor-profile.json", "BUILD-MANIFEST.json"}
BLOCKED_SUFFIXES = {".key", ".pem", ".p12", ".pfx", ".db", ".pcap", ".pcapng", ".pcm", ".wav", ".mp3", ".exe", ".dll", ".so", ".a", ".o"}
PATTERNS = {
    "private_key": re.compile(r"-----BEGIN (?:RSA |OPENSSH |EC |DSA )?PRIVATE KEY-----"),
    "github_token": re.compile(r"(?:gh[pousr]_|github_pat_)[A-Za-z0-9_]{20,}"),
    "jwt": re.compile(r"eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}"),
    "personal_windows_path": re.compile(r"[A-Za-z]:[/\\]Users[/\\](?!Public\b|Example\b)[^\s/\\]+", re.I),
    "workspace_path": re.compile(r"/mnt/[a-z]/Projects/[^\s]+|/home/[^/\s]+/Projects/"),
    "deployment_domain": re.compile(r"https?://[A-Za-z0-9.-]+\.top(?=[/:\s\"']|$)", re.I),
}
PROJECT_ID = re.compile(r"g-p-([0-9a-f]{32})")
EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@([A-Za-z0-9.-]+\.[A-Za-z]{2,})")
PRIVATE_IP = re.compile(r"\b(?:10\.(?:\d{1,3}\.){2}\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3})\b")


def findings(path, data):
    result = []
    p = Path(path)
    if (set(p.parts) & BLOCKED_DIRS or p.name in BLOCKED_NAMES or p.suffix.lower() in BLOCKED_SUFFIXES
            or p.name.startswith(".session-") or ".sqlite" in p.name
            or p.name == ".env" or p.name.startswith(".env.") and not p.name.endswith((".example", ".sample", ".template"))):
        result.append({"path": path, "line": 0, "category": "forbidden_artifact"})
    try:
        source = data.decode("utf-8")
    except UnicodeDecodeError:
        if p.suffix.lower() not in {".png", ".webp", ".ico"}:
            result.append({"path": path, "line": 0, "category": "unexpected_binary"})
        return result
    fixture = p.name.endswith("_test.go") or p.name.startswith("test_") or "testdata" in p.parts or "tests" in p.parts
    notice = "LICENSE" in p.name.upper() or "NOTICE" in p.name.upper()
    for number, line in enumerate(source.splitlines(), 1):
        categories = [label for label, pattern in PATTERNS.items() if pattern.search(line)]
        if any(len(set(m.group(1))) > 1 and m.group(1) != "0123456789abcdef" * 2 for m in PROJECT_ID.finditer(line)):
            categories.append("non_synthetic_project_id")
        if not notice and any(not (m.group(1).endswith((".invalid", ".test", ".example", ".example.com", ".example.org", ".example.net"))
                                     or m.group(1) in {"example.com", "example.org", "example.net", "users.noreply.github.com"}
                                     or fixture and m.group(1) == "fixture.chatgpt.site" and line[max(0, m.start()-8):m.start()] == "https://")
                              for m in EMAIL.finditer(line)):
            categories.append("non_example_email")
        if not fixture and any(not line[m.start():].startswith(("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")) for m in PRIVATE_IP.finditer(line)):
            categories.append("private_network_literal")
        result.extend({"path": path, "line": number, "category": category} for category in categories)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--staged", action="store_true", help="check only added/modified staged paths")
    args = parser.parse_args()
    command = ["git", "diff", "--cached", "--name-only", "--diff-filter=ACMR", "-z"] if args.staged else ["git", "ls-files", "--cached", "-z"]
    paths = subprocess.check_output(command, cwd=ROOT).decode().split("\0")
    paths = [path for path in paths if path]
    issues = []
    for path in paths:
        data = subprocess.check_output(["git", "show", ":" + path], cwd=ROOT)
        issues.extend(findings(path, data))
    print(json.dumps({"checked_files": len(paths), "findings": issues}, ensure_ascii=False, indent=2))
    return int(bool(issues))


if __name__ == "__main__":
    raise SystemExit(main())
