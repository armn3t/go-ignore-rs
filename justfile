# go-ignore-rs justfile

wasm_src := "rust-wasm"
wasm_target := "wasm32-wasip1"
wasm_out := "matcher.wasm"
wasm_hash := ".wasm-source-hash"

# default recipe: build everything
default: wasm

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

# compile the rust wasm module and copy to project root
wasm: _wasm-source-hash
    cd {{wasm_src}} && cargo build --target {{wasm_target}} --release
    cp {{wasm_src}}/target/{{wasm_target}}/release/{{wasm_out}} .
    @ls -lh {{wasm_out}}

# compile wasm in debug mode (faster builds, larger binary, better panic messages)
wasm-debug:
    cd {{wasm_src}} && cargo build --target {{wasm_target}}
    cp {{wasm_src}}/target/{{wasm_target}}/debug/{{wasm_out}} .
    @ls -lh {{wasm_out}}

# ---------------------------------------------------------------------------
# WASM source hash
# ---------------------------------------------------------------------------

# compute a sha256 hash of all Rust source files that affect the WASM binary.
# the hash is written to .wasm-source-hash and should be committed alongside
# matcher.wasm so CI can detect staleness without building.
_wasm-source-hash:
    #!/usr/bin/env sh
    hash=$(cat \
        $(find {{wasm_src}}/src -type f -name '*.rs' | sort) \
        {{wasm_src}}/Cargo.toml \
        {{wasm_src}}/Cargo.lock \
        | sha256sum | awk '{print $1}')
    echo "$hash" > {{wasm_hash}}
    echo "Source hash: $hash"

# verify the committed .wasm-source-hash matches the current Rust source
verify-wasm-hash:
    #!/usr/bin/env sh
    set -e
    if [ ! -f {{wasm_hash}} ]; then
        echo "ERROR: {{wasm_hash}} not found. Run 'just wasm' and commit the hash file."
        exit 1
    fi
    committed=$(cat {{wasm_hash}} | tr -d '[:space:]')
    current=$(cat \
        $(find {{wasm_src}}/src -type f -name '*.rs' | sort) \
        {{wasm_src}}/Cargo.toml \
        {{wasm_src}}/Cargo.lock \
        | sha256sum | awk '{print $1}')
    echo "Committed hash: $committed"
    echo "Current hash:   $current"
    if [ "$committed" != "$current" ]; then
        echo ""
        echo "ERROR: {{wasm_hash}} is stale — Rust source has changed."
        echo "Fix: run 'just wasm' and commit both {{wasm_out}} and {{wasm_hash}}."
        exit 1
    fi
    echo "✓ WASM source hash is up to date."

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

# lint go code with go vet and golangci-lint
lint-go:
    go vet ./...
    golangci-lint run ./...

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
    golangci-lint run ./...

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
# Release
# ---------------------------------------------------------------------------

# create a semver tag and push it to trigger the CI/CD publish workflow
# usage: just release 0.2.0
release version:
    #!/usr/bin/env sh
    set -e
    tag="v{{version}}"

    # Validate semver format
    if ! echo "{{version}}" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.]+)?(\+[a-zA-Z0-9.]+)?$'; then
        echo "ERROR: '{{version}}' is not valid semver (expected X.Y.Z[-prerelease][+build])"
        exit 1
    fi

    # Check that this tag doesn't already exist
    if git rev-parse "$tag" >/dev/null 2>&1; then
        echo "ERROR: tag '$tag' already exists."
        exit 1
    fi

    # Compare against the latest existing semver tag to prevent version regression.
    # Splits "vMAJOR.MINOR.PATCH" into three integers and compares lexicographically.
    latest=$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-version:refname | head -n1)
    if [ -n "$latest" ]; then
        echo "Latest existing tag: $latest"

        # Strip leading 'v' and any pre-release/build suffix for numeric comparison
        latest_core=$(echo "$latest" | sed 's/^v//; s/[-+].*//')
        new_core=$(echo "{{version}}" | sed 's/[-+].*//')

        latest_major=$(echo "$latest_core" | cut -d. -f1)
        latest_minor=$(echo "$latest_core" | cut -d. -f2)
        latest_patch=$(echo "$latest_core" | cut -d. -f3)

        new_major=$(echo "$new_core" | cut -d. -f1)
        new_minor=$(echo "$new_core" | cut -d. -f2)
        new_patch=$(echo "$new_core" | cut -d. -f3)

        # Compare: new must be strictly greater than latest
        is_greater=false
        if [ "$new_major" -gt "$latest_major" ]; then
            is_greater=true
        elif [ "$new_major" -eq "$latest_major" ]; then
            if [ "$new_minor" -gt "$latest_minor" ]; then
                is_greater=true
            elif [ "$new_minor" -eq "$latest_minor" ]; then
                if [ "$new_patch" -gt "$latest_patch" ]; then
                    is_greater=true
                fi
            fi
        fi

        if [ "$is_greater" = false ]; then
            echo "ERROR: version '{{version}}' is not greater than latest release '${latest}'."
            echo "  latest:  ${latest_major}.${latest_minor}.${latest_patch}"
            echo "  new:     ${new_major}.${new_minor}.${new_patch}"
            exit 1
        fi

        echo "✓ v{{version}} > $latest"
    else
        echo "No previous release tags found. This will be the first release."
    fi

    # Ensure working tree is clean
    if [ -n "$(git status --porcelain)" ]; then
        echo "ERROR: working tree is dirty. Commit or stash changes first."
        exit 1
    fi

    # Ensure WASM source hash is up to date
    echo "Verifying WASM source hash..."
    just verify-wasm-hash

    # Ensure WASM is up to date
    echo "Rebuilding WASM to verify binary is fresh..."
    just wasm
    if [ -n "$(git status --porcelain matcher.wasm .wasm-source-hash)" ]; then
        echo "ERROR: matcher.wasm or .wasm-source-hash changed after rebuild."
        echo "Commit the updated files first."
        exit 1
    fi

    # Run all checks before tagging
    echo ""
    echo "Running full check suite..."
    just all

    echo ""
    echo "Creating tag $tag ..."
    git tag -a "$tag" -m "Release $tag"
    echo "Pushing tag $tag to origin ..."
    git push origin "$tag"
    echo ""
    echo "✓ Tag $tag pushed. CircleCI will run CI and publish the release."
    echo "  Track progress: https://app.circleci.com/pipelines/github/armn3t/go-ignore-rs"

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
    @echo '# Rebuild wasm and regenerate source hash if rust source changed' >> .git/hooks/pre-commit
    @echo 'if git diff --cached --name-only | grep -q "^rust-wasm/"; then' >> .git/hooks/pre-commit
    @echo '    just wasm' >> .git/hooks/pre-commit
    @echo '    git add matcher.wasm .wasm-source-hash' >> .git/hooks/pre-commit
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
