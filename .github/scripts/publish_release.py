#!/usr/bin/env python3
"""Resume a draft release without overwrites; succeed only after publication."""
import json
import re
import subprocess
from argparse import ArgumentParser
from hashlib import sha256
from pathlib import Path

from release_metadata import PLUGIN_ID, TARGETS, read_metadata, verify_distribution


class GitHub:
    def __init__(self, repository):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise ValueError("invalid GitHub repository")
        self.repository = repository

    def release(self, tag):
        result = subprocess.run(["gh", "api", f"repos/{self.repository}/releases/tags/{tag}"], capture_output=True, text=True)
        if result.returncode == 0:
            return json.loads(result.stdout)
        if "(HTTP 404)" not in result.stderr:
            raise RuntimeError("GitHub release lookup failed; refusing to infer absence")
        # REST-by-tag cannot resolve a draft's pending tag. Follow gh's own
        # draft lookup: GraphQL resolves its database ID, REST returns assets.
        owner, name = self.repository.split("/")
        query = "query($owner:String!,$name:String!,$tagName:String!){repository(owner:$owner,name:$name){release(tagName:$tagName){databaseId}}}"
        result = subprocess.run(["gh", "api", "graphql", "-f", "query=" + query, "-f", "owner=" + owner, "-f", "name=" + name, "-f", "tagName=" + tag], check=True, capture_output=True, text=True)
        payload = json.loads(result.stdout)
        repository = payload.get("data", {}).get("repository")
        if payload.get("errors") or not isinstance(repository, dict) or "release" not in repository:
            raise RuntimeError("GitHub draft lookup failed; refusing to infer absence")
        draft = repository["release"]
        if draft is None:
            return None
        release_id = draft.get("databaseId")
        if type(release_id) is not int or release_id <= 0:
            raise ValueError("invalid draft release database id")
        result = subprocess.run(["gh", "api", f"repos/{self.repository}/releases/{release_id}"], check=True, capture_output=True, text=True)
        return json.loads(result.stdout)

    def create_draft(self, tag):
        subprocess.run(["gh", "release", "create", tag, "--repo", self.repository, "--draft", "--verify-tag", "--title", tag, "--generate-notes"], check=True)

    def upload(self, tag, paths):
        subprocess.run(["gh", "release", "upload", tag, "--repo", self.repository, *map(str, paths)], check=True)

    def publish(self, tag):
        subprocess.run(["gh", "release", "edit", tag, "--repo", self.repository, "--draft=false"], check=True)

    def download(self, asset):
        asset_id = asset.get("id")
        if type(asset_id) is not int or asset_id <= 0:
            raise ValueError("invalid GitHub asset id")
        return subprocess.run(["gh", "api", f"repos/{self.repository}/releases/assets/{asset_id}", "--header", "Accept: application/octet-stream"], check=True, capture_output=True).stdout


def local_assets(directory, version):
    names = {f"{PLUGIN_ID}_{version}_{goos}_{goarch}.zip" for goos, goarch in TARGETS}
    names.add(f"{PLUGIN_ID}.omc.json")
    sidecars = {name + ".sha256" for name in names}
    checksums = "".join(sorted((directory / name).read_text(encoding="utf-8") for name in sidecars))
    (directory / "checksums.txt").write_text(checksums, encoding="utf-8")
    return {name: directory / name for name in sorted(names | sidecars | {"checksums.txt"})}


def check_remote(client, release, tag, paths):
    if release is None or release.get("tag_name") != tag or type(release.get("draft")) is not bool:
        raise ValueError("release disappeared or returned an unexpected identity/state")
    if type(release.get("id")) is not int or release["id"] <= 0:
        raise ValueError("invalid remote release id")
    remote = {}
    for asset in release["assets"]:
        name = asset["name"]
        if name in remote or name not in paths:
            raise ValueError("unexpected or duplicate remote asset; refusing to alter release")
        remote[name] = asset
        local = paths[name].read_bytes()
        if asset.get("state") != "uploaded" or asset.get("size") != len(local):
            raise ValueError(f"{name}: remote asset state/size conflicts with local bytes")
        digest = asset.get("digest")
        if digest is None:
            # Some GitHub uploads have no digest. Null is never proof of equality.
            downloaded = client.download(asset)
            if len(downloaded) != len(local):
                raise ValueError(f"{name}: downloaded asset size conflicts with local bytes")
            digest = "sha256:" + sha256(downloaded).hexdigest()
        if digest != "sha256:" + sha256(local).hexdigest():
            raise ValueError(f"{name}: immutable remote asset bytes conflict with local release")
    return sorted(set(paths) - set(remote))


def publish(client, tag, paths):
    release = client.release(tag)
    if release is None:
        client.create_draft(tag)
        release = client.release(tag)
    missing = check_remote(client, release, tag, paths)
    release_id = release["id"]
    if not release["draft"]:
        if missing:
            raise ValueError("published release is incomplete; immutable releases cannot be repaired")
        return release
    if missing:
        client.upload(tag, [paths[name] for name in missing])
    # A successful upload command is not proof that every asset was accepted.
    release = client.release(tag)
    if check_remote(client, release, tag, paths) or release["id"] != release_id:
        raise ValueError("draft asset set is incomplete or release identity changed")
    if release["draft"]:
        client.publish(tag)
    # A successful edit command is not proof that the draft became public.
    release = client.release(tag)
    if check_remote(client, release, tag, paths) or release["id"] != release_id or release["draft"]:
        raise ValueError("release publication did not complete with the verified asset set")
    return release


def main():
    parser = ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--distribution", required=True, type=Path)
    args = parser.parse_args()
    version, descriptor = read_metadata(tag=args.tag)
    verify_distribution(args.distribution, descriptor, version)
    result = publish(GitHub(args.repository), args.tag, local_assets(args.distribution, version))
    print(f"Published and verified: {result['html_url']}")


if __name__ == "__main__":
    main()
