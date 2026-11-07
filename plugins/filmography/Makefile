VERSION ?= 1.2.1
BIN := dist/silo-plugin-filmography
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-arm64 test clean

build:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)-linux-amd64 .

build-arm64:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)-linux-arm64 .

test:
	go vet ./...
	go test ./...

clean:
	rm -rf dist
