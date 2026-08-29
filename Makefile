VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
# The platforms the prober is meaningful on. Kernel timestamping, path
# discovery and drop counting are Linux facilities; darwin builds so the
# project can be developed on a Mac, with those degradations flagged at
# runtime rather than hidden.
PLATFORMS := linux/amd64 linux/arm64 linux/arm linux/386 darwin/amd64 darwin/arm64

.PHONY: all build web test check dist clean

all: web build

# web/dist is committed, so `make build` works without Node.
build:
	go build -ldflags "$(LDFLAGS)" -o bin/smokeng ./cmd/smokeng

web:
	cd web && npm ci && npm run build

test:
	go test ./...

# What CI runs, and what is worth running before a commit.
check:
	gofmt -l cmd internal web/embed.go
	go vet ./...
	go test ./...
	cd web && npm run typecheck

# Release binaries. Static and CGO-free, so one file drops onto any host.
dist: web
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "$(LDFLAGS)" -o dist/smokeng-$$os-$$arch ./cmd/smokeng || exit 1; \
	done
	@cd dist && shasum -a 256 smokeng-* > SHA256SUMS
	@echo "--- dist/"
	@ls -1 dist

clean:
	rm -rf bin dist
