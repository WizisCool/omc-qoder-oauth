"""Exercise release interruption/retry safety offline, without a GitHub mutation."""
import copy
import json
import subprocess
import tempfile
import unittest
from hashlib import sha256
from pathlib import Path
from unittest.mock import patch

import publish_release as publisher


class FakeGitHub:
    def __init__(self, release=None):
        self.state = copy.deepcopy(release)
        self.calls = []
        self.fail_upload = False
        self.fail_publish = False
        self.noop_publish = False
        self.noop_upload = False
        self.downloads = {}

    def release(self, tag):
        self.calls.append(("read", tag))
        return copy.deepcopy(self.state)

    def create_draft(self, tag):
        self.calls.append(("create", tag))
        self.state = {"id": 1, "tag_name": tag, "draft": True, "assets": [], "html_url": "https://github.com/test/test/releases/tag/" + tag}

    def upload(self, tag, paths):
        self.calls.append(("upload", [p.name for p in paths]))
        if self.noop_upload:
            return
        for path in paths:
            raw = path.read_bytes()
            self.state["assets"].append({"id": len(self.state["assets"]) + 1, "name": path.name, "state": "uploaded", "size": len(raw), "digest": "sha256:" + sha256(raw).hexdigest()})
            if self.fail_upload:
                raise RuntimeError("synthetic interrupted partial upload")

    def publish(self, tag):
        self.calls.append(("publish", tag))
        if self.fail_publish:
            raise RuntimeError("synthetic publish failure")
        if not self.noop_publish:
            self.state["draft"] = False

    def download(self, asset):
        self.calls.append(("download", asset["name"]))
        return self.downloads[asset["name"]]


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.paths = {}
        self.tag = "v0.1.4"
        for name in ["test.zip", "test.omc.json", "checksums.txt"]:
            path = Path(self.temp.name) / name
            path.write_bytes(("public release bytes: " + name).encode())
            self.paths[name] = path

    def existing(self, count=0, draft=True):
        client = FakeGitHub()
        client.create_draft(self.tag)
        client.upload(self.tag, list(self.paths.values())[:count])
        client.state["draft"] = draft
        client.calls.clear()
        return client

    def test_new_release_is_explicit_draft_then_verified_publication(self):
        client = FakeGitHub()
        result = publisher.publish(client, self.tag, self.paths)
        self.assertFalse(result["draft"])
        self.assertEqual([op for op, _ in client.calls], ["read", "create", "read", "upload", "read", "publish", "read"])

    def test_empty_draft_is_completed_and_published(self):
        client = self.existing()
        self.assertFalse(publisher.publish(client, self.tag, self.paths)["draft"])
        self.assertIn(("publish", self.tag), client.calls)
        self.assertNotIn(("create", self.tag), client.calls)

    def test_partial_draft_uploads_only_missing_and_publishes(self):
        client = self.existing(count=1)
        self.assertFalse(publisher.publish(client, self.tag, self.paths)["draft"])
        uploaded = next(value for op, value in client.calls if op == "upload")
        self.assertEqual(set(uploaded), set(self.paths) - {"test.zip"})

    def test_complete_draft_is_published_without_uploads(self):
        client = self.existing(count=3)
        self.assertFalse(publisher.publish(client, self.tag, self.paths)["draft"])
        self.assertFalse(any(op == "upload" for op, _ in client.calls))

    def test_published_complete_identical_rerun_has_no_mutations(self):
        client = self.existing(count=3, draft=False)
        self.assertFalse(publisher.publish(client, self.tag, self.paths)["draft"])
        self.assertEqual([op for op, _ in client.calls], ["read"])

    def test_published_missing_assets_refuses_repair(self):
        client = self.existing(count=1, draft=False)
        with self.assertRaisesRegex(ValueError, "immutable releases cannot be repaired"):
            publisher.publish(client, self.tag, self.paths)
        self.assertEqual([op for op, _ in client.calls], ["read"])

    def test_conflicting_same_name_refuses_draft_or_published_overwrite(self):
        for draft in [True, False]:
            with self.subTest(draft=draft):
                client = self.existing(count=3, draft=draft)
                client.state["assets"][0]["digest"] = "sha256:" + "0" * 64
                with self.assertRaisesRegex(ValueError, "immutable remote asset bytes conflict"):
                    publisher.publish(client, self.tag, self.paths)
                self.assertEqual([op for op, _ in client.calls], ["read"])

    def test_missing_digest_requires_actual_download_matching_bytes(self):
        client = self.existing(count=3, draft=False)
        client.state["assets"][0]["digest"] = None
        client.downloads["test.zip"] = self.paths["test.zip"].read_bytes()
        self.assertFalse(publisher.publish(client, self.tag, self.paths)["draft"])
        self.assertIn(("download", "test.zip"), client.calls)
        client.downloads["test.zip"] = b"x" * len(self.paths["test.zip"].read_bytes())
        with self.assertRaisesRegex(ValueError, "immutable remote asset bytes conflict"):
            publisher.publish(client, self.tag, self.paths)

    def test_upload_failure_is_non_success_and_retry_resumes_missing_only(self):
        client = self.existing()
        client.fail_upload = True
        with self.assertRaisesRegex(RuntimeError, "interrupted partial upload"):
            publisher.publish(client, self.tag, self.paths)
        self.assertTrue(client.state["draft"])
        self.assertFalse(any(op == "publish" for op, _ in client.calls))
        client.fail_upload = False
        already_present = client.state["assets"][0]["name"]
        client.calls.clear()
        self.assertFalse(publisher.publish(client, self.tag, self.paths)["draft"])
        uploaded = next(value for op, value in client.calls if op == "upload")
        self.assertNotIn(already_present, uploaded)

    def test_publish_failure_is_non_success_and_retry_never_reuploads(self):
        client = self.existing(count=3)
        client.fail_publish = True
        with self.assertRaisesRegex(RuntimeError, "publish failure"):
            publisher.publish(client, self.tag, self.paths)
        self.assertTrue(client.state["draft"])
        client.fail_publish = False
        self.assertFalse(publisher.publish(client, self.tag, self.paths)["draft"])
        self.assertFalse(any(op == "upload" for op, _ in client.calls))

    def test_upload_success_but_missing_asset_cannot_publish(self):
        client = self.existing()
        client.noop_upload = True
        with self.assertRaisesRegex(ValueError, "draft asset set is incomplete"):
            publisher.publish(client, self.tag, self.paths)
        self.assertFalse(any(op == "publish" for op, _ in client.calls))

    def test_edit_success_but_still_draft_cannot_report_success(self):
        client = self.existing(count=3)
        client.noop_publish = True
        with self.assertRaisesRegex(ValueError, "publication did not complete"):
            publisher.publish(client, self.tag, self.paths)

    def test_unknown_asset_or_changed_identity_refuses_any_mutation(self):
        client = self.existing(count=1)
        client.state["assets"][0]["name"] = "unexpected.bin"
        with self.assertRaisesRegex(ValueError, "unexpected or duplicate"):
            publisher.publish(client, self.tag, self.paths)
        self.assertEqual([op for op, _ in client.calls], ["read"])
        client.state["tag_name"] = "v0.1.3"
        with self.assertRaisesRegex(ValueError, "unexpected identity/state"):
            publisher.publish(client, self.tag, self.paths)

    def test_lookup_failure_is_not_mistaken_for_missing_release(self):
        gh = publisher.GitHub("test/test")
        with patch.object(publisher.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", "gh: failure (HTTP 403)")):
            with self.assertRaisesRegex(RuntimeError, "refusing to infer absence"):
                gh.release(self.tag)
        with patch.object(publisher.subprocess, "run", side_effect=[
            subprocess.CompletedProcess([], 1, "", "gh: Not Found (HTTP 404)"),
            subprocess.CompletedProcess([], 0, json.dumps({"data": {"repository": {"release": None}}}), ""),
        ]):
            self.assertIsNone(gh.release(self.tag))

    def test_gh_commands_are_draft_first_no_clobber_and_repository_scoped(self):
        gh = publisher.GitHub("test/test")
        with patch.object(publisher.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"public")) as run:
            gh.create_draft(self.tag)
            self.assertIn("--draft", run.call_args.args[0])
            self.assertIn("--verify-tag", run.call_args.args[0])
            gh.upload(self.tag, list(self.paths.values()))
            self.assertNotIn("--clobber", run.call_args.args[0])
            gh.publish(self.tag)
            self.assertIn("--draft=false", run.call_args.args[0])
            self.assertEqual(gh.download({"id": 12}), b"public")
            self.assertIn("repos/test/test/releases/assets/12", run.call_args.args[0])


class AdapterTests(unittest.TestCase):
    """Use the real GitHub adapter and actual gh argument lists, not FakeGitHub.release."""
    def setUp(self):
        self.gh = publisher.GitHub("test/test")
        self.tag = "v0.1.4"
        self.state = None
        self.calls = []

    def gh_stub(self, args, **kwargs):
        self.calls.append(args)
        if args == ["gh", "api", "repos/test/test/releases/tags/" + self.tag]:
            # Match the real REST semantics: even an existing draft is 404.
            if self.state is None or self.state["draft"]:
                return subprocess.CompletedProcess(args, 1, "", "gh: Not Found (HTTP 404)")
            return subprocess.CompletedProcess(args, 0, json.dumps(self.state), "")
        if args[:3] == ["gh", "api", "graphql"]:
            self.assertTrue(kwargs["check"])
            self.assertEqual(args[3::2], ["-f"] * 4)
            fields = dict(field.split("=", 1) for field in args[4::2])
            self.assertEqual(fields["owner"], "test")
            self.assertEqual(fields["name"], "test")
            self.assertEqual(fields["tagName"], self.tag)
            self.assertIn("release(tagName:$tagName){databaseId}", fields["query"])
            draft = {"databaseId": self.state["id"]} if self.state else None
            return subprocess.CompletedProcess(args, 0, json.dumps({"data": {"repository": {"release": draft}}}), "")
        if args == ["gh", "api", "repos/test/test/releases/12"]:
            self.assertTrue(kwargs["check"])
            self.assertIsNotNone(self.state)
            return subprocess.CompletedProcess(args, 0, json.dumps(self.state), "")
        if args[:4] == ["gh", "release", "create", self.tag]:
            self.assertIsNone(self.state, "an existing draft must not be created a second time")
            self.assertIn("--draft", args)
            self.state = {"id": 12, "tag_name": self.tag, "draft": True, "assets": [], "html_url": "https://github.com/test/test/releases/tag/" + self.tag}
            return subprocess.CompletedProcess(args, 0)
        if args[:4] == ["gh", "release", "edit", self.tag]:
            self.assertIn("--draft=false", args)
            self.state["draft"] = False
            return subprocess.CompletedProcess(args, 0)
        self.fail("unexpected gh command: " + repr(args))

    def test_rest_tag_404_existing_draft_resolves_by_graphql_then_rest_id(self):
        self.state = {"id": 12, "tag_name": self.tag, "draft": True, "assets": [{"name": "existing.zip", "digest": "sha256:synthetic"}]}
        with patch.object(publisher.subprocess, "run", side_effect=self.gh_stub):
            self.assertEqual(self.gh.release(self.tag), self.state)
        self.assertEqual(len(self.calls), 3)
        self.assertEqual(self.calls[-1], ["gh", "api", "repos/test/test/releases/12"])

    def test_new_draft_can_be_read_back_while_rest_tag_remains_404(self):
        with patch.object(publisher.subprocess, "run", side_effect=self.gh_stub):
            self.assertIsNone(self.gh.release(self.tag))
            self.gh.create_draft(self.tag)
            self.assertTrue(self.gh.release(self.tag)["draft"])
            self.assertTrue(self.gh.release(self.tag)["draft"])
        self.assertEqual(sum(args[:3] == ["gh", "release", "create"] for args in self.calls), 1)

    def test_actual_adapter_state_machine_reads_created_draft_and_publishes(self):
        with patch.object(publisher.subprocess, "run", side_effect=self.gh_stub):
            self.assertFalse(publisher.publish(self.gh, self.tag, {})["draft"])
        self.assertEqual(self.calls[-1], ["gh", "api", "repos/test/test/releases/tags/" + self.tag])
        self.assertEqual(sum(args[:3] == ["gh", "release", "create"] for args in self.calls), 1)

    def test_published_lookup_uses_rest_without_draft_query(self):
        self.state = {"id": 12, "tag_name": self.tag, "draft": False, "assets": []}
        with patch.object(publisher.subprocess, "run", side_effect=self.gh_stub):
            self.assertEqual(self.gh.release(self.tag), self.state)
        self.assertEqual(len(self.calls), 1)

    def test_only_confirmed_rest_missing_and_graphql_null_means_absence(self):
        with patch.object(publisher.subprocess, "run", side_effect=self.gh_stub):
            self.assertIsNone(self.gh.release(self.tag))
        self.assertEqual(len(self.calls), 2)

    def test_graphql_errors_or_missing_repository_never_mean_absence(self):
        missing = subprocess.CompletedProcess([], 1, "", "gh: Not Found (HTTP 404)")
        for payload in [{"errors": [{"message": "synthetic denied"}], "data": {"repository": None}}, {"data": {"repository": None}}]:
            with self.subTest(payload=payload), patch.object(publisher.subprocess, "run", side_effect=[missing, subprocess.CompletedProcess([], 0, json.dumps(payload), "")]):
                with self.assertRaisesRegex(RuntimeError, "draft lookup failed"):
                    self.gh.release(self.tag)

    def test_graphql_transport_and_rest_id_failures_do_not_create_releases(self):
        missing = subprocess.CompletedProcess([], 1, "", "gh: Not Found (HTTP 404)")
        found = subprocess.CompletedProcess([], 0, json.dumps({"data": {"repository": {"release": {"databaseId": 12}}}}), "")
        for responses in [[missing, subprocess.CalledProcessError(1, ["gh", "api", "graphql"])],
                          [missing, found, subprocess.CalledProcessError(1, ["gh", "api", "repos/test/test/releases/12"])]]:
            with self.subTest(responses=responses), patch.object(publisher.subprocess, "run", side_effect=responses) as run:
                with self.assertRaises(subprocess.CalledProcessError):
                    publisher.publish(self.gh, self.tag, {})
                self.assertFalse(any(call.args[0][:3] == ["gh", "release", "create"] for call in run.call_args_list))


if __name__ == "__main__":
    unittest.main()
