#!/usr/bin/env python3
"""Validate build-bound release identities and public descriptor byte equality."""
import json
import re
import shlex
import subprocess
import tempfile
from argparse import ArgumentParser
from hashlib import sha256
from pathlib import Path
from zipfile import ZipFile

PLUGIN_ID = "omc-qoder-oauth"
ROOT = Path(__file__).resolve().parents[2]
TARGETS = {
    ("linux", "amd64"): "so",
    ("linux", "arm64"): "so",
    ("darwin", "amd64"): "dylib",
    ("darwin", "arm64"): "dylib",
    ("windows", "amd64"): "dll",
    ("windows", "arm64"): "dll",
}


def unique_object(pairs):
    obj = {}
    for key, value in pairs:
        if key in obj:
            raise ValueError(f"duplicate JSON field: {key}")
        obj[key] = value
    return obj


def read_metadata(root=ROOT, version=None, tag=None):
    raw = (root / "go/assets/omc.json").read_bytes()
    if len(raw) > 32768:
        raise ValueError("descriptor exceeds the public document size limit")
    descriptor = json.loads(raw.decode("utf-8"), object_pairs_hook=unique_object)
    if set(descriptor) != {"schema_version", "plugin_id", "plugin_version", "i18n"}:
        raise ValueError("unexpected descriptor fields: only public v1 documentation is allowed")
    if type(descriptor["schema_version"]) is not int or descriptor["schema_version"] != 1:
        raise ValueError("descriptor schema_version must be 1")
    if descriptor["plugin_id"] != PLUGIN_ID:
        raise ValueError("descriptor plugin_id differs from the native plugin identity")
    actual_version = descriptor["plugin_version"]
    if not isinstance(actual_version, str) or not re.fullmatch(r"(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)", actual_version):
        raise ValueError("descriptor must have an exact release version X.Y.Z")
    if version is not None and version != actual_version:
        raise ValueError("build version differs from descriptor plugin_version")
    if tag is not None and tag != "v" + actual_version:
        raise ValueError("release tag differs from descriptor plugin_version")
    i18n = descriptor["i18n"]
    if not isinstance(i18n, dict) or set(i18n) != {"description"}:
        raise ValueError("descriptor i18n must contain only public descriptions")
    descriptions = i18n["description"]
    if not isinstance(descriptions, dict) or set(descriptions) != {"zh", "en", "zh-Hant", "ms"}:
        raise ValueError("descriptor must contain the four approved description locales")
    if any(not isinstance(text, str) or not text.strip() for text in descriptions.values()):
        raise ValueError("localized descriptions must be nonempty public text")
    registry = json.loads((root / "registry.json").read_text(encoding="utf-8"), object_pairs_hook=unique_object)
    entries = [entry for entry in registry["plugins"] if entry["id"] == PLUGIN_ID]
    if len(entries) != 1 or entries[0]["version"] != actual_version:
        raise ValueError("registry version differs from descriptor plugin_version")
    dispatch = (root / "go/dispatch.go").read_text(encoding="utf-8")
    default_version = re.search(r'^var pluginVersion = "([^"\n]+)"$', dispatch, re.MULTILINE)
    if default_version is None or default_version[1] != actual_version:
        raise ValueError("default pluginVersion differs from descriptor plugin_version")
    return actual_version, raw


def verify_ldflags(flags, version):
    tokens = iter(shlex.split(flags))
    linked_version = None
    for token in tokens:
        assignment = next(tokens, "") if token == "-X" else token.removeprefix("-X=") if token.startswith("-X=") else ""
        if assignment.startswith("main.pluginVersion="):
            linked_version = assignment.removeprefix("main.pluginVersion=")
    if linked_version != version:
        raise ValueError("linker pluginVersion differs from release version")


def verify_library(library, descriptor, version, goos=None, goarch=None):
    if descriptor not in library.read_bytes():
        raise ValueError(f"{library.name}: embedded descriptor bytes differ from release source")
    info = subprocess.run(["go", "version", "-m", str(library)], check=True, capture_output=True, text=True).stdout
    settings = dict(re.findall(r"^\s*build\s+(\S+?)=(.*)$", info, re.MULTILINE))
    if settings.get("-buildmode") != "c-shared" or settings.get("CGO_ENABLED") != "1":
        raise ValueError(f"{library.name}: not a cgo shared-library build")
    if (goos is not None and settings.get("GOOS") != goos) or (goarch is not None and settings.get("GOARCH") != goarch):
        raise ValueError(f"{library.name}: build target differs from platform archive")
    # Go omits -ldflags in trimpath builds. Validate the actual linker input
    # before building; native ABI smoke tests verify the resulting value too.
    if "-ldflags" in settings:
        flags = settings["-ldflags"]
        verify_ldflags(json.loads(flags) if flags.startswith('"') else flags, version)


def write_checksum(path):
    digest = sha256(path.read_bytes()).hexdigest()
    path.with_name(path.name + ".sha256").write_text(f"{digest}  {path.name}\n", encoding="utf-8")


def verify_checksum(path):
    expected = f"{sha256(path.read_bytes()).hexdigest()}  {path.name}\n"
    if path.with_name(path.name + ".sha256").read_text(encoding="utf-8") != expected:
        raise ValueError(f"{path.name}: checksum sidecar differs from file bytes")


def verify_distribution(directory, descriptor, version):
    attachment = directory / f"{PLUGIN_ID}.omc.json"
    if attachment.read_bytes() != descriptor:
        raise ValueError("release attachment differs from embedded descriptor source")
    verify_checksum(attachment)
    expected_archives = {f"{PLUGIN_ID}_{version}_{goos}_{goarch}.zip" for goos, goarch in TARGETS}
    if {path.name for path in directory.glob("*.zip")} != expected_archives:
        raise ValueError("release must contain exactly the six expected platform archives")
    for (goos, goarch), ext in TARGETS.items():
        archive = directory / f"{PLUGIN_ID}_{version}_{goos}_{goarch}.zip"
        verify_checksum(archive)
        name = f"{PLUGIN_ID}.{ext}"
        with ZipFile(archive) as zipped:
            if zipped.namelist() != [name]:
                raise ValueError(f"{archive.name}: ZIP must contain only the native library")
            entry = zipped.getinfo(name)
            if entry.date_time != (2020, 1, 1, 0, 0, 0) or entry.external_attr >> 16 != 0o100755:
                raise ValueError(f"{archive.name}: nondeterministic library timestamp or mode")
            with tempfile.TemporaryDirectory() as temp:
                library = Path(temp) / name
                library.write_bytes(zipped.read(name))
                verify_library(library, descriptor, version, goos, goarch)
    expected_checksums = {name + ".sha256" for name in expected_archives} | {attachment.name + ".sha256"}
    if {path.name for path in directory.glob("*.sha256")} != expected_checksums:
        raise ValueError("release has missing or unexpected checksum sidecars")


def main():
    parser = ArgumentParser(description=__doc__)
    parser.add_argument("--version")
    parser.add_argument("--tag")
    parser.add_argument("--ldflags")
    parser.add_argument("--library", type=Path)
    parser.add_argument("--distribution", type=Path)
    args = parser.parse_args()
    version, descriptor = read_metadata(version=args.version, tag=args.tag)
    if args.ldflags is not None:
        verify_ldflags(args.ldflags, version)
    if args.library is not None:
        verify_library(args.library, descriptor, version)
    if args.distribution is not None:
        verify_distribution(args.distribution, descriptor, version)
    print(version)


if __name__ == "__main__":
    main()
