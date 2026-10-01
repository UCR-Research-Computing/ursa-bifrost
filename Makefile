# Local gauntlet: run `make check` before every commit.
export GOTOOLCHAIN ?= go1.26.8

.PHONY: check fmt vet test build install fixtures

check: fmt vet test build

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

test:
	go test -race -count=1 ./...

build:
	go build -o /dev/null ./cmd/bifrost

install:
	scripts/install.sh

# Re-record fixtures from a read-only probe (see scripts/make_fixtures.py)
fixtures:
	python3 scripts/make_fixtures.py
