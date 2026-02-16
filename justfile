# go-ignore-rs justfile

wasm_src := "rust-wasm"
wasm_target := "wasm32-wasip1"
wasm_out := "matcher.wasm"

# default recipe: build everything
default: wasm

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

# run go tests (requires wasm to be built first)
test: wasm
    go test -v ./...

# run go benchmarks
bench: wasm
    go test -bench=. -benchmem ./...

# run rust tests (native, not wasm)
test-rust:
    cd {{wasm_src}} && cargo test

# check rust code without building
check:
    cd {{wasm_src}} && cargo check --target {{wasm_target}}

# format and lint rust code
fmt:
    cd {{wasm_src}} && cargo fmt
    cd {{wasm_src}} && cargo clippy --target {{wasm_target}} -- -D warnings

# build everything and run all tests
all: wasm test

# remove all build artifacts
clean:
    cd {{wasm_src}} && cargo clean
    rm -f {{wasm_out}}

# show the wasm binary size
size: wasm
    @echo "Release binary:"
    @ls -lh {{wasm_out}}
