BASE_VERSION := $(shell sed -n 's/^var Version = "\(.*\)"/\1/p' main.go)
GIT_REV := $(shell git rev-parse --short HEAD)
GIT_DIRTY := $(shell test -z "$$(git status --porcelain)" || printf '%s' '.dirty')
DEV_VERSION := $(BASE_VERSION)-dev+$(GIT_REV)$(GIT_DIRTY)
LOCAL_BIN ?= $(HOME)/.local/bin/quorum

.PHONY: dev fmt-check test build deadcode lint check install-hooks

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

deadcode:
	@output="$$(go tool deadcode ./...)"; status=$$?; \
	if test $$status -ne 0; then exit $$status; fi; \
	if test -n "$$output"; then \
		printf '%s\n' 'Dead code found:' "$$output" >&2; \
		exit 1; \
	fi

lint:
	golangci-lint run ./...

check: fmt-check test build deadcode lint

install-hooks:
	@common_dir="$$(git rev-parse --git-common-dir)"; \
	common_dir="$$(cd "$$common_dir" && pwd -P)"; \
	hooks_dir="$$common_dir/quorum-hooks"; \
	current="$$(git config --local --get core.hooksPath || true)"; \
	if test -n "$$current" && test "$$current" != '.githooks' && test "$$current" != "$$hooks_dir"; then \
		printf 'refusing to replace core.hooksPath=%s\n' "$$current" >&2; \
		exit 1; \
	fi; \
	mkdir -p "$$hooks_dir"; \
	install -m 0755 .githooks/pre-commit "$$hooks_dir/pre-commit"; \
	install -m 0755 .githooks/pre-push "$$hooks_dir/pre-push"; \
	git config --local core.hooksPath "$$hooks_dir"; \
	printf 'Git hooks installed in %s\n' "$$hooks_dir"
