.PHONY: build test cross fmt verify
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
# fmt rewrites every Go file in place. Run it if `verify` reports unformatted code.
fmt:
	gofmt -w .
# verify mirrors CI's `verify` job (minus the goreleaser check): fail on
# unformatted code, then vet, test, and the no-cgo arm64 cross-compile. Run it
# before pushing — `make test` alone doesn't catch gofmt, so a formatting-only
# slip passes locally and fails CI.
verify:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-formatted (run 'make fmt'):"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	go vet ./...
	$(MAKE) test
	$(MAKE) cross
