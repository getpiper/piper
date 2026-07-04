.PHONY: build test cross
build:
	CGO_ENABLED=0 go build -o bin/piperd ./cmd/piperd
	CGO_ENABLED=0 go build -o bin/piper  ./cmd/piper
test:
	go test ./...
cross:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /dev/null ./...
