package ignore

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// NewMatcher + Close lifecycle
// ---------------------------------------------------------------------------

func TestNewMatcherBasic(t *testing.T) {
	m, err := NewMatcher([]string{"*.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	if m.handle == 0 {
		t.Fatal("expected non-zero handle")
	}
	if m.closed {
		t.Fatal("matcher should not be closed")
	}
}

func TestNewMatcherEmptyPatterns(t *testing.T) {
	m, err := NewMatcher([]string{})
	if err != nil {
		t.Fatalf("NewMatcher with empty patterns failed: %v", err)
	}
	defer m.Close()

	// Empty matcher should not match anything.
	if m.Match("anything.txt") {
		t.Error("empty matcher should not match any file")
	}
}

func TestCloseIdempotent(t *testing.T) {
	m, err := NewMatcher([]string{"*.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close should be no-op but got: %v", err)
	}
}

func TestUseAfterClosePanics(t *testing.T) {
	m, err := NewMatcher([]string{"*.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	m.Close()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on Match after Close")
		}
	}()
	m.Match("debug.log")
}

// ---------------------------------------------------------------------------
// Match — single file path
// ---------------------------------------------------------------------------

func TestMatchStarExtension(t *testing.T) {
	m, err := NewMatcher([]string{"*.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	tests := []struct {
		path string
		want bool
	}{
		{"debug.log", true},
		{"error.log", true},
		{"app.txt", false},
		{"src/debug.log", true},
		{"deeply/nested/path/trace.log", true},
	}
	for _, tc := range tests {
		if got := m.Match(tc.path); got != tc.want {
			t.Errorf("Match(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestMatchExactFilename(t *testing.T) {
	m, err := NewMatcher([]string{"Thumbs.db"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	if !m.Match("Thumbs.db") {
		t.Error("should match Thumbs.db")
	}
	if !m.Match("subdir/Thumbs.db") {
		t.Error("should match in subdirectory")
	}
	if m.Match("thumbs.db") {
		t.Error("should be case sensitive by default")
	}
}

func TestMatchDoublestar(t *testing.T) {
	m, err := NewMatcher([]string{"**/logs"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	for _, p := range []string{"logs", "a/logs", "a/b/logs"} {
		if !m.Match(p) {
			t.Errorf("Match(%q) should be true", p)
		}
	}
}

func TestMatchQuestionMark(t *testing.T) {
	m, err := NewMatcher([]string{"debug?.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	if !m.Match("debug0.log") {
		t.Error("? should match single character")
	}
	if m.Match("debug.log") {
		t.Error("? should not match zero characters")
	}
	if m.Match("debug10.log") {
		t.Error("? should not match two characters")
	}
}

func TestMatchCharacterClass(t *testing.T) {
	m, err := NewMatcher([]string{"debug[0-9].log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	if !m.Match("debug5.log") {
		t.Error("should match digit in range")
	}
	if m.Match("debugA.log") {
		t.Error("should not match letter outside range")
	}
}

// ---------------------------------------------------------------------------
// MatchDir — directory paths
// ---------------------------------------------------------------------------

func TestMatchDirTrailingSlashPattern(t *testing.T) {
	m, err := NewMatcher([]string{"build/"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	if !m.MatchDir("build") {
		t.Error("build/ pattern should match directory 'build'")
	}
	if m.Match("build") {
		t.Error("build/ pattern should NOT match file named 'build'")
	}
}

func TestMatchDirWithoutTrailingSlash(t *testing.T) {
	m, err := NewMatcher([]string{"build"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	if !m.Match("build") {
		t.Error("'build' pattern should match file")
	}
	if !m.MatchDir("build") {
		t.Error("'build' pattern should match directory")
	}
}

// ---------------------------------------------------------------------------
// MatchResult — detailed result
// ---------------------------------------------------------------------------

func TestMatchResultValues(t *testing.T) {
	m, err := NewMatcher([]string{"*.log", "!important.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	tests := []struct {
		path  string
		isDir bool
		want  int
	}{
		{"debug.log", false, MatchIgnore},
		{"important.log", false, MatchWhitelist},
		{"src/main.go", false, MatchNone},
	}
	for _, tc := range tests {
		if got := m.MatchResult(tc.path, tc.isDir); got != tc.want {
			t.Errorf("MatchResult(%q, %v) = %d, want %d", tc.path, tc.isDir, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Negation patterns
// ---------------------------------------------------------------------------

func TestNegationBasic(t *testing.T) {
	m, err := NewMatcher([]string{"*.log", "!important.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	if !m.Match("debug.log") {
		t.Error("debug.log should be ignored")
	}
	if m.Match("important.log") {
		t.Error("important.log should NOT be ignored (whitelisted)")
	}
}

func TestNegationOrderMatters(t *testing.T) {
	m, err := NewMatcher([]string{"*.log", "!important.log", "important.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	// Last pattern re-ignores important.log.
	if !m.Match("important.log") {
		t.Error("last pattern should override: important.log should be ignored")
	}
}

// ---------------------------------------------------------------------------
// Anchored patterns
// ---------------------------------------------------------------------------

func TestRootedPatternLeadingSlash(t *testing.T) {
	m, err := NewMatcher([]string{"/build"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	if !m.Match("build") {
		t.Error("/build should match at root")
	}
	if m.Match("src/build") {
		t.Error("/build should NOT match in subdirectory")
	}
}

func TestMiddleSlashAnchors(t *testing.T) {
	m, err := NewMatcher([]string{"doc/frotz"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	if !m.Match("doc/frotz") {
		t.Error("doc/frotz should match")
	}
	if m.Match("a/doc/frotz") {
		t.Error("anchored pattern should NOT match deeper")
	}
}

// ---------------------------------------------------------------------------
// Comments and edge cases
// ---------------------------------------------------------------------------

func TestCommentsAndBlanks(t *testing.T) {
	m, err := NewMatcher([]string{
		"# this is a comment",
		"",
		"*.log",
		"",
		"# another comment",
	})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	if !m.Match("debug.log") {
		t.Error("should still match *.log")
	}
	if m.Match("readme.txt") {
		t.Error("should not match non-log files")
	}
}

func TestEscapedHash(t *testing.T) {
	m, err := NewMatcher([]string{"\\#file"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	if !m.Match("#file") {
		t.Error("escaped # should match literal #file")
	}
}

func TestEscapedBang(t *testing.T) {
	m, err := NewMatcher([]string{"\\!important"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	if !m.Match("!important") {
		t.Error("escaped ! should match literal !important")
	}
}

// ---------------------------------------------------------------------------
// Filter — batch filtering
// ---------------------------------------------------------------------------

func TestFilterBasic(t *testing.T) {
	m, err := NewMatcher([]string{"*.log", "build/"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	paths := []string{
		"src/main.go",
		"debug.log",
		"error.log",
		"build/",
		"README.md",
	}

	got := m.Filter(paths)
	want := []string{"src/main.go", "README.md"}

	assertStringSliceEqual(t, got, want)
}

func TestFilterWithNegation(t *testing.T) {
	m, err := NewMatcher([]string{"*.log", "!important.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	got := m.Filter([]string{"debug.log", "important.log", "error.log", "src/main.go"})
	want := []string{"important.log", "src/main.go"}

	assertStringSliceEqual(t, got, want)
}

func TestFilterAllIgnored(t *testing.T) {
	m, err := NewMatcher([]string{"*"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	got := m.Filter([]string{"a.txt", "b.txt", "c.txt"})
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

func TestFilterNoneIgnored(t *testing.T) {
	m, err := NewMatcher([]string{"*.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	input := []string{"a.txt", "b.rs", "c.go"}
	got := m.Filter(input)

	assertStringSliceEqual(t, got, input)
}

func TestFilterEmptyInput(t *testing.T) {
	m, err := NewMatcher([]string{"*.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	got := m.Filter([]string{})
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestFilterPreservesOrder(t *testing.T) {
	m, err := NewMatcher([]string{"*.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	got := m.Filter([]string{"z.txt", "a.txt", "m.txt", "debug.log", "b.txt"})
	want := []string{"z.txt", "a.txt", "m.txt", "b.txt"}

	assertStringSliceEqual(t, got, want)
}

func TestFilterDirectoryDetection(t *testing.T) {
	m, err := NewMatcher([]string{"build/"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	got := m.Filter([]string{
		"build/",    // trailing slash → directory → should be filtered
		"build",     // no slash → file → should be kept
		"src/main.go",
	})
	want := []string{"build", "src/main.go"}

	assertStringSliceEqual(t, got, want)
}

func TestFilterLargePatternSet(t *testing.T) {
	patterns := []string{
		"*.o", "*.a", "*.so", "*.dylib", "*.dll", "*.exe",
		"*.log", "*.tmp", "*.swp", "*.swo", "*.bak", "*.orig",
		"build/", "dist/", "target/", "out/",
		"node_modules/", "vendor/",
		".git/", ".DS_Store", "Thumbs.db",
		"*.pyc", "__pycache__/",
	}

	m, err := NewMatcher(patterns)
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	paths := []string{
		"src/main.rs",
		"src/lib.rs",
		"Cargo.toml",
		"README.md",
		"build/",
		"target/",
		"main.o",
		"libfoo.a",
		"libbar.so",
		"node_modules/",
		".DS_Store",
		"Thumbs.db",
		"app.log",
		"temp.tmp",
		".vim.swp",
		"src/utils.rs",
		"docs/guide.md",
		"tests/test_main.rs",
	}

	got := m.Filter(paths)
	want := []string{
		"src/main.rs",
		"src/lib.rs",
		"Cargo.toml",
		"README.md",
		"src/utils.rs",
		"docs/guide.md",
		"tests/test_main.rs",
	}

	assertStringSliceEqual(t, got, want)
}

// ---------------------------------------------------------------------------
// FilterParallel
// ---------------------------------------------------------------------------

func TestFilterParallelBasic(t *testing.T) {
	m, err := NewMatcher([]string{"*.log", "build/"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	paths := []string{
		"src/main.go",
		"debug.log",
		"build/",
		"README.md",
	}

	got := m.FilterParallel(paths)
	want := []string{"src/main.go", "README.md"}

	assertStringSliceEqual(t, got, want)
}

func TestFilterParallelEmpty(t *testing.T) {
	m, err := NewMatcher([]string{"*.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	got := m.FilterParallel([]string{})
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestFilterParallelPreservesOrder(t *testing.T) {
	m, err := NewMatcher([]string{"*.log"})
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	// Generate enough paths to actually trigger multiple workers.
	numPaths := runtime.NumCPU() * 100
	paths := make([]string, numPaths)
	var wantKept []string
	for i := 0; i < numPaths; i++ {
		if i%5 == 0 {
			paths[i] = fmt.Sprintf("file_%04d.log", i)
		} else {
			paths[i] = fmt.Sprintf("file_%04d.txt", i)
			wantKept = append(wantKept, paths[i])
		}
	}

	got := m.FilterParallel(paths)

	assertStringSliceEqual(t, got, wantKept)
}

func TestFilterParallelMatchesFilter(t *testing.T) {
	patterns := []string{"*.log", "*.tmp", "build/", "!important.log"}

	m1, err := NewMatcher(patterns)
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m1.Close()

	m2, err := NewMatcher(patterns)
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m2.Close()

	numPaths := runtime.NumCPU() * 200
	paths := make([]string, numPaths)
	for i := 0; i < numPaths; i++ {
		switch i % 7 {
		case 0:
			paths[i] = fmt.Sprintf("dir_%d/file.log", i)
		case 1:
			paths[i] = fmt.Sprintf("dir_%d/file.tmp", i)
		case 2:
			paths[i] = "build/"
		case 3:
			paths[i] = fmt.Sprintf("dir_%d/important.log", i)
		default:
			paths[i] = fmt.Sprintf("dir_%d/file_%d.rs", i, i)
		}
	}

	sequential := m1.Filter(paths)
	parallel := m2.FilterParallel(paths)

	assertStringSliceEqual(t, parallel, sequential)
}

// ---------------------------------------------------------------------------
// Concurrent usage — multiple Matchers from multiple goroutines
// ---------------------------------------------------------------------------

func TestConcurrentMatchers(t *testing.T) {
	const goroutines = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)

	errors := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()

			patterns := []string{fmt.Sprintf("*.pattern%d", id)}
			m, err := NewMatcher(patterns)
			if err != nil {
				errors <- fmt.Errorf("goroutine %d: NewMatcher failed: %w", id, err)
				return
			}
			defer m.Close()

			matchPath := fmt.Sprintf("file.pattern%d", id)
			noMatchPath := fmt.Sprintf("file.other%d", id)

			if !m.Match(matchPath) {
				errors <- fmt.Errorf("goroutine %d: expected %q to match", id, matchPath)
				return
			}
			if m.Match(noMatchPath) {
				errors <- fmt.Errorf("goroutine %d: expected %q to NOT match", id, noMatchPath)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}
}

func TestConcurrentFilterParallel(t *testing.T) {
	const goroutines = 8

	var wg sync.WaitGroup
	wg.Add(goroutines)

	errors := make(chan error, goroutines)

	paths := make([]string, 500)
	for i := range paths {
		if i%3 == 0 {
			paths[i] = fmt.Sprintf("file_%d.log", i)
		} else {
			paths[i] = fmt.Sprintf("file_%d.txt", i)
		}
	}

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()

			m, err := NewMatcher([]string{"*.log"})
			if err != nil {
				errors <- fmt.Errorf("goroutine %d: NewMatcher failed: %w", id, err)
				return
			}
			defer m.Close()

			got := m.FilterParallel(paths)

			for _, p := range got {
				if strings.HasSuffix(p, ".log") {
					errors <- fmt.Errorf("goroutine %d: FilterParallel kept %q", id, p)
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}
}

// ---------------------------------------------------------------------------
// Instance pooling — verify instances are reused
// ---------------------------------------------------------------------------

func TestInstanceReuse(t *testing.T) {
	// Create and close multiple matchers sequentially.
	// The pool should reuse instances rather than creating new ones.
	for i := 0; i < 10; i++ {
		m, err := NewMatcher([]string{fmt.Sprintf("*.ext%d", i)})
		if err != nil {
			t.Fatalf("iteration %d: NewMatcher failed: %v", i, err)
		}

		path := fmt.Sprintf("file.ext%d", i)
		if !m.Match(path) {
			t.Errorf("iteration %d: expected %q to match", i, path)
		}

		m.Close()
	}
}

// ---------------------------------------------------------------------------
// Complex real-world scenario
// ---------------------------------------------------------------------------

func TestRealWorldGitignore(t *testing.T) {
	patterns := []string{
		"# Build",
		"build/",
		"dist/",
		"*.o",
		"*.a",
		"",
		"# Logs",
		"*.log",
		"logs/",
		"",
		"# Dependencies",
		"node_modules/",
		"vendor/",
		"",
		"# Keep important files",
		"!.gitkeep",
		"!README.md",
	}

	m, err := NewMatcher(patterns)
	if err != nil {
		t.Fatalf("NewMatcher failed: %v", err)
	}
	defer m.Close()

	// Individual match checks.
	tests := []struct {
		path  string
		isDir bool
		want  int
	}{
		{"build", true, MatchIgnore},
		{"dist", true, MatchIgnore},
		{"main.o", false, MatchIgnore},
		{"lib.a", false, MatchIgnore},
		{"app.log", false, MatchIgnore},
		{"node_modules", true, MatchIgnore},
		{"vendor", true, MatchIgnore},
		{"src/main.rs", false, MatchNone},
		{"README.md", false, MatchWhitelist},
		{".gitkeep", false, MatchWhitelist},
	}
	for _, tc := range tests {
		got := m.MatchResult(tc.path, tc.isDir)
		if got != tc.want {
			t.Errorf("MatchResult(%q, isDir=%v) = %d, want %d",
				tc.path, tc.isDir, got, tc.want)
		}
	}

	// Batch filter check.
	paths := []string{
		"src/main.rs",
		"src/lib.rs",
		"build/",
		"debug.log",
		"node_modules/",
		"README.md",
		"docs/guide.md",
		".gitkeep",
	}
	got := m.Filter(paths)
	want := []string{
		"src/main.rs",
		"src/lib.rs",
		"README.md",
		"docs/guide.md",
		".gitkeep",
	}
	assertStringSliceEqual(t, got, want)
}

// ---------------------------------------------------------------------------
// Benchmarks
//
// Results on 13th Gen Intel Core i7-1355U (12 threads):
//
//   BenchmarkNewMatcherClose-12          33433    35569 ns/op     276 B/op    11 allocs/op
//   BenchmarkMatchSingle-12             703929     1897 ns/op     144 B/op     7 allocs/op
//   BenchmarkFilter100-12                 8113   133771 ns/op    6848 B/op    17 allocs/op
//   BenchmarkFilter10000-12                 96 11810333 ns/op  762133 B/op    17 allocs/op
//   BenchmarkFilterParallel10000-12        310  3798503 ns/op 1123692 B/op   380 allocs/op
//
// Key observations:
//   - NewMatcher+Close round-trip is ~35µs (instance reuse via sync.Pool)
//   - Single Match call is ~1.8µs (alloc + memcpy + is_match + dealloc)
//   - Filter allocs are constant (17) regardless of path count — batch FFI works
//   - FilterParallel is ~3.2x faster than Filter at 10k paths
// ---------------------------------------------------------------------------

// ~35µs/op — pool round-trip: get instance, compile patterns, destroy, return
func BenchmarkNewMatcherClose(b *testing.B) {
	patterns := []string{"*.log", "build/", "node_modules/", "*.tmp", "!important.log"}
	b.ResetTimer()
	for b.Loop() {
		m, err := NewMatcher(patterns)
		if err != nil {
			b.Fatal(err)
		}
		m.Close()
	}
}

// ~1.8µs/op — single path FFI: alloc + memcpy + is_match + dealloc
func BenchmarkMatchSingle(b *testing.B) {
	m, err := NewMatcher([]string{"*.log", "build/", "node_modules/", "*.tmp"})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()

	b.ResetTimer()
	for b.Loop() {
		m.Match("src/deeply/nested/path/main.go")
	}
}

// ~130µs/op — batch FFI: one round-trip for 100 paths, 17 allocs constant
func BenchmarkFilter100(b *testing.B) {
	m, err := NewMatcher([]string{"*.log", "*.tmp", "build/", "node_modules/"})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()

	paths := make([]string, 100)
	for i := range paths {
		if i%4 == 0 {
			paths[i] = fmt.Sprintf("dir/file_%d.log", i)
		} else {
			paths[i] = fmt.Sprintf("dir/file_%d.rs", i)
		}
	}

	b.ResetTimer()
	for b.Loop() {
		m.Filter(paths)
	}
}

// ~12ms/op — batch FFI: one round-trip for 10k paths, still 17 allocs
func BenchmarkFilter10000(b *testing.B) {
	m, err := NewMatcher([]string{"*.log", "*.tmp", "build/", "node_modules/"})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()

	paths := make([]string, 10000)
	for i := range paths {
		if i%4 == 0 {
			paths[i] = fmt.Sprintf("dir/file_%d.log", i)
		} else {
			paths[i] = fmt.Sprintf("dir/file_%d.rs", i)
		}
	}

	b.ResetTimer()
	for b.Loop() {
		m.Filter(paths)
	}
}

// ~3.8ms/op — parallel: ~3.2x faster than sequential Filter at 10k paths
func BenchmarkFilterParallel10000(b *testing.B) {
	m, err := NewMatcher([]string{"*.log", "*.tmp", "build/", "node_modules/"})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()

	paths := make([]string, 10000)
	for i := range paths {
		if i%4 == 0 {
			paths[i] = fmt.Sprintf("dir/file_%d.log", i)
		} else {
			paths[i] = fmt.Sprintf("dir/file_%d.rs", i)
		}
	}

	b.ResetTimer()
	for b.Loop() {
		m.FilterParallel(paths)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func assertStringSliceEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("length mismatch: got %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q\nfull got:  %v\nfull want: %v", i, got[i], want[i], got, want)
			return
		}
	}
}
