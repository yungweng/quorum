BASE_VERSION := $(shell sed -n 's/^var Version = "\(.*\)"/\1/p' main.go)
GIT_REV := $(shell git rev-parse --short HEAD)
GIT_DIRTY := $(shell test -z "$$(git status --porcelain)" || printf '%s' '.dirty')
DEV_VERSION := $(BASE_VERSION)-dev+$(GIT_REV)$(GIT_DIRTY)
LOCAL_BIN ?= $(HOME)/.local/bin/quorum

.PHONY: dev fmt-check test build lint check install-hooks

# Build the current checkout where the maintainer's shell finds it before the
# Homebrew release. Override LOCAL_BIN to test another location.
dev:
	@mkdir -p "$(dir $(LOCAL_BIN))"
	go build -ldflags "-X main.Version=$(DEV_VERSION)" -o "$(LOCAL_BIN)" .
	@printf 'built %s: ' "$(LOCAL_BIN)"
	@"$(LOCAL_BIN)" --version

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if test -n "$$unformatted"; then \
		printf '%s\n' 'Run gofmt on these files:' "$$unformatted" >&2; \
		exit 1; \
	fi

test:
	go test -race ./...

build:
	go build ./...

lint:
	golangci-lint run ./...

check: fmt-check test build lint

install-hooks:
	@current="$$(git config --local --get core.hooksPath || true)"; \
	if test -n "$$current" && test "$$current" != '.githooks'; then \
		printf 'refusing to replace core.hooksPath=%s\n' "$$current" >&2; \
		exit 1; \
	fi
	git config --local core.hooksPath .githooks
	@printf '%s\n' 'Git hooks enabled from .githooks'
