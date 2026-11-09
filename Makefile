VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: test build vectors clean

test: test-bootstrap
	go vet ./...
	go test ./...

# Static binaries for the machines a Nuit Cloud Server runs on (no runtime needed on the machine).
build:
	mkdir -p dist
	for arch in amd64 arm64; do \
	  GOOS=linux GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/nuit-cloud-agent-linux-$$arch ./cmd/nuit-cloud-agent; \
	done
	echo $(VERSION) > dist/VERSION
	cd dist && (sha256sum nuit-cloud-agent-linux-* > SHA256SUMS 2>/dev/null || shasum -a 256 nuit-cloud-agent-linux-* > SHA256SUMS)

clean:
	rm -rf dist

# The profile features ship inside the agent's release (ADR 0001: a task names a feature, never fetches a script). They are
# written here, in internal/profiles/bootstrap (the only copy; it moved from the platform repository, mgjoyce/nuit-cloud#175).
# Their test runs them against a scratch root with stub system commands, with the by-hand profiles (by-hand/) added.
.PHONY: test-bootstrap
test-bootstrap:
	sh internal/profiles/test-bootstrap.sh
