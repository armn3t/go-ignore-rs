package ignore

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"strings"
	"sync"
)

// MatchNone indicates the path did not match any pattern.
const MatchNone = 0

// MatchIgnore indicates the path matched an ignore pattern and should be excluded.
const MatchIgnore = 1

// MatchWhitelist indicates the path matched a negation pattern (e.g. !keep.log)
// and should be kept.
const MatchWhitelist = 2

// Matcher holds a borrowed WASM instance with a compiled set of gitignore-style
// patterns. It is NOT safe for concurrent use — each goroutine should create its
// own Matcher via NewMatcher.
//
// Close must be called when the Matcher is no longer needed. This destroys the
// compiled patterns and returns the WASM instance to the pool for reuse.
type Matcher struct {
	eng      *engine
	inst     *wasmInstance
	handle   uint32
	patterns string // stored so FilterParallel can compile on borrowed instances
	closed   bool
}

// NewMatcher compiles gitignore-style patterns into a Matcher. Internally it
// borrows a WASM instance from the pool and compiles the patterns on it.
//
// The caller must call Close when done.
//
// Patterns follow standard .gitignore syntax:
//   - "*.log"          matches all .log files
//   - "build/"         matches the build directory
//   - "!important.log" negates a previous pattern
//   - Lines starting with # are comments
//   - Empty lines are ignored
func NewMatcher(patterns []string) (*Matcher, error) {
	eng, err := getEngine()
	if err != nil {
		return nil, err
	}

	inst, err := eng.getInstance()
	if err != nil {
		return nil, err
	}

	joined := strings.Join(patterns, "\n")

	handle, err := createMatcherOnInstance(eng, inst, joined)
	if err != nil {
		eng.putInstance(inst)
		return nil, err
	}

	return &Matcher{
		eng:      eng,
		inst:     inst,
		handle:   handle,
		patterns: joined,
	}, nil
}

// createMatcherOnInstance compiles a newline-joined pattern string on a WASM
// instance and returns the handle. This is used by both NewMatcher and
// FilterParallel (for temporary workers).
func createMatcherOnInstance(eng *engine, inst *wasmInstance, patterns string) (uint32, error) {
	ptr, size, err := eng.writeString(inst, patterns)
	if err != nil {
		return 0, err
	}
	defer eng.freeBytes(inst, ptr, size)

	results, err := inst.fnCreateMatcher.Call(eng.ctx, uint64(ptr), uint64(size))
	if err != nil {
		return 0, fmt.Errorf("ignore: create_matcher call failed: %w", err)
	}

	handle := uint32(results[0])
	if handle == 0 {
		return 0, fmt.Errorf("ignore: failed to compile patterns (create_matcher returned 0)")
	}

	return handle, nil
}

// destroyMatcherOnInstance removes a matcher from a WASM instance's internal
// state.
func destroyMatcherOnInstance(eng *engine, inst *wasmInstance, handle uint32) {
	if handle == 0 {
		return
	}
	_, _ = inst.fnDestroyMatcher.Call(eng.ctx, uint64(handle))
}

// Match reports whether the given file path is ignored by the compiled patterns.
//
// For matching directory paths, use MatchDir instead. For checking large numbers
// of paths, prefer Filter or FilterParallel which use batch FFI.
func (m *Matcher) Match(path string) bool {
	return m.MatchResult(path, false) == MatchIgnore
}

// MatchDir reports whether the given directory path is ignored by the compiled
// patterns.
func (m *Matcher) MatchDir(path string) bool {
	return m.MatchResult(path, true) == MatchIgnore
}

// MatchResult returns the detailed match result for a path:
//
//	MatchNone      (0) = not matched
//	MatchIgnore    (1) = ignored
//	MatchWhitelist (2) = whitelisted (negated pattern)
//	-1                 = error
func (m *Matcher) MatchResult(path string, isDir bool) int {
	m.mustBeOpen()

	ptr, size, err := m.eng.writeString(m.inst, path)
	if err != nil {
		return -1
	}
	defer m.eng.freeBytes(m.inst, ptr, size)

	isDirArg := uint64(0)
	if isDir {
		isDirArg = 1
	}

	results, err := m.inst.fnIsMatch.Call(m.eng.ctx,
		uint64(m.handle), uint64(ptr), uint64(size), isDirArg)
	if err != nil {
		return -1
	}

	return int(int32(results[0]))
}

// Filter returns only the paths from the input slice that are NOT ignored by
// the compiled patterns.
//
// It uses the batch_filter WASM export under the hood — a single FFI round-trip
// regardless of how many paths are in the slice.
//
// Paths ending with "/" are treated as directories for matching purposes.
func (m *Matcher) Filter(paths []string) []string {
	m.mustBeOpen()

	if len(paths) == 0 {
		return nil
	}

	return batchFilterOnInstance(m.eng, m.inst, m.handle, paths)
}

// batchFilterOnInstance runs batch_filter on a specific instance/handle and
// returns the kept paths. Used by both Filter and FilterParallel workers.
func batchFilterOnInstance(eng *engine, inst *wasmInstance, handle uint32, paths []string) []string {
	blob := strings.Join(paths, "\n")

	pathsPtr, pathsSize, err := eng.writeString(inst, blob)
	if err != nil {
		return nil
	}
	defer eng.freeBytes(inst, pathsPtr, pathsSize)

	// Allocate 8 bytes for the result info (result_ptr: i32, result_len: i32).
	infoResults, err := inst.fnAlloc.Call(eng.ctx, 8)
	if err != nil {
		return nil
	}
	infoPtr := uint32(infoResults[0])
	if infoPtr == 0 {
		return nil
	}
	defer eng.freeBytes(inst, infoPtr, 8)

	results, err := inst.fnBatchFilter.Call(eng.ctx,
		uint64(handle), uint64(pathsPtr), uint64(pathsSize), uint64(infoPtr))
	if err != nil {
		return nil
	}

	count := int32(results[0])
	if count <= 0 {
		// count == 0 means nothing was kept; count == -1 means error.
		return nil
	}

	// Read result pointer and length from the info buffer.
	infoBuf, ok := inst.mod.Memory().Read(infoPtr, 8)
	if !ok {
		return nil
	}

	resultPtr := binary.LittleEndian.Uint32(infoBuf[0:4])
	resultLen := binary.LittleEndian.Uint32(infoBuf[4:8])

	if resultPtr == 0 || resultLen == 0 {
		return nil
	}

	// Read the result blob and free it.
	resultBytes, err := eng.readBytes(inst, resultPtr, resultLen)
	eng.freeBytes(inst, resultPtr, resultLen) // always free, even on error
	if err != nil {
		return nil
	}

	return strings.Split(string(resultBytes), "\n")
}

// FilterParallel returns only the paths that are NOT ignored, using multiple
// WASM instances in parallel. It splits the path list into runtime.NumCPU()
// chunks, processes each on a separate WASM instance, and merges results in
// order.
//
// Additional instances are temporarily borrowed from the pool and returned
// when done. The patterns are compiled independently on each instance.
//
// For small path lists (< 10k), the parallelism overhead may exceed the
// savings. Use Filter for small lists.
func (m *Matcher) FilterParallel(paths []string) []string {
	m.mustBeOpen()

	if len(paths) == 0 {
		return nil
	}

	numWorkers := runtime.NumCPU()
	if numWorkers < 1 {
		numWorkers = 1
	}
	if numWorkers > len(paths) {
		numWorkers = len(paths)
	}

	// For a single worker, just use the regular Filter path.
	if numWorkers <= 1 {
		return m.Filter(paths)
	}

	// Split paths into chunks.
	chunkSize := (len(paths) + numWorkers - 1) / numWorkers
	type chunk struct {
		paths []string
		index int
	}
	chunks := make([]chunk, 0, numWorkers)
	for i := 0; i < len(paths); i += chunkSize {
		end := i + chunkSize
		if end > len(paths) {
			end = len(paths)
		}
		chunks = append(chunks, chunk{paths: paths[i:end], index: len(chunks)})
	}
	numWorkers = len(chunks) // actual number of chunks may be less

	// Worker state: one slot per chunk for its results.
	results := make([][]string, numWorkers)
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	// Chunk 0 uses the Matcher's own instance.
	go func() {
		defer wg.Done()
		results[0] = batchFilterOnInstance(m.eng, m.inst, m.handle, chunks[0].paths)
	}()

	// Chunks 1..N-1 borrow temporary instances from the pool.
	for i := 1; i < numWorkers; i++ {
		go func(idx int) {
			defer wg.Done()

			inst, err := m.eng.getInstance()
			if err != nil {
				return
			}
			defer m.eng.putInstance(inst)

			handle, err := createMatcherOnInstance(m.eng, inst, m.patterns)
			if err != nil {
				return
			}
			defer destroyMatcherOnInstance(m.eng, inst, handle)

			results[idx] = batchFilterOnInstance(m.eng, inst, handle, chunks[idx].paths)
		}(i)
	}

	wg.Wait()

	// Merge results in order.
	total := 0
	for _, r := range results {
		total += len(r)
	}
	merged := make([]string, 0, total)
	for _, r := range results {
		merged = append(merged, r...)
	}
	return merged
}

// Close destroys the compiled matcher and returns the WASM instance to the pool
// for reuse. Must be called when the Matcher is no longer needed.
//
// Calling Close more than once is a no-op. Calling any other method after Close
// will panic.
func (m *Matcher) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true

	destroyMatcherOnInstance(m.eng, m.inst, m.handle)
	m.eng.putInstance(m.inst)
	m.inst = nil
	m.handle = 0
	return nil
}

func (m *Matcher) mustBeOpen() {
	if m.closed {
		panic("ignore: use of closed Matcher")
	}
}
