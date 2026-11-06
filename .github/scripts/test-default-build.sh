#!/usr/bin/env bash
# Exercise real release-linked native libraries, not only Go test defaults.
set -euo pipefail
VERSION="$(python3 .github/scripts/release_metadata.py)"
make package
python3 .github/scripts/smoke-native.py --library "build/omc-qoder-oauth.$(go env GOOS | sed 's/darwin/dylib/;s/linux/so/;s/windows/dll/')" --version "${VERSION}"

if make build VERSION=0.0.0-dev >build/rejected-version.log 2>&1; then
  echo 'ERROR: explicit mismatched VERSION was accepted' >&2
  exit 1
fi
grep -q 'build version differs' build/rejected-version.log
if make build GO_LDFLAGS='-s -w -X main.pluginVersion=0.0.0-dev' >build/rejected-ldflags.log 2>&1; then
  echo 'ERROR: mismatched linker input was accepted' >&2
  exit 1
fi
grep -q 'linker pluginVersion differs' build/rejected-ldflags.log

# A deliberately bad final -X value must also be rejected by the real ABI.
ext="$(go env GOOS | sed 's/darwin/dylib/;s/linux/so/;s/windows/dll/')"
(cd go && CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
  -ldflags '-s -w -X main.pluginVersion=0.0.0-dev' -o "../build/version-mismatch.${ext}" .)
if python3 .github/scripts/smoke-native.py --library "build/version-mismatch.${ext}" --version "${VERSION}" >build/rejected-native-version.log 2>&1; then
  echo 'ERROR: real mismatched native registration was accepted' >&2
  exit 1
fi
grep -q 'native registration differs' build/rejected-native-version.log
echo 'Default build/package and explicit VERSION/ldflags/native-ABI drift regressions passed.'
