##@ Check
## Targets to be used to ensure code quality
.PHONY: verify-code
verify-code: vet lint ## Verify code formatting
	@diff -u <(echo -n) <(gofmt -d `find . -type f -name '*.go' -not -path "./vendor/*"`)

.PHONY: verify-vendors
verify-vendors: vendor ## Verify vendors and go.mod up to date
	git diff --exit-code vendor/
	git diff --exit-code go.sum