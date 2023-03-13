##@ Check
## Targets to be used to ensure code quality
.PHONY: verify-code
verify-code: lint ## Verify code formatting
	@diff -u <(echo -n) <(gofmt -d `find . -type f -name '*.go' -not -path "./vendor/*"`)