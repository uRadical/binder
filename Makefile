# Development tasks for binder.
#
# The CRAP flags live here rather than in CI so that a local run and a pipeline
# run measure the same thing. go-crap has no configuration file, so this is the
# single definition of how it is invoked.

COVERAGE   := coverage.out
CRAP_FLAGS := --exclude 'example/.*\.go' --threshold 30

.PHONY: all test cover crap crap-baseline bench vet fmt lint check fuzz clean

all: fmt vet check test

# Static analysis and vulnerability scanning, as CI runs them.
check:
	staticcheck ./...
	govulncheck ./...

# A short fuzz run. CI does the same on every push; run longer when changing
# the reflection paths.
fuzz:
	go test -run '^$$' -fuzz 'FuzzBind$$' -fuzztime=30s
	go test -run '^$$' -fuzz 'FuzzBindWithOptions' -fuzztime=15s
	go test -run '^$$' -fuzz 'FuzzBindStruct' -fuzztime=15s

test:
	go test -race ./...

# Produce the coverage profile the CRAP scan reads. Running it here means the
# scan does not have to run the suite a second time.
cover:
	go test -coverprofile=$(COVERAGE) ./...
	go tool cover -func=$(COVERAGE) | tail -1

# Fail if any function exceeds the complexity-versus-coverage threshold.
crap: cover
	go-crap scan ./... $(CRAP_FLAGS) --coverage-profile=$(COVERAGE) --fail-above

# Regenerate the baseline used for regression comparison. Commit the result
# only when the new scores are intended.
crap-baseline: cover
	go-crap scan ./... $(CRAP_FLAGS) --coverage-profile=$(COVERAGE) -f json -o crap-baseline.json

bench:
	go test -run '^$$' -bench=. -benchmem ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

# Report unformatted files without rewriting them, as CI does.
lint:
	@if [ "$$(gofmt -s -l . | wc -l)" -gt 0 ]; then \
		echo "The following files are not formatted:"; \
		gofmt -s -l .; \
		exit 1; \
	fi

clean:
	rm -f $(COVERAGE)
