.PHONY: fmt lint test frontend build dev run app dmg icon

# Go 1.27 and the macOS SDK target macOS 13+; Wails' cgo asks for 10.13,
# and the linker warns about every object file unless all three agree.
ifeq ($(shell uname -s),Darwin)
export MACOSX_DEPLOYMENT_TARGET = 13.0
export CGO_CFLAGS = -mmacosx-version-min=13.0
export CGO_LDFLAGS = -mmacosx-version-min=13.0
endif

# The packages that don't import Wails: they build and test with
# CGO_ENABLED=0, which is how CI runs them (desktop:lint, desktop:test).
CGO_FREE = $(shell go list -f '{{.ImportPath}}{{range .Deps}} {{.}}{{end}}' ./... | grep -v github.com/wailsapp | cut -d' ' -f1)

fmt:
	gofmt -w .

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "gofmt found unformatted files (run 'make fmt')" && exit 1)
	go vet ./...
	CGO_ENABLED=0 go vet $(CGO_FREE)
	cd frontend && npx tsc -b

# With cgo on macOS, so the FSEvents watcher is tested too.
test:
	go test ./... -count=1

frontend:
	cd frontend && npm run build
	find cmd/cloud-desktop/dist -mindepth 1 ! -name .gitkeep -delete
	cp -R frontend/dist/. cmd/cloud-desktop/dist/

# The production tag: assets from the binary, never from a dev server.
build: frontend
	go build -tags production -o bin/cloud-desktop ./cmd/cloud-desktop
	go build -o bin/cloudsync ./cmd/cloudsync

# The window from Vite's dev server (hot reload), /api and /app still
# through the app's asset handler.
dev:
	@cd frontend && npx vite --clearScreen false & vite=$$!; trap "kill $$vite" EXIT; \
		until curl -sf -o /dev/null http://127.0.0.1:5174; do sleep 0.2; done; \
		FRONTEND_DEVSERVER_URL=http://127.0.0.1:5174 go run ./cmd/cloud-desktop

run: build
	./bin/cloud-desktop

# The macOS app and DMG (E15-1). VERSION comes from the latest v* tag
# (git describe), or is passed in: make dmg VERSION=1.2.3. The Info.plist
# versions must be numbers, so they get only its leading x.y.z.
ifndef VERSION
VERSION := $(patsubst v%,%,$(or $(shell git describe --tags --match 'v[0-9]*' 2>/dev/null),v0.0.0-dev))
endif
SHORT_VERSION := $(shell echo '$(VERSION)' | sed -E 's/^([0-9]+(\.[0-9]+){0,2}).*/\1/')
APP_NAME = Облако
APP = bin/$(APP_NAME).app
DMG = bin/Cloud-$(VERSION)-macos-universal.dmg
RELEASE_LDFLAGS = -s -w -X github.com/do-service/cloud/desktop/internal/api.Version=$(VERSION)

# One binary per architecture, glued by lipo: cgo (Wails, FSEvents) can't
# build both in one go. cloudsync rides along in Contents/MacOS, signed on
# its own first, since the bundle's signature only seals nested code.
# Ad-hoc signing only (no Developer ID yet): arm64 refuses to run unsigned
# code, and Gatekeeper still asks users to confirm the first launch.
app: frontend
	rm -rf "$(APP)" bin/darwin
	for arch in arm64 amd64; do \
		CGO_ENABLED=1 GOOS=darwin GOARCH=$$arch go build -tags production -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o bin/darwin/$$arch/cloud-desktop ./cmd/cloud-desktop && \
		CGO_ENABLED=1 GOOS=darwin GOARCH=$$arch go build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o bin/darwin/$$arch/cloudsync ./cmd/cloudsync || exit 1; \
	done
	mkdir -p "$(APP)/Contents/MacOS" "$(APP)/Contents/Resources"
	lipo -create -output "$(APP)/Contents/MacOS/cloud-desktop" bin/darwin/arm64/cloud-desktop bin/darwin/amd64/cloud-desktop
	lipo -create -output "$(APP)/Contents/MacOS/cloudsync" bin/darwin/arm64/cloudsync bin/darwin/amd64/cloudsync
	sed -e 's/@VERSION@/$(VERSION)/' -e 's/@SHORT_VERSION@/$(SHORT_VERSION)/' build/darwin/Info.plist > "$(APP)/Contents/Info.plist"
	plutil -lint "$(APP)/Contents/Info.plist"
	cp build/darwin/icons.icns "$(APP)/Contents/Resources/"
	xattr -cr "$(APP)"
	codesign --force --sign - --identifier com.do-service.cloud.cloudsync "$(APP)/Contents/MacOS/cloudsync"
	codesign --force --sign - "$(APP)"
	codesign --verify --strict --deep "$(APP)"

# hdiutil create now and then fails with "Resource busy" on CI runners
# (a disk image still being released), hence the retries.
dmg: app
	rm -rf bin/dmg "$(DMG)" "$(DMG).sha256"
	mkdir -p bin/dmg
	cp -R "$(APP)" bin/dmg/
	ln -s /Applications bin/dmg/Applications
	ok=; for i in 1 2 3 4 5; do \
		hdiutil create -volname "$(APP_NAME)" -srcfolder bin/dmg -fs HFS+ -format UDZO -ov "$(DMG)" && ok=1 && break; \
		sleep 5; \
	done; test -n "$$ok"
	rm -rf bin/dmg
	cd bin && shasum -a 256 "$(notdir $(DMG))" > "$(notdir $(DMG)).sha256"

# Regenerates build/darwin/icons.icns (committed) from icon.go.
icon:
	rm -rf bin/icon.iconset && mkdir -p bin/icon.iconset
	go run build/darwin/icon.go bin/icon.png
	for s in 16 32 128 256 512; do \
		sips -z $$s $$s bin/icon.png --out bin/icon.iconset/icon_$${s}x$${s}.png >/dev/null && \
		sips -z $$((s*2)) $$((s*2)) bin/icon.png --out bin/icon.iconset/icon_$${s}x$${s}@2x.png >/dev/null || exit 1; \
	done
	iconutil -c icns -o build/darwin/icons.icns bin/icon.iconset
	rm -rf bin/icon.iconset bin/icon.png

# Linux (E15-3): bin/Cloud-<version>-linux-<arch>.AppImage, .deb and .rpm
# (+ .sha256) for the host's architecture only: cgo against GTK 4 and
# WebKitGTK 6.0 doesn't cross-compile, so CI builds each on its own runner.
.PHONY: linux linux-bin icon-linux
LINUX_ARCH = $(shell go env GOARCH)
LINUX_APP_ID = com.do-service.cloud
APPIMAGE = bin/Cloud-$(VERSION)-linux-$(LINUX_ARCH).AppImage
NFPM = go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0
# The same Wails as the app's: its `generate appimage` bundles WebKitGTK
# (helper processes included) with linuxdeploy's GTK plugin.
WAILS3 = go run github.com/wailsapp/wails/v3/cmd/wails3@$(shell go list -m -f '{{.Version}}' github.com/wailsapp/wails/v3)
# A pinned linuxdeploy, put where Wails looks before downloading its
# "continuous" build.
LINUXDEPLOY_VERSION = 1-alpha-20251107-1
LINUXDEPLOY_ARCH = $(if $(filter arm64,$(LINUX_ARCH)),aarch64,x86_64)
LINUXDEPLOY_SHA256_x86_64 = c20cd71e3a4e3b80c3483cef793cda3f4e990aca14014d23c544ca3ce1270b4d
LINUXDEPLOY_SHA256_aarch64 = 620095110d693282b8ebeb244a95b5e911cf8f65f76c88b4b47d16ae6346fcff

# cloudsync without cgo: inotify needs none, and a static binary runs on
# any distro.
linux-bin: frontend
	rm -rf bin/linux
	CGO_ENABLED=1 go build -tags production -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o bin/linux/cloud-desktop ./cmd/cloud-desktop
	CGO_ENABLED=0 go build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o bin/linux/cloudsync ./cmd/cloudsync

linux: linux-bin
	rm -rf bin/appimage bin/*.AppImage* bin/*.deb* bin/*.rpm*
	mkdir -p bin/appimage
	curl -fsSL --retry 3 -o bin/appimage/linuxdeploy-$(LINUXDEPLOY_ARCH).AppImage \
		https://github.com/linuxdeploy/linuxdeploy/releases/download/$(LINUXDEPLOY_VERSION)/linuxdeploy-$(LINUXDEPLOY_ARCH).AppImage
	echo "$(LINUXDEPLOY_SHA256_$(LINUXDEPLOY_ARCH))  bin/appimage/linuxdeploy-$(LINUXDEPLOY_ARCH).AppImage" | sha256sum -c -
	cp build/linux/icons/256.png bin/appimage/$(LINUX_APP_ID).png
	$(WAILS3) generate appimage -binary bin/linux/cloud-desktop -icon bin/appimage/$(LINUX_APP_ID).png \
		-desktopfile build/linux/$(LINUX_APP_ID).desktop -builddir bin/appimage -outputdir bin/appimage
	mv bin/appimage/cloud-desktop-$(LINUXDEPLOY_ARCH).AppImage "$(APPIMAGE)"
	rm -rf bin/appimage
	VERSION=$(VERSION) GOARCH=$(LINUX_ARCH) $(NFPM) package -f build/linux/nfpm.yaml -p deb -t bin/
	VERSION=$(VERSION) GOARCH=$(LINUX_ARCH) $(NFPM) package -f build/linux/nfpm.yaml -p rpm -t bin/
	cd bin && for f in *.AppImage *.deb *.rpm; do sha256sum "$$f" > "$$f.sha256"; done

# Regenerates build/linux/icons/*.png (committed) from build/darwin/icon.go,
# on a Mac like `make icon`.
icon-linux:
	mkdir -p bin build/linux/icons
	go run build/darwin/icon.go bin/icon.png
	for s in 32 48 64 128 256 512; do \
		sips -z $$s $$s bin/icon.png --out build/linux/icons/$$s.png >/dev/null || exit 1; \
	done
	rm -f bin/icon.png

# The Windows build (E15-2): for amd64 and arm64 each, an NSIS installer
# (per user, no admin rights) and a portable zip, both with a .sha256:
# bin/Cloud-$(VERSION)-windows-<arch>-setup.exe and …-windows-<arch>.zip.
# Wails needs no cgo on Windows, so this cross-builds from any OS; it
# needs makensis (NSIS 3: brew install makensis, choco install nsis) and
# zip or 7z. The icon, manifest and version go in through .syso files from
# go-winres, deleted after the build. The WebView2 bootstrapper is the one
# the pinned Wails ships (its `wails3 generate webview2bootstrapper`).
.PHONY: windows
GO_WINRES = go run github.com/tc-hib/go-winres@v0.3.3
WIN_VERSION := $(shell echo '$(SHORT_VERSION).0.0.0' | cut -d. -f1-4)
WEBVIEW2_SETUP = $(shell go list -m -f '{{.Dir}}' github.com/wailsapp/wails/v3)/internal/commands/webview2/MicrosoftEdgeWebview2Setup.exe
ZIP = $(if $(shell command -v zip),zip -qX,7z a -tzip -bso0)
SHA256 = $(if $(shell command -v sha256sum),sha256sum,shasum -a 256)

windows: frontend
	rm -rf bin/windows bin/Cloud-*-windows-*
	mkdir -p bin/windows
	go run build/darwin/icon.go bin/windows/icon.ico
	cp '$(WEBVIEW2_SETUP)' bin/windows/
	$(GO_WINRES) make --in build/windows/winres.json --arch amd64,arm64 --out cmd/cloud-desktop/rsrc \
		--product-version '$(VERSION)' --file-version '$(WIN_VERSION)'
	$(GO_WINRES) simply --manifest cli --icon bin/windows/icon.ico --arch amd64,arm64 --out cmd/cloudsync/rsrc \
		--product-name 'Облако' --file-description 'Облако: синхронизация из командной строки' \
		--original-filename cloudsync.exe --product-version '$(VERSION)' --file-version '$(WIN_VERSION)'
	for arch in amd64 arm64; do \
		name=Cloud-$(VERSION)-windows-$$arch; \
		GOOS=windows GOARCH=$$arch CGO_ENABLED=0 go build -tags production -trimpath -ldflags "$(RELEASE_LDFLAGS) -H windowsgui" -o bin/windows/$$arch/cloud-desktop.exe ./cmd/cloud-desktop && \
		GOOS=windows GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o bin/windows/$$arch/cloudsync.exe ./cmd/cloudsync && \
		makensis -V2 -NOCD -INPUTCHARSET UTF8 -DARCH=$$arch -DVERSION='$(VERSION)' -DVI_VERSION='$(WIN_VERSION)' \
			-DBIN='bin\windows' -DOUTFILE=bin/$$name-setup.exe \
			build/windows/installer.nsi && \
		(cd bin/windows/$$arch && $(ZIP) ../../$$name.zip cloud-desktop.exe cloudsync.exe) || exit 1; \
	done
	rm -f cmd/*/rsrc_windows_*.syso
	cd bin && for f in Cloud-$(VERSION)-windows-*.exe Cloud-$(VERSION)-windows-*.zip; do \
		$(SHA256) "$$f" > "$$f.sha256" || exit 1; \
	done
