.DEFAULT_GOAL := check

.PHONY: check lint check-go check-helm hooks help

check:
	lefthook run pre-commit --force

lint:
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

check-go:
	@go_version=$$(awk '$$1 == "go" { print $$2 }' go.mod); \
	docker_go_version=$$(sed -n 's/^FROM .*golang:\([0-9.]*\)-.*/\1/p' Dockerfile); \
	if [ "$$go_version" != "$$docker_go_version" ]; then \
	  echo 'Go versions in go.mod and Dockerfile must match.' >&2; exit 1; \
	fi
	@unformatted=$$("$$(go env GOROOT)/bin/gofmt" -l cmd internal) || exit $$?; \
	if [ -n "$$unformatted" ]; then \
	  printf 'Run gofmt on:\n%s\n' "$$unformatted" >&2; exit 1; \
	fi
	go vet ./...
	go test -race -count=1 -timeout=5m ./...

check-helm:
	bash .github/scripts/check-helm.sh

hooks:
	lefthook validate
	lefthook install

help:
	@printf '%s\n' \
	  'make / make check  Run all checks through Lefthook' \
	  'make lint          Run Staticcheck' \
	  'make check-go      Check Go formatting, versions, vet and race tests' \
	  'make check-helm    Lint and render the Helm chart' \
	  'make hooks         Validate and install Git hooks' \
	  'make help          Show these commands'
