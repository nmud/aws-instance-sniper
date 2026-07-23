BINARY  := isnipe
PKG     := ./cmd/isnipe
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: build install fmt vet test clean dist

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) $(PKG)

install:
	go install -trimpath -ldflags="$(LDFLAGS)" $(PKG)

fmt:
	gofmt -w cmd

vet:
	go vet ./...

test:
	go test ./...

clean:
	rm -rf $(BINARY) $(BINARY).exe dist

# Cross-compile a binary for every platform into ./dist
dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
	  out=dist/$(BINARY)-$$os-$$arch$$ext; \
	  echo "  $$out"; \
	  GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="$(LDFLAGS)" -o $$out $(PKG) || exit 1; \
	done
