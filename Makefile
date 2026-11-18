# go-syslog build targets. Target platform: linux/amd64 only.

VERSION ?= 0.0.0
BINARY  := dist/go-syslog
LDFLAGS := -s -w -X main.version=$(VERSION)
NFPM    ?= nfpm
STATICCHECK_VERSION := v0.8.2

export VERSION

.PHONY: all build test lint rpm clean

all: lint test build

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/go-syslog

test:
	go test -race ./...

lint:
	@unformatted=$$(gofmt -l .); if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	GOOS=linux GOARCH=amd64 go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

# Requires VERSION in the form X.Y.Z, e.g. make rpm VERSION=1.2.0
rpm: build
	@echo "$(VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "VERSION must be X.Y.Z, got '$(VERSION)'"; exit 1; }
	$(NFPM) package --packager rpm --config packaging/nfpm.yaml --target dist/

clean:
	rm -f dist/go-syslog dist/*.rpm
