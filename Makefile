.PHONY: all build web test fmt clean

all: web build

# Build the single binary; web/dist is committed, so this works without Node.
build:
	go build -o bin/smokeng ./cmd/smokeng

# Rebuild the embedded frontend (requires Node).
web:
	cd web && npm install && npm run build

test:
	go test ./...

fmt:
	gofmt -w cmd internal web/embed.go

clean:
	rm -rf bin
