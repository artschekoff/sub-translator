VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BINARY   := sub-translator
BIN_DIR  := bin
DIST_DIR := $(BIN_DIR)/dist
LDFLAGS   = -ldflags "-X main.version=$(VERSION)"

.PHONY: build build-all pack test fmt vet tidy validate clean install release gui dmg

build:
	go build $(LDFLAGS) -o $(BIN_DIR)/$(BINARY) .

build-all:
	GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o $(BIN_DIR)/$(BINARY)-darwin-amd64 .
	GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o $(BIN_DIR)/$(BINARY)-darwin-arm64 .
	GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o $(BIN_DIR)/$(BINARY)-linux-amd64 .
	GOOS=linux   GOARCH=arm64 go build $(LDFLAGS) -o $(BIN_DIR)/$(BINARY)-linux-arm64 .
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(BIN_DIR)/$(BINARY)-windows-amd64.exe .

pack: build-all
	mkdir -p $(DIST_DIR)
	tar -czf $(DIST_DIR)/$(BINARY)-darwin-amd64.tar.gz  -C $(BIN_DIR) $(BINARY)-darwin-amd64
	tar -czf $(DIST_DIR)/$(BINARY)-darwin-arm64.tar.gz  -C $(BIN_DIR) $(BINARY)-darwin-arm64
	tar -czf $(DIST_DIR)/$(BINARY)-linux-amd64.tar.gz   -C $(BIN_DIR) $(BINARY)-linux-amd64
	tar -czf $(DIST_DIR)/$(BINARY)-linux-arm64.tar.gz   -C $(BIN_DIR) $(BINARY)-linux-arm64
	zip -j   $(DIST_DIR)/$(BINARY)-windows-amd64.zip        $(BIN_DIR)/$(BINARY)-windows-amd64.exe

test:
	go test ./... -v

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

validate: fmt vet test

clean:
	rm -rf $(BIN_DIR)

install: build
	sudo cp $(BIN_DIR)/$(BINARY) /usr/local/bin/$(BINARY)

gui:
	cd gui && wails build

# Unsigned by an authority, not unsigned: `wails build` ad-hoc self-signs the
# bundle, and the copy below adds a file inside Contents/MacOS/ *after* that
# signature is sealed — which invalidates the seal. Those are two different
# Gatekeeper outcomes, not degrees of the same one. An ad-hoc signature with an
# intact seal is the unidentified-developer case: `spctl` exits 3, and
# right-click → Open overrides it, which is what the README documents. A broken
# seal reports "a sealed resource is missing or invalid" and exits 1, right-click
# → Open offers no override for it at all, and only `xattr -d` clears it.
#
# So the bundle is re-signed after everything that goes in it is in it. That
# returns `codesign -v` to valid and `spctl -a -t exec` to the pristine exit 3 —
# the first-run experience the README is written for. It is still ad-hoc: no
# Developer ID, no notarisation, and a first run still needs right-click → Open
# or `xattr -d com.apple.quarantine`.
dmg: build gui
	rm -rf $(BIN_DIR)/dmg && mkdir -p $(BIN_DIR)/dmg $(DIST_DIR)
	cp -R gui/build/bin/$(BINARY).app $(BIN_DIR)/dmg/
	cp $(BIN_DIR)/$(BINARY) $(BIN_DIR)/dmg/$(BINARY).app/Contents/MacOS/
	codesign --force --deep --sign - $(BIN_DIR)/dmg/$(BINARY).app
	hdiutil create -volname $(BINARY) -srcfolder $(BIN_DIR)/dmg \
		-ov -format UDZO $(DIST_DIR)/$(BINARY).dmg

release:
	@set -e; \
	current=$$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0"); \
	echo "Current version: $$current"; \
	printf "Bump type? [major/minor/patch] (default: patch): "; \
	read bump; \
	bump=$${bump:-patch}; \
	ver=$${current#v}; \
	major=$$(echo $$ver | cut -d. -f1); \
	minor=$$(echo $$ver | cut -d. -f2); \
	patch=$$(echo $$ver | cut -d. -f3); \
	case $$bump in \
		major) major=$$((major+1)); minor=0; patch=0 ;; \
		minor) minor=$$((minor+1)); patch=0 ;; \
		*)     patch=$$((patch+1)) ;; \
	esac; \
	new="v$${major}.$${minor}.$${patch}"; \
	echo "Tagging and releasing $$new..."; \
	git tag $$new; \
	git push origin $$new; \
	$(MAKE) pack VERSION=$$new; \
	gh release create $$new $(DIST_DIR)/* --generate-notes --title "Release $$new"
