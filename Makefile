BASE_VERSION := $(shell sed -n 's/^var Version = "\(.*\)"/\1/p' main.go)
GIT_REV := $(shell git rev-parse --short HEAD)
GIT_DIRTY := $(shell test -z "$$(git status --porcelain)" || printf '%s' '.dirty')
DEV_VERSION := $(BASE_VERSION)-dev+$(GIT_REV)$(GIT_DIRTY)
LOCAL_BIN ?= $(HOME)/.local/bin/quorum

.PHONY: dev check

# Build the current checkout where the maintainer's shell finds it before the
# Homebrew release. Override LOCAL_BIN to test another location.
dev:
	@mkdir -p "$(dir $(LOCAL_BIN))"
	go build -ldflags "-X main.Version=$(DEV_VERSION)" -o "$(LOCAL_BIN)" .
	@printf 'built %s: ' "$(LOCAL_BIN)"
	@"$(LOCAL_BIN)" --version

check:
	@test -z "$$(gofmt -l .)"
	go test -race ./...
	golangci-lint run ./...
