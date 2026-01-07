.PHONY: build clean test fmt vet lint help gen-proto mod-tidy mod-vendor install

# project name
PROJECT_NAME := Conch

# Go commands
GOCMD := go
GOBUILD := $(GOCMD) build
GOCLEAN := $(GOCMD) clean
GOTEST := $(GOCMD) test
GOGET := $(GOCMD) get
GOMOD := $(GOCMD) mod

# binary output directory
BIN_DIR := bin

# conch-agent config
CONCH_AGENT_PROTO_DIR := ./api
CONCH_AGENT_GEN_DIR := ./api/go_proto

# get all cmd directory subdirectories as binary names
CMDS := $(shell find cmd -mindepth 1 -maxdepth 1 -type d -exec basename {} \;)

help:
	@echo "available commands:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

gen-proto:
	@echo "installing proto tools (if not exist)..."
	@which protoc-gen-go >/dev/null 2>&1 || GOBIN=$(shell go env GOPATH)/bin go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
	@which protoc-gen-go-grpc >/dev/null 2>&1 || GOBIN=$(shell go env GOPATH)/bin go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
	
	@echo "generating proto code..."
	@mkdir -p $(CONCH_AGENT_GEN_DIR)
	@protoc \
		--proto_path=$(CONCH_AGENT_PROTO_DIR) \
		--go_out=$(CONCH_AGENT_GEN_DIR) \
		--go_opt=paths=source_relative \
		--go-grpc_out=$(CONCH_AGENT_GEN_DIR) \
		--go-grpc_opt=paths=source_relative,require_unimplemented_servers=false \
		$(CONCH_AGENT_PROTO_DIR)/*.proto;
	@echo "proto code generated to $(CONCH_AGENT_GEN_DIR)"

build: gen-proto mod-tidy
	@echo "building all binaries..."
	@mkdir -p $(BIN_DIR)
	@for cmd in $(CMDS); do \
		echo "building cmd/$$cmd..."; \
		$(GOBUILD) -o $(BIN_DIR)/$$cmd ./cmd/$$cmd; \
	done
	@echo "building completed, binaries located in $(BIN_DIR)/ directory"

build-%:
	@echo "building cmd/$*..."
	@mkdir -p $(BIN_DIR)
	$(GOBUILD) -o $(BIN_DIR)/$* ./cmd/$*

clean:
	@echo "cleaning build artifacts..."
	$(GOCLEAN)
	rm -rf $(BIN_DIR)
	rm -rf $(CONCH_AGENT_GEN_DIR)
	@echo "cleaning completed"

test:
	@echo "running tests..."
	$(GOTEST) -v ./...

test-coverage:
	@echo "running tests and generating coverage report..."
	$(GOTEST) -v -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out -o coverage.html
	@echo "coverage report generated: coverage.html"

fmt:
	@echo "formatting code..."
	$(GOCMD) fmt ./...

vet:
	@echo "running go vet..."
	$(GOCMD) vet ./...

lint: fmt vet

mod-tidy:
	@echo "tidying dependencies..."
	$(GOMOD) tidy

mod-vendor:
	@echo "downloading dependencies to vendor directory..."
	$(GOMOD) vendor

install: build
	@echo "building and installing to $(shell go env GOPATH)/bin..."
	@for cmd in $(CMDS); do \
		cp $(BIN_DIR)/$$cmd $(shell go env GOPATH)/bin/$$cmd || true; \
	done
