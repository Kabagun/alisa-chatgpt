"""Download and verify task-local Linux build tools, without system installs."""
import hashlib
import json
from pathlib import Path
import shutil
import tarfile
import urllib.request
import zipfile

ROOT = Path(__file__).resolve().parent
DOWNLOADS = ROOT / "downloads"
TOOLS = ROOT / "tools"
DOWNLOADS.mkdir(exist_ok=True)
TOOLS.mkdir(exist_ok=True)
records = []


def metadata(url):
    with urllib.request.urlopen(url, timeout=40) as response:
        return json.load(response)


def download(name, url, sha256):
    target = DOWNLOADS / name
    if not target.is_file() or hashlib.sha256(target.read_bytes()).hexdigest() != sha256:
        partial = target.with_suffix(target.suffix + ".partial")
        with urllib.request.urlopen(url, timeout=60) as response, partial.open("wb") as output:
            shutil.copyfileobj(response, output, 1 << 20)
        if hashlib.sha256(partial.read_bytes()).hexdigest() != sha256:
            raise RuntimeError("download checksum mismatch")
        partial.replace(target)
    records.append({"filename": name, "url": url, "sha256": sha256, "size": target.stat().st_size})
    print("VERIFIED", name, flush=True)
    return target


go_release = next(item for item in metadata("https://go.dev/dl/?mode=json&include=all") if item["version"] == "go1.27.3")
go_file = next(item for item in go_release["files"] if item["os"] == "linux" and item["arch"] == "amd64" and item["kind"] == "archive")
go_archive = download(go_file["filename"], "https://go.dev/dl/" + go_file["filename"], go_file["sha256"])
if not (TOOLS / "go/bin/go").is_file():
    with tarfile.open(go_archive) as archive:
        archive.extractall(TOOLS, filter="data")

uv_meta = metadata("https://pypi.org/pypi/uv/0.13.6/json")
uv_file = next(item for item in uv_meta["urls"] if item["packagetype"] == "bdist_wheel" and "x86_64" in item["filename"] and "manylinux" in item["filename"])
uv_wheel = download(uv_file["filename"], uv_file["url"], uv_file["digests"]["sha256"])
with zipfile.ZipFile(uv_wheel) as archive:
    name = next(name for name in archive.namelist() if name.endswith(".data/scripts/uv"))
    uv = TOOLS / "uv"
    uv.write_bytes(archive.read(name))
    uv.chmod(0o755)

zig_meta = metadata("https://pypi.org/pypi/ziglang/0.17.0/json")
zig_file = next(item for item in zig_meta["urls"] if item["packagetype"] == "bdist_wheel" and "x86_64" in item["filename"] and "manylinux" in item["filename"])
download(zig_file["filename"], zig_file["url"], zig_file["digests"]["sha256"])
(ROOT / "verified-downloads.json").write_text(json.dumps(records, indent=2) + "\n")
print("BOOTSTRAP_ARTIFACTS_READY", flush=True)
