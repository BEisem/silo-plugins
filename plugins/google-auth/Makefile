VERSION ?= 0.3.2
BIN := dist/silo-plugin-google-auth
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test clean
build:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)-linux-amd64 .
test:
	go vet ./...
	go test ./...
clean:
	rm -rf dist
