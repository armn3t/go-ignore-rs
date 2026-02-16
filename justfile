# go-ignore-rs justfile

wasm_src := "rust-wasm"
wasm_target := "wasm32-wasip1"
wasm_out := "matcher.wasm"

# default recipe: build everything
default: wasm

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

# compile the rust wasm module and copy to project root
wasm:
    cd {{wasm_src}} && cargo build --target {{wasm_target}} --release
    cp {{wasm_src}}/target/{{wasm_target}}/release/{{wasm_out}} .
    @ls -lh {{wasm_out}}

# compile wasm in debug mode (faster builds, larger binary, better panic messages)
wasm-debug:
    cd {{wasm_src}} && cargo build --target {{wasm_target}}
    cp {{wasm_src}}/target/{{wasm_target}}/debug/{{wasm_out}} .
    @ls -lh {{wasm_out}}

# ---------------------------------------------------------------------------
# Format
# ---------------------------------------------------------------------------

# format rust code
fmt-rust:
    cd {{wasm_src}} && cargo fmt

# format go code
fmt-go:
    gofmt -w .

# format all code
fmt: fmt-rust fmt-go

# ---------------------------------------------------------------------------
# Lint
# ---------------------------------------------------------------------------

# lint rust code with clippy
lint-rust:
    cd {{wasm_src}} && cargo clippy --target {{wasm_target}} -- -D warnings

# lint go code with go vet
lint-go:
    go vet ./...

# lint all code
lint: lint-rust lint-go

# ---------------------------------------------------------------------------
# Check (format + lint without modifying files)
# ---------------------------------------------------------------------------

# check rust formatting and lint
check-rust:
    cd {{wasm_src}} && cargo fmt -- --check
    cd {{wasm_src}} && cargo clippy --target {{wasm_target}} -- -D warnings

# check go formatting and lint
check-go:
    @echo "Checking go formatting..."
    @test -z "$(gofmt -l .)" || (echo "Go files need formatting:"; gofmt -l .; exit 1)
    go vet ./...

# check all formatting and lint (does not modify files)
check: check-rust check-go

# ---------------------------------------------------------------------------
# Test
# ---------------------------------------------------------------------------

# run go tests (requires wasm to be built first)
test: wasm
    go test -v ./...

# run go benchmarks
bench: wasm
    go test -bench=. -benchmem ./...

# run rust tests (native, not wasm)
test-rust:
    cd {{wasm_src}} && cargo test

# run all tests (rust + go)
test-all: test-rust test

# ---------------------------------------------------------------------------
# CI / all
# ---------------------------------------------------------------------------

# build everything, check formatting/lint, run all tests
all: wasm check test-all

# ---------------------------------------------------------------------------
# Pre-commit hook
# ---------------------------------------------------------------------------

# install the git pre-commit hook
install-hooks:
    @echo '#!/usr/bin/env sh' > .git/hooks/pre-commit
    @echo 'set -e' >> .git/hooks/pre-commit
    @echo '' >> .git/hooks/pre-commit
    @echo '# Run all checks (format + lint) without modifying files' >> .git/hooks/pre-commit
    @echo 'just check' >> .git/hooks/pre-commit
    @echo '' >> .git/hooks/pre-commit
    @echo '# Run rust tests' >> .git/hooks/pre-commit
    @echo 'just test-rust' >> .git/hooks/pre-commit
    @echo '' >> .git/hooks/pre-commit
    @echo '# Rebuild wasm if rust source changed' >> .git/hooks/pre-commit
    @echo 'if git diff --cached --name-only | grep -q "^rust-wasm/"; then' >> .git/hooks/pre-commit
    @echo '    just wasm' >> .git/hooks/pre-commit
    @echo '    git add matcher.wasm' >> .git/hooks/pre-commit
    @echo 'fi' >> .git/hooks/pre-commit
    @echo '' >> .git/hooks/pre-commit
    @echo '# Run go tests' >> .git/hooks/pre-commit
    @echo 'just test' >> .git/hooks/pre-commit
    @chmod +x .git/hooks/pre-commit
    @echo "Pre-commit hook installed at .git/hooks/pre-commit"

# uninstall the git pre-commit hook
uninstall-hooks:
    rm -f .git/hooks/pre-commit
    @echo "Pre-commit hook removed"

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------

# remove all build artifacts
clean:
    cd {{wasm_src}} && cargo clean
    rm -f {{wasm_out}}

# show the wasm binary size
size: wasm
    @echo "Release binary:"
    @ls -lh {{wasm_out}}
