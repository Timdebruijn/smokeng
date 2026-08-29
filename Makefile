VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
# The platforms the prober is meaningful on. Kernel timestamping, path
# discovery and drop counting are Linux facilities; darwin builds so the
# project can be developed on a Mac, with those degradations flagged at
# runtime rather than hidden.
PLATFORMS := linux/amd64 linux/arm64 linux/arm linux/386 darwin/amd64 darwin/arm64

.PHONY: all build web test check notices dist clean

all: web build

# web/dist is committed, so `make build` works without Node.
build:
	go build -ldflags "$(LDFLAGS)" -o bin/smokeng ./cmd/smokeng

web:
	cd web && npm ci && npm run build

test:
	go test ./...

# What CI runs, and what is worth running before a commit.
#
# The cross-platform vet is not redundant with the build: `go build` skips
# _test.go files, so a Linux-only test can stop compiling and neither the
# host vet nor a cross-compile will notice. That is exactly how a broken
# timestamp test reached CI once.
check:
	gofmt -l cmd internal web/embed.go
	go vet ./...
	@for a in amd64 arm64 386 arm; do \
		echo "vet linux/$$a"; \
		GOOS=linux GOARCH=$$a go vet ./... || exit 1; \
	done
	go test ./...
	cd web && npm run typecheck

# The licences of everything statically linked into the binary. smokeng is
# MIT, but its dependencies travel inside the released file and most of them
# require their notice to travel with it.
notices:
	@./scripts/notices.sh THIRD-PARTY-NOTICES

# Release binaries. Static and CGO-free, so one file drops onto any host.
dist: web
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "$(LDFLAGS)" -o dist/smokeng-$$os-$$arch ./cmd/smokeng || exit 1; \
	done
	@./scripts/notices.sh dist/THIRD-PARTY-NOTICES
	@cp LICENSE dist/
	@cd dist && shasum -a 256 smokeng-* LICENSE THIRD-PARTY-NOTICES > SHA256SUMS
	@echo "--- dist/"
	@ls -1 dist

clean:
	rm -rf bin dist THIRD-PARTY-NOTICES
