BINARY   := silo-plugin-wisp
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DIST     := dist
# Strip symbols and inject the build-time version into main.version.
LDFLAGS  := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64

.PHONY: all build test vet fmt fmt-check tidy clean dist manifest

all: fmt-check vet test build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

tidy:
	go mod tidy

# Cross-compile a stripped binary per supported platform into dist/.
# Layout: dist/<binary>-<os>-<arch>
dist: clean
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=$(DIST)/$(BINARY)-$$os-$$arch; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $$out . || exit 1; \
	done

# Print the plugin manifest with the running binary's checksum stamped in.
manifest: build
	./$(BINARY) manifest

clean:
	rm -rf $(DIST) $(BINARY)
