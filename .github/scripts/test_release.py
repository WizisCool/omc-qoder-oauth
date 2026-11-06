"""Offline regression tests for identity gates and deterministic release packaging."""
import copy
import importlib.util
import json
import os
import subprocess
import tempfile
import unittest
from hashlib import sha256
from pathlib import Path
from unittest.mock import patch
from zipfile import ZipFile

import release_metadata as metadata

spec = importlib.util.spec_from_file_location("package_release", Path(__file__).with_name("package-release.py"))
package_release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(package_release)


def build_info(version, goos="linux", goarch="arm64"):
    return f'''plugin: go1.27.1
\tbuild\t-buildmode=c-shared
\tbuild\t-ldflags="-s -w -X main.pluginVersion={version}"
\tbuild\tCGO_ENABLED=1
\tbuild\tGOOS={goos}
\tbuild\tGOARCH={goarch}
'''


class ReleaseMetadataTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.version, self.raw = metadata.read_metadata()
        self.descriptor = json.loads(self.raw)
        (self.root / "go/assets").mkdir(parents=True)
        (self.root / "go/dispatch.go").write_text(f'var pluginVersion = "{self.version}"\n', encoding="utf-8")
        (self.root / "go/assets/omc.json").write_bytes(self.raw)
        self.registry = {"plugins": [{"id": metadata.PLUGIN_ID, "version": self.version}]}
        self.write_registry()

    def write_descriptor(self, descriptor):
        (self.root / "go/assets/omc.json").write_text(json.dumps(descriptor), encoding="utf-8")

    def write_registry(self):
        (self.root / "registry.json").write_text(json.dumps(self.registry), encoding="utf-8")

    def test_current_release_source_and_tag_agree(self):
        self.assertEqual(metadata.read_metadata(self.root, version=self.version, tag="v" + self.version), (self.version, self.raw))

    def test_build_version_override_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "build version"):
            metadata.read_metadata(self.root, version="0.0.0-dev")

    def test_wrong_tag_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "release tag"):
            metadata.read_metadata(self.root, tag="v0.0.0")

    def test_registry_version_drift_is_rejected(self):
        self.registry["plugins"][0]["version"] = "0.0.0"
        self.write_registry()
        with self.assertRaisesRegex(ValueError, "registry version"):
            metadata.read_metadata(self.root)

    def test_default_link_variable_drift_is_rejected(self):
        (self.root / "go/dispatch.go").write_text('var pluginVersion = "0.0.0-dev"\n', encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "default pluginVersion"):
            metadata.read_metadata(self.root)

    def test_additional_runtime_or_menu_fields_are_rejected(self):
        for field in ["token", "credentials", "menu", "settings", "actions", "min_cpa_version"]:
            with self.subTest(field=field):
                descriptor = copy.deepcopy(self.descriptor)
                descriptor[field] = "synthetic-unapproved-value"
                self.write_descriptor(descriptor)
                with self.assertRaisesRegex(ValueError, "unexpected descriptor fields"):
                    metadata.read_metadata(self.root)

    def test_locale_and_description_shapes_are_bounded(self):
        for descriptions in [{"en": "only English"}, {**self.descriptor["i18n"]["description"], "other": "extra"},
                             {**self.descriptor["i18n"]["description"], "ms": ""},
                             {**self.descriptor["i18n"]["description"], "en": {"token": "synthetic"}}]:
            descriptor = copy.deepcopy(self.descriptor)
            descriptor["i18n"]["description"] = descriptions
            self.write_descriptor(descriptor)
            with self.assertRaises(ValueError):
                metadata.read_metadata(self.root)

    def test_invalid_identity_schema_and_inexact_versions_are_rejected(self):
        for field, value in [("plugin_id", "other"), ("schema_version", True), ("schema_version", 2),
                             ("plugin_version", "v0.1.4"), ("plugin_version", "0.0.0-dev"), ("plugin_version", "01.1.4")]:
            with self.subTest(field=field, value=value):
                descriptor = copy.deepcopy(self.descriptor)
                descriptor[field] = value
                self.write_descriptor(descriptor)
                with self.assertRaises(ValueError):
                    metadata.read_metadata(self.root)

    def test_duplicate_json_fields_are_rejected(self):
        (self.root / "go/assets/omc.json").write_bytes(self.raw.replace(b'"schema_version": 1', b'"schema_version": 2, "schema_version": 1'))
        with self.assertRaisesRegex(ValueError, "duplicate JSON"):
            metadata.read_metadata(self.root)

    def test_binary_must_contain_the_exact_raw_descriptor(self):
        library = self.root / "plugin.so"
        library.write_bytes(b"synthetic native file without descriptor")
        with self.assertRaisesRegex(ValueError, "embedded descriptor"):
            metadata.verify_library(library, self.raw, self.version)

    def test_actual_link_flags_must_match_not_just_default_source(self):
        library = self.root / "plugin.so"
        library.write_bytes(b"synthetic-native\x00" + self.raw)
        for linked in ["0.0.0-dev", self.version + "0"]:
            with self.subTest(linked=linked), patch.object(metadata.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, build_info(linked))):
                with self.assertRaisesRegex(ValueError, "linker pluginVersion"):
                    metadata.verify_library(library, self.raw, self.version)
        with patch.object(metadata.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, build_info(self.version))):
            metadata.verify_library(library, self.raw, self.version, "linux", "arm64")
            with self.assertRaisesRegex(ValueError, "build target"):
                metadata.verify_library(library, self.raw, self.version, "linux", "amd64")

    def test_trimpath_build_info_is_not_mistaken_for_linker_proof(self):
        library = self.root / "plugin.so"
        library.write_bytes(self.raw)
        info = build_info(self.version).replace(f'\tbuild\t-ldflags="-s -w -X main.pluginVersion={self.version}"\n', "")
        with patch.object(metadata.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, info)):
            metadata.verify_library(library, self.raw, self.version)
        with self.assertRaisesRegex(ValueError, "linker pluginVersion"):
            metadata.verify_ldflags("-s -w", self.version)
        with self.assertRaisesRegex(ValueError, "linker pluginVersion"):
            metadata.verify_ldflags("-s -w -X main.pluginVersion=0.0.0-dev", self.version)
        metadata.verify_ldflags(f"-s -w -X main.pluginVersion={self.version}", self.version)

    def test_zip_is_deterministic_single_library_with_separate_description(self):
        library = self.root / f"{metadata.PLUGIN_ID}.so"
        library.write_bytes(b"synthetic-native\x00" + self.raw)
        archive = self.root / f"{metadata.PLUGIN_ID}_{self.version}_linux_arm64.zip"
        checksum = archive.with_name(archive.name + ".sha256")
        descriptor = self.root / f"{metadata.PLUGIN_ID}.omc.json"
        with patch.object(metadata.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, build_info(self.version))):
            package_release.package(library, archive, checksum, descriptor, self.version)
            first = archive.read_bytes()
            os.utime(library, (100, 100))
            library.chmod(0o600)
            package_release.package(library, archive, checksum, descriptor, self.version)
            self.assertEqual(first, archive.read_bytes())
        with ZipFile(archive) as zipped:
            self.assertEqual(zipped.namelist(), [library.name])
            self.assertEqual(zipped.read(library.name), library.read_bytes())
            self.assertEqual(zipped.getinfo(library.name).date_time, (2020, 1, 1, 0, 0, 0))
            self.assertEqual(zipped.getinfo(library.name).external_attr >> 16, 0o100755)
        self.assertEqual(descriptor.read_bytes(), self.raw)
        self.assertEqual(checksum.read_text(), f"{sha256(first).hexdigest()}  {archive.name}\n")
        metadata.verify_checksum(descriptor)

    def test_release_distribution_requires_all_targets_and_matching_bytes(self):
        descriptor = self.root / f"{metadata.PLUGIN_ID}.omc.json"
        for (goos, goarch), ext in metadata.TARGETS.items():
            library = self.root / f"{metadata.PLUGIN_ID}.{ext}"
            library.write_bytes(b"synthetic-native\x00" + self.raw)
            archive = self.root / f"{metadata.PLUGIN_ID}_{self.version}_{goos}_{goarch}.zip"
            with patch.object(metadata.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, build_info(self.version, goos, goarch))):
                package_release.package(library, archive, archive.with_name(archive.name + ".sha256"), descriptor, self.version)
        with patch.object(metadata, "verify_library") as verify:
            metadata.verify_distribution(self.root, self.raw, self.version)
            self.assertEqual(verify.call_count, 6)
            descriptor.write_bytes(self.raw + b"\n")
            metadata.write_checksum(descriptor)
            with self.assertRaisesRegex(ValueError, "attachment differs"):
                metadata.verify_distribution(self.root, self.raw, self.version)
            descriptor.write_bytes(self.raw)
            metadata.write_checksum(descriptor)
            archive.unlink()
            with self.assertRaisesRegex(ValueError, "six expected platform archives"):
                metadata.verify_distribution(self.root, self.raw, self.version)

    def test_checksum_drift_is_rejected(self):
        asset = self.root / "public-asset"
        asset.write_bytes(b"public bytes")
        metadata.write_checksum(asset)
        metadata.verify_checksum(asset)
        asset.write_bytes(b"changed public bytes")
        with self.assertRaisesRegex(ValueError, "checksum sidecar"):
            metadata.verify_checksum(asset)


if __name__ == "__main__":
    unittest.main()
