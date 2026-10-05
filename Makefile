GOOS ?= $(shell go env GOOS)
BINARY_NAME := cardano-txpump
BINARY_SUFFIX := $(if $(filter windows,$(GOOS)),.exe,)
BINARY_OUTPUT := $(BINARY_NAME)$(BINARY_SUFFIX)

.PHONY: build

build:
	go build -o $(BINARY_OUTPUT) ./cmd/txpump
