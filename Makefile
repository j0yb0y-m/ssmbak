BIN     := bin/ssmbak
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build run test vet install clean

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) .

# Safe by default: shows what would be backed up, writes nothing.
run: build
	./$(BIN) backup --dry-run

test:
	go vet ./...
	go test ./...

install:
	go install -trimpath -ldflags="$(LDFLAGS)" .

clean:
	rm -rf bin
