#!/usr/bin/env python3
"""Check the native ABI and passive resources; trap every host callback offline."""
import base64
import ctypes
import json
from argparse import ArgumentParser
from hashlib import sha256
from pathlib import Path

from release_metadata import PLUGIN_ID, read_metadata

ROOT = Path(__file__).resolve().parents[2]

# The two sign-in entries the panel publishes, in declaration order. Each one
# names its own artwork, so a host renders two distinguishable cards.
OAUTH_ENTRIES = (("qoder", "Qoder", "qoder-color.svg"), ("qoder-cn", "Qoder CN", "qoder.svg"))
CREDENTIAL_PROVIDER = "qoder"
# Served artwork is compared against the embedded sources, so a stale or
# swapped deployment cannot pass.
SVG_ASSETS = (
    ("qoder-color.svg", "image/svg+xml"),
    ("qoder.svg", "image/svg+xml"),
)


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


class HostAPI(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("host_ctx", ctypes.c_void_p), ("call", ctypes.c_void_p), ("free_buffer", ctypes.c_void_p)]


class PluginAPI(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("call", ctypes.c_void_p), ("free_buffer", ctypes.c_void_p), ("shutdown", ctypes.c_void_p)]


def smoke(library, version, descriptor):
    native = ctypes.CDLL(str(library.resolve()))
    native.cliproxy_plugin_init.argtypes = [ctypes.POINTER(HostAPI), ctypes.POINTER(PluginAPI)]
    native.cliproxy_plugin_init.restype = ctypes.c_int
    callbacks = []
    host_call = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p, ctypes.c_size_t, ctypes.POINTER(Buffer))
    host_free = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)

    @host_call
    def trap(_context, method, _request, _length, _response):
        callbacks.append(method.decode("ascii"))
        return 1

    @host_free
    def free_host(_pointer, _length):
        pass

    host = HostAPI(abi_version=1, call=ctypes.cast(trap, ctypes.c_void_p), free_buffer=ctypes.cast(free_host, ctypes.c_void_p))
    plugin = PluginAPI()
    if native.cliproxy_plugin_init(ctypes.byref(host), ctypes.byref(plugin)) != 0 or plugin.abi_version != 1:
        raise ValueError("native plugin initialization failed")
    call = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_char_p, ctypes.c_void_p, ctypes.c_size_t, ctypes.POINTER(Buffer))(plugin.call)
    free = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)(plugin.free_buffer)
    shutdown = ctypes.CFUNCTYPE(None)(plugin.shutdown)

    def rpc(method, request):
        raw = json.dumps(request).encode("utf-8")
        source = ctypes.create_string_buffer(raw)
        response = Buffer()
        rc = call(method.encode("ascii"), source, len(raw), ctypes.byref(response))
        try:
            envelope = json.loads(ctypes.string_at(response.ptr, response.len))
        finally:
            free(response.ptr, response.len)
        if rc != 0 or envelope.get("ok") is not True:
            raise ValueError(f"native {method} failed")
        return envelope["result"]

    try:
        registration = rpc("plugin.register", {})
        if registration["metadata"]["Version"] != version:
            raise ValueError("native registration differs from release version")
        if registration.get("schema_version") != 10:
            raise ValueError("native registration must declare schema 10 for multi-entry OAuth")
        # Two OAuth login entries, each with its own name and logo. A host
        # publishes these as oauth_provider / oauth_providers /
        # oauth_provider_entries, so the shape is checked here rather than only
        # in the offline unit tests.
        auth = rpc("auth.identifier", {})
        if auth.get("identifier") != CREDENTIAL_PROVIDER:
            raise ValueError("native primary OAuth identifier must be qoder")
        if auth.get("identifiers") != ["qoder", "qoder-cn"]:
            raise ValueError("native OAuth identifiers must be [qoder, qoder-cn]")
        credentials = auth.get("credential_providers") or []
        if CREDENTIAL_PROVIDER not in credentials:
            raise ValueError("native credential provider qoder must be declared")
        entries = auth.get("entries") or []
        if [entry.get("identifier") for entry in entries] != [entry_id for entry_id, _, _ in OAUTH_ENTRIES]:
            raise ValueError("native OAuth entries must match the declared identifiers")
        for entry, (_expected_id, expected_name, expected_asset) in zip(entries, OAUTH_ENTRIES):
            if not entry.get("name") or not entry.get("logo"):
                raise ValueError("native OAuth entry is missing a display name or logo")
            if entry["name"] != expected_name or not entry["logo"].endswith("/" + expected_asset):
                raise ValueError(f"native OAuth entry {entry.get('identifier')} has the wrong name or logo")
        if entries[0]["logo"] == entries[1]["logo"]:
            raise ValueError("native OAuth entries must not share one logo")
        for method in ("executor.identifier", "quota.identifier"):
            if rpc(method, {}).get("identifier") != CREDENTIAL_PROVIDER:
                raise ValueError(f"native {method} must stay on the qoder provider key")
        management = rpc("management.register", {})
        resources = management["resources"]
        if [entry["path"] for entry in resources] != ["/qoder-color.svg", "/qoder.svg", "/omc.json"] or any(entry["menu"] for entry in resources) or management.get("routes"):
            raise ValueError("native public resources must be exactly three passive assets with no menu")
        prefix = f"/v0/resource/plugins/{PLUGIN_ID}"
        assets = {name: ((ROOT / "go" / "assets" / name).read_bytes(), content_type) for name, content_type in SVG_ASSETS}
        assets["omc.json"] = (descriptor, "application/json; charset=utf-8")
        probes = 0
        for name, (expected, content_type) in assets.items():
            path = f"{prefix}/{name}"
            for query in ({}, {"state": ["forged-state"], "site": ["qoder"]}, {"state": ["other-state"], "site": ["qoder-cn", "qoder"]}):
                response = rpc("management.handle", {"Method": "GET", "Path": path, "Query": query, "Headers": {"Authorization": ["Bearer synthetic"]}, "Body": base64.b64encode(b"synthetic body").decode(), "host_callback_id": "untrusted-callback"})
                if response["StatusCode"] != 200 or response["Headers"].get("Content-Type") != [content_type] or response["Headers"].get("Location") or base64.b64decode(response["Body"], validate=True) != expected:
                    raise ValueError(f"native static resource failed: {name}")
                probes += 1
            for method in ("", "get", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "TRACE", "CONNECT", "UNKNOWN"):
                response = rpc("management.handle", {"Method": method, "Path": path})
                if response["StatusCode"] != 405 or response["Headers"].get("Allow") != ["GET"] or response["Headers"].get("Location"):
                    raise ValueError("native static resource accepted a non-GET method")
                probes += 1
        for name in (*assets, "qoder-color.SVG", "oauth-select", "oauth-start"):
            path = f"{prefix}/{name}"
            paths = [path+"/", path+".bak", path+"?state=inline", "/foreign"+path, f"{prefix}/subdir/{name}", f"{prefix}/../{name}", f"{prefix}/%2e/{name}", f"/v0/resource/plugins/other/{name}"]
            if name.startswith("oauth-"):
                paths.append(path)
            for variant in paths:
                for method in ("", "get", "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "TRACE", "CONNECT", "UNKNOWN"):
                    response = rpc("management.handle", {"Method": method, "Path": variant, "Query": {"state": ["forged-state"], "site": ["qoder-cn"]}, "host_callback_id": "untrusted-callback"})
                    if response["StatusCode"] != 404 or (response.get("Headers") or {}).get("Location"):
                        raise ValueError("native retired/nonexact resource was not a passive 404")
                    probes += 1
        if callbacks:
            raise ValueError("public resources invoked a host callback")
        print(f"native ABI/resource verified: {library.name}, version={version}, descriptor sha256={sha256(descriptor).hexdigest()}, passive probes={probes}, host callbacks=0")
    finally:
        shutdown()


def main():
    parser = ArgumentParser(description=__doc__)
    parser.add_argument("--library", required=True, type=Path)
    parser.add_argument("--version", required=True)
    args = parser.parse_args()
    version, descriptor = read_metadata(version=args.version)
    smoke(args.library, version, descriptor)


if __name__ == "__main__":
    main()
