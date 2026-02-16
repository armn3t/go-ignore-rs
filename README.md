# go-ignore-rs

Go package that wraps the Rust [`ignore`](https://docs.rs/ignore) crate's gitignore-style
pattern matcher via WebAssembly. The WASM binary is embedded in the package — no Rust
toolchain or shared libraries are needed at runtime.

The concurrency model is modelled after
[`wasilibs/go-re2`](https://github.com/wasilibs/go-re2): a single compiled WASM module is
shared across the process; individual module instances (each with their own linear memory)
are pooled via `sync.Pool` and checked out exclusively per caller, so concurrent use
requires no locks.

## Requirements

- Go 1.21 or later (uses `go:embed`)
- No CGo, no shared libraries, no runtime file dependencies

## Installation

```sh
go get github.com/armn3t/go-ignore-rs
```

## Quick start

```go
package main

import (
    "fmt"
    "log"

    ignore "github.com/armn3t/go-ignore-rs"
)

func main() {
    m, err := ignore.NewMatcher([]string{
        "*.log",
        "build/",
        "node_modules/",
        "!important.log", // negation: keep this file even though *.log ignores it
    })
    if err != nil {
        log.Fatal(err)
    }
    defer m.Close()

    fmt.Println(m.Match("debug.log"))     // true  — ignored by *.log
    fmt.Println(m.Match("important.log")) // false — whitelisted by !important.log
    fmt.Println(m.Match("src/main.go"))   // false — not matched

    kept, err := m.Filter([]string{
        "src/main.go",
        "debug.log",
        "important.log",
        "README.md",
    })
    // kept == []string{"src/main.go", "important.log", "README.md"}
}
```

## API

### `NewMatcher(patterns []string) (*Matcher, error)`

Compiles a set of gitignore-style patterns into a `Matcher`. Internally borrows a WASM
instance from the process-wide pool (or instantiates a new one if the pool is empty).

`Close` must be called when the `Matcher` is no longer needed. The typical idiom is
`defer m.Close()`.

```go
m, err := ignore.NewMatcher([]string{"*.log", "build/", "!important.log"})
if err != nil {
    return err
}
defer m.Close()
```

The first call in a process compiles the WASM module (AOT, via wazero). This takes
roughly 10–50ms and happens exactly once via `sync.Once`. Subsequent calls pay only the
cost of borrowing a pooled instance (~100ns) and compiling the pattern set (~1–10µs).

### `Match(path string) bool`

Reports whether a file path is ignored by the compiled patterns.

```go
m.Match("debug.log")     // true  — ignored
m.Match("src/main.go")   // false — not ignored
```

On an internal WASM error, `Match` returns `false`. This means a WASM error is
indistinguishable from a non-matching result at this API level. If you need to detect
errors, use `MatchResult` and check for `-1`.

### `MatchDir(path string) bool`

Same as `Match` but treats the path as a directory. This matters for patterns with a
trailing slash (e.g. `build/` only matches directories, not files named `build`).

```go
m.MatchDir("build")    // true  — matched by "build/"
m.Match("build")       // false — "build/" does not match files
```

### `MatchResult(path string, isDir bool) int`

Returns the detailed match result. Useful when you need to distinguish between a path that
was not matched and one that was explicitly whitelisted, or when you need to detect errors.

| Return value | Constant | Meaning |
|---|---|---|
| `0` | `MatchNone` | Path did not match any pattern |
| `1` | `MatchIgnore` | Path matched an ignore pattern |
| `2` | `MatchWhitelist` | Path matched a negation pattern (`!`) |
| `-1` | — | Internal WASM error |

```go
switch m.MatchResult("important.log", false) {
case ignore.MatchNone:
    fmt.Println("not matched")
case ignore.MatchIgnore:
    fmt.Println("ignored")
case ignore.MatchWhitelist:
    fmt.Println("whitelisted (negation pattern)")
}
```

### `Filter(paths []string) ([]string, error)`

Returns only the paths that are **not** ignored. Uses a single FFI round-trip regardless
of how many paths are in the slice — all paths are sent to Rust as one blob, filtered
there, and the result is returned as one blob. Allocation count is constant (does not grow
with the number of paths).

```go
kept, err := m.Filter([]string{
    "src/main.go",
    "debug.log",
    "build/output.bin",
    "README.md",
})
// kept == []string{"src/main.go", "README.md"}
```

Paths ending with `/` are treated as directories for pattern matching purposes:

```go
kept, err := m.Filter([]string{
    "build/",  // directory → matched by "build/", excluded
    "build",   // file      → not matched by "build/", kept
})
// kept == []string{"build"}
```

Returns `nil, nil` when all paths are filtered out or the input is empty.

### `FilterParallel(paths []string) ([]string, error)`

Same as `Filter` but splits the path list into `runtime.NumCPU()` chunks and processes
each chunk on a separate WASM instance in parallel. Results are merged in the original
order.

```go
kept, err := m.FilterParallel(millionsOfPaths)
```

When to use `FilterParallel` vs `Filter`:

| Path count | Recommendation |
|---|---|
| < 10k | `Filter` — parallelism overhead exceeds savings |
| 10k – 1M | Either; benchmark for your pattern set |
| > 1M | `FilterParallel` — approaches linear speedup with core count |

On a 12-thread machine with a realistic pattern set, measured speedups:

| Paths | `Filter` | `FilterParallel` | Speedup |
|---|---|---|---|
| 100 | ~134µs | — | — |
| 10,000 | ~11.8ms | ~3.8ms | ~3.1× |

**Note:** on each `FilterParallel` call, the pattern set is re-compiled on each worker
instance. The compilation cost (~1–10µs per worker) is paid on every call. For repeated
calls on the same `Matcher` with small path lists, this overhead accumulates — use
`Filter` in those cases.

### `Close() error`

Destroys the compiled pattern set and returns the WASM instance to the pool for reuse.
Must be called when the `Matcher` is no longer needed.

- Calling `Close` more than once is a no-op.
- Calling any other method after `Close` panics.

## Concurrency

A `Matcher` is **not safe for concurrent use**. Each goroutine must create its own
`Matcher`:

```go
// Correct: one Matcher per goroutine
var wg sync.WaitGroup
for _, req := range requests {
    req := req
    wg.Add(1)
    go func() {
        defer wg.Done()
        m, err := ignore.NewMatcher(req.Patterns)
        if err != nil {
            return
        }
        defer m.Close()
        req.Result, _ = m.Filter(req.Paths)
    }()
}
wg.Wait()
```

Internally, each `NewMatcher` call borrows a WASM module instance from a `sync.Pool`.
Because each instance has its own linear memory, concurrent callers never contend with
each other — no locks are needed around matching calls. The pool is self-tuning: it grows
under load and the GC reclaims idle instances when traffic drops.

```
goroutine 1:  NewMatcher → [pool instance A] → Filter → Close → return instance A
goroutine 2:  NewMatcher → [pool instance B] → Filter → Close → return instance B
goroutine 3:  NewMatcher → [pool instance A] → Match  → Close → return instance A
                                  ↑ reused after goroutine 1 returned it
```

## Pattern syntax

Patterns follow the [`.gitignore` specification](https://git-scm.com/docs/gitignore).

| Pattern | Effect |
|---|---|
| `*.log` | Matches any file ending in `.log` at any depth |
| `build/` | Matches the `build` directory only (not a file named `build`) |
| `build` | Matches both a file and a directory named `build` |
| `!important.log` | Negates: keeps `important.log` even if a prior pattern ignores it |
| `**/logs` | Matches `logs` at any depth |
| `logs/**` | Matches everything inside `logs/` |
| `logs/**/debug.log` | Matches `debug.log` at any depth inside `logs/` |
| `/build` | Anchored to root: matches `build` but not `src/build` |
| `doc/frotz` | Contains a slash (not trailing): anchored, matches `doc/frotz` only |
| `debug?.log` | `?` matches exactly one non-separator character |
| `debug[0-9].log` | Character class: `debug0.log` through `debug9.log` |
| `# comment` | Ignored (comment line) |
| `\#file` | Escaped `#`: matches a file literally named `#file` |
| `\!important` | Escaped `!`: matches a file literally named `!important` |

**Rule ordering:** later patterns override earlier ones. The last matching pattern wins.

```go
// "!important.log" whitelists important.log even though "*.log" came first
ignore.NewMatcher([]string{"*.log", "!important.log"})

// But adding "important.log" last re-ignores it again
ignore.NewMatcher([]string{"*.log", "!important.log", "important.log"})
```

## Performance

Benchmark results on a 13th Gen Intel i7-1355U (12 threads):

```
BenchmarkNewMatcherClose-12       ~35µs/op     276 B/op    11 allocs/op
BenchmarkMatchSingle-12           ~1.9µs/op    144 B/op     7 allocs/op
BenchmarkFilter100-12             ~134µs/op   6848 B/op    17 allocs/op
BenchmarkFilter10000-12           ~11.8ms/op   762KB        17 allocs/op
BenchmarkFilterParallel10000-12   ~3.8ms/op    1.1MB       380 allocs/op
```

Key observations:

- **`Filter` allocation count is constant** (17) regardless of how many paths are in the
  slice. The entire batch is sent to Rust as a single blob, filtered there, and returned
  as a single blob — one FFI round-trip, not one per path.
- **`FilterParallel` at 10k paths** is ~3.1× faster than sequential `Filter`, with
  allocation count growing only with the number of worker instances (not with path count).
- **`NewMatcher`** at ~35µs/op reflects pool checkout (~100ns), pattern compilation in
  Rust (~1–10µs), and WASM module startup amortised across the process lifetime.

## Building the WASM module

The compiled `matcher.wasm` is checked into the repository so that Go consumers can
`go get` the package without a Rust toolchain. To rebuild it from source:

**Prerequisites:**

```sh
rustup target add wasm32-wasip1
cargo install just          # optional, for the justfile recipes
```

**Build:**

```sh
just wasm
# or without just:
cd rust-wasm && cargo build --target wasm32-wasip1 --release
cp rust-wasm/target/wasm32-wasip1/release/rust_wasm.wasm matcher.wasm
```

**Run all checks and tests:**

```sh
just all        # wasm + format + lint + rust tests + go tests
just test       # go tests only (uses embedded matcher.wasm)
just test-rust  # rust unit tests (runs on host, not WASM)
just bench      # go benchmarks
```

See [`docs/DESIGN.md`](docs/DESIGN.md) for the full architecture, concurrency model, data
flow, and memory management documentation.

## Known limitations

- **`Match`/`MatchDir` swallow errors.** On an internal WASM error, both return `false`
  rather than surfacing the error. Use `MatchResult` and check for `-1` if you need
  error-awareness on single-path calls.

- **WASM linear memory does not shrink.** A pooled instance that processes a very large
  path batch retains its expanded memory until the GC evicts it from `sync.Pool`. For
  workloads that alternate between very large and very small batches, the pool may hold
  instances with inflated memory between GC cycles.

- **`FilterParallel` re-compiles patterns on every call.** Worker instances (all but the
  first) compile the same pattern set from scratch on each `FilterParallel` invocation.
  The cost is ~1–10µs per worker and is negligible for large path lists, but accumulates
  for repeated calls on small lists.

- **No directory walking.** This package matches paths against patterns; it does not walk
  the filesystem. Use `fs.WalkDir` to enumerate paths and feed them into `Filter`.

- **No `.gitignore` file loading.** Patterns must be supplied explicitly by the caller.
  To load a `.gitignore` file, read it and pass `strings.Split(content, "\n")` as the
  patterns.
