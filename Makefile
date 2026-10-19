.PHONY: build test race vet bench fmt contention lifecycle native secrets release release-check vulncheck

VERSION ?= 0.1.1
LDFLAGS := -s -w -X amgo/internal/version.Version=$(VERSION)
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/amgo ./cmd/amgo

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

bench:
	go test ./benchmark -run TestStartupDistribution -v
	go test ./internal/store -run '^$$' -bench . -benchmem

contention:
	AMGO_CONTENTION=1 go test ./internal/store -run TestContentionMultiProcess -count=1 -v

lifecycle:
	AMGO_LIFECYCLE=1 go test ./internal/store -run TestLifecycle -count=1 -v -timeout 10m

native:
	AMGO_NATIVE=1 go test -tags native -count=1 -timeout 20m ./tests/native -v

secrets:
	./scripts/secret-scan.sh

release:
	VERSION='$(VERSION)' ./scripts/build-release.sh

release-check:
	AMGO_RELEASE=1 ./scripts/release-check.sh

vulncheck:
	go run $(GOVULNCHECK) ./...

fmt:
	gofmt -w cmd internal benchmark tests
