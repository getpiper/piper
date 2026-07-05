.PHONY: build test cross
# -s -w strip the symbol table and DWARF debug info: no runtime effect, and it
# claws back the bulk of embedded Caddy's size (piperd ~70M -> ~48M).
LDFLAGS := -s -w
build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/piperd ./cmd/piperd
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/piper  ./cmd/piper
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/piper-relay ./cmd/piper-relay
test:
	go test ./...
cross:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /dev/null ./...
