.PHONY: test build build-darwin-arm64 build-linux-amd64 clean tidy

GO ?= go
CGO ?= CGO_ENABLED=0
LDFLAGS ?= -s -w
BUILDFLAGS ?= -trimpath -ldflags '$(LDFLAGS)'

test:
	$(GO) test ./...

test-v:
	$(GO) test -v ./...

tidy:
	$(GO) mod tidy

build: build-darwin-arm64 build-linux-amd64

build-darwin-arm64:
	$(CGO) GOOS=darwin GOARCH=arm64 $(GO) build $(BUILDFLAGS) -o dist/nl-punch-darwin-arm64 ./cmd/nl-punch
	$(CGO) GOOS=darwin GOARCH=arm64 $(GO) build $(BUILDFLAGS) -o dist/nl-punch-signaling-darwin-arm64 ./cmd/nl-punch-signaling
	$(CGO) GOOS=darwin GOARCH=arm64 $(GO) build $(BUILDFLAGS) -o dist/nl-punch-bench-darwin-arm64 ./cmd/nl-punch-bench

build-linux-amd64:
	$(CGO) GOOS=linux GOARCH=amd64 $(GO) build $(BUILDFLAGS) -o dist/nl-punch-linux-amd64 ./cmd/nl-punch
	$(CGO) GOOS=linux GOARCH=amd64 $(GO) build $(BUILDFLAGS) -o dist/nl-punch-signaling-linux-amd64 ./cmd/nl-punch-signaling
	$(CGO) GOOS=linux GOARCH=amd64 $(GO) build $(BUILDFLAGS) -o dist/nl-punch-bench-linux-amd64 ./cmd/nl-punch-bench

clean:
	rm -rf dist/
