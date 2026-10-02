.PHONY: help fmt test vet check install

help:
	@echo "make install Build collector; install stable Keychain helper only if absent"
	@echo "make fmt   Format Go source"
	@echo "make test  Run unit and integration tests"
	@echo "make vet   Run static checks"
	@echo "make check Run fmt check, tests and vet"

fmt:
	gofmt -w ./cmd ./internal

test:
	go test ./...

vet:
	go vet ./...

check:
	test -z "$$(gofmt -l ./cmd ./internal)"
	go test ./...
	go vet ./...

install: ../.study_state/bin/study-keychain
	go build -trimpath -buildvcs=false -o ../.study_state/bin/study-monitor ./cmd/study-monitor

# Intentionally has no source prerequisites: changing the collector must not
# invalidate the executable identity trusted by macOS Keychain.
../.study_state/bin/study-keychain:
	mkdir -p ../.study_state/bin
	chmod 700 ../.study_state/bin
	go build -trimpath -buildvcs=false -o $@ ./cmd/study-keychain
