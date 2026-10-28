GOOS ?= $(shell go env GOOS)
BINARY_NAME := cardano-txpump
BINARY_SUFFIX := $(if $(filter windows,$(GOOS)),.exe,)
BINARY_OUTPUT := $(BINARY_NAME)$(BINARY_SUFFIX)

.PHONY: build

build:
	go build -o $(BINARY_OUTPUT) ./cmd/txpump

# Regenerate internal/txpump/blueprints from cardano-templates. Requires
# aiken v1.1.22 on PATH and a cardano-templates checkout at TEMPLATES_COMMIT
# in TEMPLATES_DIR.
TEMPLATES_COMMIT := cbc86fc5d5a25daece94666a9e6f1c48368c1c72

.PHONY: blueprints
blueprints:
	@test -n "$(TEMPLATES_DIR)" || { echo "set TEMPLATES_DIR to a cardano-templates checkout"; exit 1; }
	@test "$$(git -C "$(TEMPLATES_DIR)" rev-parse HEAD)" = "$(TEMPLATES_COMMIT)" || { echo "checkout must be at $(TEMPLATES_COMMIT)"; exit 1; }
	@aiken --version | grep -Eq '(^|[[:space:]])v1\.1\.22(\+[^[:space:]]*)?$$' || { echo "aiken v1.1.22 required"; exit 1; }
	for t in escrow vesting simple-transfer; do \
		(cd "$(TEMPLATES_DIR)/$$t/onchain/aiken" && aiken build) && \
		cp "$(TEMPLATES_DIR)/$$t/onchain/aiken/plutus.json" internal/txpump/blueprints/$$t.json || exit 1; \
	done
