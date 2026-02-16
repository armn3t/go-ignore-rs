# go-ignore-rs — Design Document

## 1. Overview

`go-ignore-rs` provides Go bindings to the Rust [ignore](https://docs.rs/ignore) crate's
gitignore-style glob pattern matching via WebAssembly. Patterns are compiled on the Rust
side into an efficient matcher, and Go code tests file paths against it through a thin
WASM FFI layer.

### Goals

- **Correctness** — Full gitignore spec compliance by delegating to the battle-tested `ignore` crate.
- **Performance** — Compile WASM once, pool instances, batch FFI for millions of paths. Parallel filtering out of the box.
- **Ergonomics** — Idiomatic Go API. Zero runtime file dependencies (WASM binary is embedded).
- **Safety** — No CGo. No unsafe Go. Clear ownership and lifecycle semantics.

### Non-Goals

- Directory walking / recursive filesystem traversal (use Go's `fs.WalkDir` and feed paths into the matcher).
- `.gitignore` file discovery or chaining (the caller supplies patterns explicitly).

---

## 2. Architecture

```text
┌──────────────────────────────────────────────────────────────────┐
│  Go Application                                                  │
│                                                                  │
│    m, _ := ignore.NewMatcher(patterns)                           │
│    defer m.Close()                                               │
│    kept := m.Filter(paths)          // sequential batch          │
│    kept := m.FilterParallel(paths)  // parallel across N cores   │
│      │                                                           │
│      ▼                                                           │
│    ┌────────────────────────────────────────────┐                 │
│    │  engine  (package-level singleton)         │                 │
│    │  ┌──────────────────────────────────────┐  │                 │
│    │  │ wazero.Runtime                       │  │  compiled       │
│    │  │ wazero.CompiledModule                │◄─┼─ once via      │
│    │  │ (AOT-compiled WASM bytecode)         │  │  sync.Once     │
│    │  └──────────────────────────────────────┘  │                 │
│    │                                            │                 │
│    │  ┌──────────────────────────────────────┐  │                 │
│    │  │ sync.Pool of bare WASM instances     │  │                 │
│    │  │ (no matchers loaded — just memory)   │  │                 │
│    │  └──────────────────────────────────────┘  │                 │
│    └───────────────┬────────────────────────────┘                 │
│                    │ NewMatcher: Get() from pool                  │
│                    │ Close:     Put() back to pool                │
│                    ▼                                              │
│    ┌────────────────────────────────────────────┐                 │
│    │  WASM Module Instance                      │                 │
│    │  (own linear memory, own globals)          │                 │
│    │  ┌──────────────────────────────────────┐  │                 │
│    │  │  Rust: HashMap<u32, Gitignore>       │  │                 │
│    │  │  alloc / dealloc                     │  │                 │
│    │  │  create_matcher / destroy_matcher     │  │                 │
│    │  │  is_match / batch_filter              │  │                 │
│    │  └──────────────────────────────────────┘  │                 │
│    └────────────────────────────────────────────┘                 │
└──────────────────────────────────────────────────────────────────┘
```

### Key Layers

| Layer | Lifetime | Thread-safe? | Visible to user? | Description |
|---|---|---|---|---|
| **engine** | Process | ✅ Yes | ❌ No (internal) | Singleton. Holds the `wazero.Runtime`, `wazero.CompiledModule`, and a `sync.Pool` of bare WASM instances. Created once via `sync.Once`. |
| **Instance pool** | Process | ✅ Yes | ❌ No (internal) | `sync.Pool` of WASM module instances with no matchers loaded. Instances are checked out by `NewMatcher` and returned by `Close`. GC reclaims idle instances automatically. |
| **Matcher** | Request / call-site | ❌ No | ✅ Yes | The only user-facing type. Holds a borrowed WASM instance + a compiled pattern set. Created per request with fresh patterns, returned to pool on `Close()`. |

---

## 3. Concurrency Model

### The constraint: patterns change every request

When patterns differ on every request, we cannot pre-load matchers into pooled instances.
Instead, we pool **bare WASM instances** and create/destroy matchers on them per request.

### Why the pool exists

The pool has nothing to do with matchers — it is about **WASM instance thread safety**.
A `wazero` `api.Module` instance is **not safe for concurrent use**. Two goroutines
calling `alloc` → write → `is_match` on the same instance simultaneously will corrupt
each other's memory writes. The pool guarantees that each goroutine gets its own
instance to execute on.

Multiple matchers **can** coexist on the same instance (the `HashMap<u32, Gitignore>`
handles that fine). The issue is never about matchers — it is always about which
goroutine gets to *execute* on a given instance. One goroutine at a time, period.

### Why pool bare instances (not create fresh ones)?

WASM module **compilation** (parsing bytecode → native code) is expensive — on the order
of tens of milliseconds. WASM module **instantiation** (allocating linear memory,
initializing globals) is cheap but not free — on the order of tens of microseconds.

Per-request cost comparison:

| Step | Without instance pool | With instance pool |
|---|---|---|
| Get WASM instance | ~50–100µs (instantiate, allocate linear memory) | ~100ns (`sync.Pool.Get`) |
| `create_matcher` (compile patterns) | ~1–10µs | ~1–10µs |
| N × `is_match` | ~1–2µs each | ~1–2µs each |
| `destroy_matcher` | ~1µs | ~1µs |
| Release instance | ~10µs (close + GC pressure) | ~100ns (`sync.Pool.Put`) |
| **Per-request overhead** | **~60–110µs** | **~2–12µs** |

Under high concurrency (thousands of req/s), the pooled approach saves ~50–100µs of
allocation overhead per request. `sync.Pool` is self-tuning: it grows under load and the
GC reclaims idle instances when traffic drops.

### The scale problem: millions of files

With millions of files and thousands of patterns, the pool serves a second, more
important purpose: **intra-request parallelism**.

A single WASM instance is single-threaded. Matching 10M files sequentially at ~1–2µs
each takes ~10–20 seconds. With a pool, `FilterParallel` can split the file list into
N chunks, grab N instances from the pool (each with the same patterns compiled
independently), match in parallel, and merge results:

| Files | Sequential (1 instance) | Parallel (8 instances) | Parallel (16 instances) |
|---|---|---|---|
| 1M | ~2s | ~250ms | ~125ms |
| 10M | ~20s | ~2.5s | ~1.25s |

Each instance compiles the same patterns independently (~1–10µs per instance), which is
negligible compared to the matching time saved.

### Per-path FFI overhead at scale

At millions of files, calling `alloc` → memcpy → `is_match` → `dealloc` for every
single path becomes a bottleneck — the FFI crossing overhead dominates:

| Approach | 1M files | 10M files |
|---|---|---|
| Per-path `is_match` | ~2s FFI overhead | ~20s FFI overhead |
| Batch `batch_filter` (one FFI round-trip) | ~ms (single memcpy in + memcpy out) | ~tens of ms |

For this reason, `batch_filter` is a **core WASM export**, not a future optimization.
It accepts all paths as a single newline-separated blob, iterates in Rust (no FFI per
path), and returns the filtered result as a newline-separated blob.

### `wazero` concurrency model

| Object | Thread-safe? |
|---|---|
| `wazero.Runtime` | ✅ Yes |
| `wazero.CompiledModule` | ✅ Yes |
| `api.Module` (instance) | ❌ No |

Since each `api.Module` instance has its own linear memory, there is zero contention
between concurrent callers — no locks are needed around matching calls. The `sync.Pool`
simply manages checkout/return.

### Design: invisible pooling

The pool is an internal implementation detail. The user never sees it:

```go
// Per request — patterns can be completely different each time
m, err := ignore.NewMatcher(patterns)   // grabs bare instance from pool
if err != nil { ... }
defer m.Close()                         // destroys matcher, returns instance to pool

kept := m.Filter(paths)                 // sequential, uses batch_filter under the hood
kept := m.FilterParallel(paths)         // parallel across NumCPU instances
```

Under concurrent load, multiple goroutines each get their own `Matcher` (backed by
separate WASM instances), with no contention:

```text
goroutine 1:  NewMatcher(patternsA) ──► Filter ──────────────► Close
goroutine 2:  NewMatcher(patternsB) ──► FilterParallel ──────► Close
goroutine 3:  NewMatcher(patternsC) ──► Match ──► Match ─────► Close
                   │                                              │
                   └── Pool.Get()                   Pool.Put() ───┘
```

`FilterParallel` internally grabs additional instances from the pool for its worker
goroutines, and returns them when done:

```text
FilterParallel(10M paths, 8 workers):
  1. Split paths into 8 chunks
  2. Grab 7 additional instances from pool (the Matcher already has 1)
  3. Compile same patterns on each additional instance → 7 new handles
  4. 8 goroutines each call batch_filter on their chunk
  5. Merge results (order-preserving)
  6. Destroy the 7 temporary handles, return 7 instances to pool
```

### Memory per instance

A WASM instance on `wazero` costs roughly:

- **Linear memory**: starts at 1 page = **64KB**, grows on demand as `alloc` is called
- **Host-side overhead**: wazero Go structs, function table, etc. — roughly **10–50KB**
- **Total**: **~100–300KB** per instance depending on pattern and path sizes

Even 50 pooled instances would be ~15MB — trivially small.

---

## 4. Project Structure

```text
go-ignore-rs/
├── docs/
│   └── DESIGN.md               ← you are here
├── rust-wasm/                   # Rust WASM source
│   ├── Cargo.toml
│   └── src/
│       └── lib.rs
├── go.mod
├── go.sum
├── engine.go                    # engine singleton, WASM compilation, instance pool
├── matcher.go                   # Matcher type (borrows instance from pool)
├── ignore.go                    # Public API surface, package doc
├── ignore_test.go               # Tests
├── matcher.wasm                 # Compiled WASM binary (go:embed)
├── Makefile                     # Build orchestration
├── README.md
└── cmd/
    └── example/
        └── main.go              # Example usage
```

---

## 5. Rust WASM Module

### Target

`wasm32-wasip1` — stable in Rust, first-class `wazero` WASI support.

### Crate dependencies

```toml
[dependencies]
ignore = "0.4"
```

### Exported functions

| Export | Signature (WASM types) | Description |
|---|---|---|
| `alloc` | `(size: i32) -> i32` | Allocate `size` bytes in linear memory. Returns pointer. |
| `dealloc` | `(ptr: i32, size: i32)` | Free a previous allocation. |
| `create_matcher` | `(patterns_ptr: i32, patterns_len: i32) -> i32` | Parse newline-separated gitignore patterns. Compile into a `Gitignore` matcher. Store in a global `HashMap`. Return handle ID (>0) or 0 on error. |
| `is_match` | `(handle: i32, path_ptr: i32, path_len: i32, is_dir: i32) -> i32` | Test path against matcher. Returns: `0` = not matched, `1` = ignored, `2` = whitelisted (negated pattern). |
| `batch_filter` | `(handle: i32, paths_ptr: i32, paths_len: i32, out_ptr: *mut i32, out_len: *mut i32) -> i32` | Filter newline-separated paths in Rust. Allocates result buffer internally. Writes result ptr and len to the provided out-pointers. Returns number of kept paths, or -1 on error. |
| `destroy_matcher` | `(handle: i32)` | Drop the matcher, free its memory from the `HashMap`. |

### Internal state

```text
static MATCHERS: Mutex<HashMap<u32, Gitignore>>
static NEXT_ID: AtomicU32
```

Each WASM instance is single-threaded, so the Mutex is uncontended — it exists only to
satisfy Rust's `static` safety requirements.

Because instances are reused across requests (returned to the pool), `NEXT_ID`
increments monotonically across the instance's lifetime. `destroy_matcher` removes the
entry from the `HashMap`, so there is no unbounded growth.

### Pattern parsing

Patterns are passed as a single newline-separated UTF-8 string. Each line is added to a
`GitignoreBuilder`. Empty lines and comment lines (starting with `#`) are handled natively
by the builder, matching `.gitignore` file semantics exactly.

### `batch_filter` implementation

```text
1. Read newline-separated paths from (paths_ptr, paths_len)
2. For each path, call Gitignore::matched(path, is_dir=false)
3. Collect non-ignored paths into a result Vec<u8> (newline-separated)
4. Leak the result Vec (caller must dealloc)
5. Write result ptr and len to out-pointers
6. Return count of kept paths
```

The caller (Go) is responsible for calling `dealloc` on the result buffer.

---

## 6. Go Public API

### Core types

```go
// engine is unexported — managed as a package-level singleton.
// Holds the wazero.Runtime, CompiledModule, and a sync.Pool of
// bare WASM instances.
type engine struct {
    runtime  wazero.Runtime
    compiled wazero.CompiledModule
    pool     sync.Pool // yields *wasmInstance
}

// Matcher holds a borrowed WASM instance with a compiled pattern set.
// NOT safe for concurrent use. Each goroutine should create its own
// Matcher via NewMatcher.
type Matcher struct {
    inst   *wasmInstance // borrowed from engine.pool
    handle uint32        // handle returned by create_matcher
    closed bool
}
```

### Public functions

```go
// NewMatcher compiles gitignore-style patterns into a Matcher.
// Internally borrows a WASM instance from the pool.
// The caller must call Close() when done.
//
// Patterns follow standard .gitignore syntax:
//   - "*.log"          matches all .log files
//   - "build/"         matches the build directory
//   - "!important.log" negates a previous pattern
//   - "#comment"       ignored (comment line)
//
// Example:
//
//   m, err := ignore.NewMatcher([]string{"*.log", "build/", "!important.log"})
//   if err != nil { ... }
//   defer m.Close()
//
func NewMatcher(patterns []string) (*Matcher, error)

// Match reports whether the given file path is ignored by the compiled patterns.
// Uses per-path FFI (is_match). For large path lists, prefer Filter or FilterParallel.
func (m *Matcher) Match(path string) bool

// MatchDir reports whether the given directory path is ignored.
func (m *Matcher) MatchDir(path string) bool

// MatchResult returns the detailed match result for a path:
// 0 = not matched, 1 = ignored, 2 = whitelisted (negated pattern).
func (m *Matcher) MatchResult(path string, isDir bool) int

// Filter returns only the paths from the input slice that are NOT ignored.
// Uses batch_filter under the hood — a single FFI round-trip regardless of
// how many paths are in the slice.
func (m *Matcher) Filter(paths []string) []string

// FilterParallel returns only the paths that are NOT ignored, using multiple
// WASM instances in parallel. Splits the path list into runtime.NumCPU()
// chunks, processes each on a separate instance, and merges results in order.
//
// Additional instances are temporarily borrowed from the pool and returned
// when done. The patterns are compiled independently on each instance.
//
// For small path lists (<10k), the parallelism overhead may exceed the savings.
// Use Filter for small lists.
func (m *Matcher) FilterParallel(paths []string) []string

// Close destroys the compiled matcher and returns the WASM instance to
// the pool for reuse. Must be called when the Matcher is no longer needed.
// Calling Close more than once is a no-op.
func (m *Matcher) Close() error
```

---

## 7. Data Flow

### Initialization (once per process)

```text
1. First call to NewMatcher triggers sync.Once
2. engine reads embedded matcher.wasm bytes (go:embed)
3. wazero.Runtime compiles WASM → CompiledModule (AOT native code)
4. engine is stored as package-level singleton
5. sync.Pool is initialized with a factory that instantiates new WASM modules
```

### NewMatcher(patterns)

```text
1. engine.pool.Get() → bare WASM instance (or factory creates a new one)
2. Go joins patterns with "\n" into a single string
3. Go calls alloc(len) → ptr in WASM memory
4. Go writes pattern bytes into WASM memory at ptr
5. Go calls create_matcher(ptr, len) → handle (>0 on success, 0 on error)
6. Go calls dealloc(ptr, len)
7. Return Matcher{inst, handle}
```

### Matcher.Match(path)

```text
1. Go calls alloc(len(path)) → ptr
2. Go writes path bytes into WASM memory at ptr
3. Go calls is_match(handle, ptr, len, is_dir=0) → result
4. Go calls dealloc(ptr, len)
5. Return result == 1
```

### Matcher.Filter(paths) — batch

```text
1. Go joins paths with "\n" into a single blob
2. Go calls alloc(len(blob)) → paths_ptr
3. Go writes blob into WASM memory at paths_ptr
4. Go calls alloc(4) twice → out_ptr_loc, out_len_loc (for return values)
5. Go calls batch_filter(handle, paths_ptr, len, out_ptr_loc, out_len_loc)
6. Go reads result ptr and len from out_ptr_loc, out_len_loc
7. Go reads result bytes from WASM memory
8. Go calls dealloc on paths blob, result blob, and out-pointer allocations
9. Go splits result by "\n" into []string
10. Return filtered paths
```

### Matcher.FilterParallel(paths)

```text
1. Determine N = runtime.NumCPU()
2. Split paths into N chunks
3. Chunk 0 uses the Matcher's own WASM instance
4. For chunks 1..N-1:
   a. engine.pool.Get() → bare WASM instance
   b. create_matcher(same patterns) → temp handle
5. Launch N goroutines, each calling batch_filter on its chunk
6. Wait for all goroutines to complete
7. For chunks 1..N-1:
   a. destroy_matcher(temp handle)
   b. engine.pool.Put(instance)
8. Merge results in order
9. Return filtered paths
```

### Matcher.Close()

```text
1. If already closed, return nil (no-op)
2. Call destroy_matcher(handle) — removes matcher from Rust HashMap
3. Return WASM instance to engine.pool via Pool.Put()
4. Mark Matcher as closed
```

---

## 8. Memory Management

### WASM side (Rust)

- `alloc` creates a `Vec<u8>` of the requested size, leaks it via `into_raw_parts`,
  and returns the pointer. The caller (Go) owns this memory until it calls `dealloc`.
- `dealloc` reconstructs the `Vec` from the pointer and length, then drops it.
- `batch_filter` allocates a result buffer internally and leaks it. The caller (Go)
  must `dealloc` this buffer after reading the result.
- Matchers live in the global `HashMap` and are dropped via `destroy_matcher`.

### Go side

- Every `alloc` is paired with a `dealloc`. Helper methods handle this pairing.
- `batch_filter` results are dealloc'd immediately after copying to Go-side memory.
- `Matcher.Close()` calls `destroy_matcher(handle)` to free the matcher in the
  `HashMap`, then returns the bare WASM instance to the pool.

### Instance reuse safety

When an instance is returned to the pool, all matchers on it have been destroyed via
`destroy_matcher`. The instance's linear memory may still contain residual data from
prior allocations, but this is irrelevant — the Rust allocator tracks free/used blocks
correctly, and the next `NewMatcher` call allocates fresh memory for its patterns via
`alloc` and gets a new handle via `create_matcher`. The `NEXT_ID` counter ensures
handle uniqueness across the instance's entire lifetime.

### Leak prevention

- If `Close()` is not called, the WASM instance is NOT returned to the pool and
  will eventually be garbage collected by `wazero`'s runtime (which reclaims the
  linear memory). However, explicit `Close()` is strongly recommended and documented.
- `sync.Pool` entries may be collected by the Go GC between GC cycles. When that
  happens, the instance's `Close` method is called via a runtime finalizer registered
  during instance creation.

---

## 9. Error Handling

| Scenario | Handling |
|---|---|
| WASM compilation failure | `NewMatcher` returns error (only possible on first call) |
| Instance creation failure (pool factory) | `NewMatcher` returns error |
| Invalid / malformed patterns | `create_matcher` returns 0; `NewMatcher` returns descriptive error, instance is returned to pool |
| `alloc` returns 0 (OOM in WASM linear memory) | `NewMatcher` / `Match` / `Filter` returns `ErrOutOfMemory`, instance is returned to pool |
| `batch_filter` returns -1 | `Filter` / `FilterParallel` returns error |
| Calling `Match` after `Close` | Panic (programmer error, same convention as `sync.Mutex`) |
| Double `Close` | No-op (safe) |

---

## 10. Performance Considerations

| Concern | Mitigation |
|---|---|
| WASM compilation cost (~10–50ms) | `sync.Once` — happens exactly once per process. |
| Instance creation cost (~50–100µs) | `sync.Pool` — instances are reused across requests. New instances are only created when the pool is empty under load. |
| Pattern compilation cost per request | Unavoidable since patterns change each request. The `ignore` crate compiles globs into regexes, typically ~1–10µs depending on pattern count. |
| Per-path FFI overhead | Each `Match` call = `alloc` + memcpy + `is_match` + `dealloc`. ~1–2µs per call. Acceptable for small lists. |
| Large file lists (>10k paths) | `Filter` uses `batch_filter` — single FFI round-trip. Newline-join on Go side, single memcpy in, Rust loops internally, single memcpy out. |
| Very large file lists (>1M paths) | `FilterParallel` splits across `runtime.NumCPU()` instances. Each chunk uses `batch_filter`. Near-linear speedup. |
| Memory overhead per pooled instance | ~100–300KB per instance. `sync.Pool` is self-tuning — grows under load, GC reclaims idle instances. No configuration needed. |

---

## 11. Build Pipeline

### Prerequisites

- Rust toolchain with `wasm32-wasip1` target: `rustup target add wasm32-wasip1`
- Go 1.21+

### Makefile targets

| Target | Description |
|---|---|
| `make wasm` | Compile Rust to `matcher.wasm`, copy to project root. |
| `make test` | Run Go tests (uses embedded `.wasm`). |
| `make bench` | Run Go benchmarks. |
| `make all` | `wasm` → `test`. |
| `make clean` | Remove build artifacts. |

### CI

```text
1. Install Rust + wasm32-wasip1 target
2. Install Go
3. make all
```

The committed `matcher.wasm` is checked into the repository so that Go-only consumers
can `go get` the package without needing a Rust toolchain.

---

## 12. Implementation Order

| Phase | Files | Description |
|---|---|---|
| **1. Rust WASM** | `rust-wasm/Cargo.toml`, `rust-wasm/src/lib.rs` | Implement all 6 exports (`alloc`, `dealloc`, `create_matcher`, `is_match`, `batch_filter`, `destroy_matcher`). Verify with a Rust test harness or `wasmtime` CLI. |
| **2. Build** | `Makefile` | `make wasm` target to produce `matcher.wasm`. |
| **3. Engine** | `engine.go` | `wazero` runtime init, module compilation, `sync.Once` singleton, instance pool with factory. |
| **4. Matcher** | `matcher.go` | `NewMatcher`, `Match`, `MatchDir`, `MatchResult`, `Filter`, `FilterParallel`, `Close`. FFI helpers (alloc/write/call/dealloc). |
| **5. Public API** | `ignore.go` | Package doc, any top-level convenience wrappers. |
| **6. Tests** | `ignore_test.go` | Pattern matching, negation, directory matching, batch filter, parallel filter, concurrent usage, edge cases. |
| **7. Example** | `cmd/example/main.go` | Demonstrates creating a Matcher per request with different patterns. |
| **8. Docs** | `README.md` | Installation, usage, API reference, benchmarks. |

---

## 13. Example Usage

### Single request

```go
package main

import (
    "fmt"

    ignore "github.com/user/go-ignore-rs"
)

func main() {
    patterns := []string{
        "*.log",
        "build/",
        "node_modules/",
        "!important.log",
    }

    m, err := ignore.NewMatcher(patterns)
    if err != nil {
        panic(err)
    }
    defer m.Close()

    files := []string{
        "src/main.go",
        "debug.log",
        "important.log",
        "build/output.bin",
        "README.md",
    }

    kept := m.Filter(files)
    fmt.Println(kept)
    // Output: [src/main.go important.log README.md]
}
```

### Concurrent server (patterns differ per request)

```go
http.HandleFunc("/filter", func(w http.ResponseWriter, r *http.Request) {
    var req struct {
        Patterns []string
        Paths    []string
    }
    json.NewDecoder(r.Body).Decode(&req)

    // Each request gets its own Matcher with its own patterns.
    // Internally, a bare WASM instance is borrowed from a pool.
    m, err := ignore.NewMatcher(req.Patterns)
    if err != nil {
        http.Error(w, err.Error(), 400)
        return
    }
    defer m.Close() // destroys matcher, returns WASM instance to pool

    kept := m.Filter(req.Paths)
    json.NewEncoder(w).Encode(kept)
})
```

No pool types, no configuration, no sizing decisions. The internal `sync.Pool` handles
everything automatically.

### Large-scale parallel filtering

```go
m, err := ignore.NewMatcher(patterns)
if err != nil {
    panic(err)
}
defer m.Close()

// 10 million paths — FilterParallel splits across NumCPU instances
kept := m.FilterParallel(millionsOfPaths)
```

---

## 14. Open Questions / Future Work

- **Streaming**: Accept an `io.Reader` of newline-separated paths and return an `io.Reader`
  of results, for memory-efficient processing of huge lists.
- **WASM size optimization**: `wasm-opt` pass, `lto = true`, strip debug info. Target
  < 500KB for the embedded binary.
- **Benchmarks against pure-Go alternatives**: Compare with `go-gitignore`, `doublestar`,
  etc. to quantify the correctness and performance tradeoffs.
- **Error detail from Rust**: Currently `create_matcher` returns 0 on error. Consider
  adding a `get_last_error` export that returns a string describing what went wrong
  (e.g., which pattern failed to parse and why).
- **Reusable write buffer**: Instead of `alloc`/`dealloc` per `Match` call, maintain a
  pre-allocated buffer in each instance that grows as needed, avoiding repeated
  allocation for similarly-sized paths.
- **Directory detection in batch_filter**: Currently `batch_filter` assumes all paths are
  files (`is_dir=false`). Consider a convention (e.g., trailing `/`) or a separate
  `batch_filter_dirs` export.