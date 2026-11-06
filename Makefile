PLUGIN_ID ?= omc-qoder-oauth
VERSION ?= $(shell python3 .github/scripts/release_metadata.py)
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
GO_LDFLAGS ?= -s -w -X main.pluginVersion=$(VERSION)
PLUGIN_EXT = $(if $(filter darwin,$(GOOS)),dylib,$(if $(filter windows,$(GOOS)),dll,so))

.PHONY: test vet check-release build package clean

test:
	cd go && GOPROXY=off GOSUMDB=off go test ./...
	python3 -m unittest discover -s .github/scripts -p 'test_*.py'

vet:
	cd go && GOPROXY=off GOSUMDB=off go vet ./...

check-release:
	python3 .github/scripts/release_metadata.py --version "$(VERSION)" --ldflags "$(GO_LDFLAGS)"

build: check-release
	mkdir -p build
	cd go && CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -buildmode=c-shared -ldflags "$(GO_LDFLAGS)" -o ../build/$(PLUGIN_ID).$(PLUGIN_EXT) .
	rm -f build/$(PLUGIN_ID).h
	python3 .github/scripts/release_metadata.py --version "$(VERSION)" --library build/$(PLUGIN_ID).$(PLUGIN_EXT)
	if [ "$(GOOS)/$(GOARCH)" = "$$(go env GOOS)/$$(go env GOARCH)" ]; then python3 .github/scripts/smoke-native.py --library build/$(PLUGIN_ID).$(PLUGIN_EXT) --version "$(VERSION)"; fi

package: build
	python3 .github/scripts/package-release.py --library build/$(PLUGIN_ID).$(PLUGIN_EXT) --archive build/$(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip --checksum build/$(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip.sha256 --descriptor build/$(PLUGIN_ID).omc.json --version "$(VERSION)"

clean:
	rm -rf build
