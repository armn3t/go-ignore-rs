// Package ignore provides Go bindings to the Rust ignore crate's gitignore-style
// glob pattern matching via WebAssembly.
//
// Patterns are compiled on the Rust side into an efficient matcher, and Go code
// tests file paths against it through a thin WASM FFI layer. The WASM module is
// embedded in the binary at compile time — there are no runtime file dependencies.
//
// # Quick Start
//
//	m, err := ignore.NewMatcher([]string{"*.log", "build/", "!important.log"})
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer m.Close()
//
//	fmt.Println(m.Match("debug.log"))     // true  (ignored)
//	fmt.Println(m.Match("important.log")) // false (whitelisted)
//	fmt.Println(m.Match("src/main.go"))   // false (not matched)
//
//	kept, err := m.Filter([]string{"debug.log", "important.log", "src/main.go"})
//	// kept == []string{"important.log", "src/main.go"}
//
// # Concurrency
//
// A Matcher is NOT safe for concurrent use. Each goroutine should create its own
// Matcher via NewMatcher. Internally, WASM instances are pooled and reused across
// Matcher lifecycles — the pool is invisible to callers and requires no
// configuration.
//
// For large path lists (> 1M paths), use FilterParallel which splits the work
// across multiple WASM instances in parallel:
//
//	kept, err := m.FilterParallel(millionsOfPaths)
//
// # Pattern Syntax
//
// Patterns follow the standard .gitignore specification:
//
//   - "*" matches any sequence of non-separator characters
//   - "?" matches any single non-separator character
//   - "**" matches any sequence of characters including separators
//   - "[abc]" or "[a-z]" matches character classes
//   - A trailing "/" restricts the pattern to directories only
//   - A leading "/" anchors the pattern to the root
//   - A leading "!" negates the pattern (whitelists previously ignored paths)
//   - Lines starting with "#" are comments
//   - Empty lines are ignored
//   - Later patterns override earlier ones
package ignore
