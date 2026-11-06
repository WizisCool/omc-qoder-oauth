#!/usr/bin/env python3
"""Package one native library and a separate, byte-identical OMC declaration."""
from argparse import ArgumentParser
from pathlib import Path
from zipfile import ZIP_DEFLATED, ZipFile, ZipInfo

from release_metadata import PLUGIN_ID, TARGETS, read_metadata, verify_library, write_checksum


def package(library, archive, checksum, descriptor, version):
    _, raw = read_metadata(version=version)
    targets = [(goos, goarch, ext) for (goos, goarch), ext in TARGETS.items()
               if archive.name == f"{PLUGIN_ID}_{version}_{goos}_{goarch}.zip"]
    if len(targets) != 1:
        raise ValueError("archive filename must match the exact release version and platform")
    goos, goarch, ext = targets[0]
    if library.name != f"{PLUGIN_ID}.{ext}":
        raise ValueError("native library filename differs from archive target")
    if checksum != archive.with_name(archive.name + ".sha256"):
        raise ValueError("archive checksum must be its .sha256 sidecar")
    if descriptor.name != f"{PLUGIN_ID}.omc.json":
        raise ValueError("descriptor attachment must use the plugin identity")
    verify_library(library, raw, version, goos, goarch)
    info = ZipInfo(library.name, (2020, 1, 1, 0, 0, 0))
    info.external_attr = 0o100755 << 16
    info.compress_type = ZIP_DEFLATED
    with ZipFile(archive, "w") as output:
        output.writestr(info, library.read_bytes())
    write_checksum(archive)
    descriptor.write_bytes(raw)
    write_checksum(descriptor)
    print(f"{archive.name}: {library.name}")
    print(f"{descriptor.name}: unchanged embedded descriptor source")


def main():
    parser = ArgumentParser(description=__doc__)
    parser.add_argument("--library", required=True, type=Path)
    parser.add_argument("--archive", required=True, type=Path)
    parser.add_argument("--checksum", required=True, type=Path)
    parser.add_argument("--descriptor", required=True, type=Path)
    parser.add_argument("--version", required=True)
    args = parser.parse_args()
    package(args.library, args.archive, args.checksum, args.descriptor, args.version)


if __name__ == "__main__":
    main()
