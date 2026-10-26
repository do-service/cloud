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
	cp -R frontend/dist/ cmd/cloud-desktop/dist/

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
