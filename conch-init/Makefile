.PHONY: build clean test check fmt install

TARGET := conch-init
BUILD_TYPE ?= release
UNAME_M := $(shell uname -m)

ifeq ($(UNAME_M),x86_64)
RUST_INIT_TARGET ?= x86_64-unknown-linux-musl
else ifeq ($(UNAME_M),aarch64)
RUST_INIT_TARGET ?= aarch64-unknown-linux-musl
else
$(error unsupported architecture: $(UNAME_M))
endif

ifeq ($(BUILD_TYPE),release)
CARGO_BUILD_FLAGS := --release
TARGET_PATH := target/$(RUST_INIT_TARGET)/release/$(TARGET)
else
CARGO_BUILD_FLAGS :=
TARGET_PATH := target/$(RUST_INIT_TARGET)/debug/$(TARGET)
endif

DESTDIR ?=
BINDIR ?= /usr/bin

build:
	cargo build $(CARGO_BUILD_FLAGS) --target $(RUST_INIT_TARGET)

install: build
	install -D -m 0755 $(TARGET_PATH) $(DESTDIR)$(BINDIR)/$(TARGET)

fmt:
	cargo fmt

check:
	cargo fmt --check
	cargo check --target $(RUST_INIT_TARGET)

test:
	cargo test --target $(RUST_INIT_TARGET)

clean:
	cargo clean
