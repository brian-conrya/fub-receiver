.PHONY: test lint vet fmt fmt-check vuln ci tools

GO ?= go
GOBIN ?= $(shell $(GO) env GOPATH)/bin

# Tool versions used by `make tools`. Keep these in sync with .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION ?= v2.14.0
GOVULNCHECK_VERSION ?= latest

test:
	$(GO) test -race -shuffle=on -count=1 ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "Unformatted files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

lint:
	$(GOBIN)/golangci-lint run ./...

vuln:
	$(GOBIN)/govulncheck ./...

tools:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

ci: fmt-check vet lint test vuln
