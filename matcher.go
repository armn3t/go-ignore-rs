package ignore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
)

// Sentinel errors returned by MatchResult. Each corresponds to a specific
// negative error code from the WASM is_match export.
var (
	// ErrInvalidHandle is returned when the handle argument is not positive.
	// This should not occur during normal use and indicates a bug.
	ErrInvalidHandle = errors.New("ignore: matcher handle is not positive (never a valid handle)")

	// ErrInvalidPath is returned when the path pointer or length argument
	// sent to WASM is invalid (null pointer with non-zero length, or negative
	// length). This should not occur during normal Go usage.
	ErrInvalidPath = errors.New("ignore: path argument has a null pointer or negative length")

	// ErrPathEncoding is returned when the path bytes are not valid UTF-8.
	// Go strings may contain arbitrary bytes; pass only valid UTF-8 paths.
	ErrPathEncoding = errors.New("ignore: path is not valid UTF-8")

	// ErrHandleNotFound is returned when the handle is positive but not
	// registered in the matcher map. This typically means the Matcher was
	// already closed before MatchResult was called.
	ErrHandleNotFound = errors.New("ignore: matcher handle not found (may have been destroyed)")
)

// Matcher holds a borrowed WASM instance with a compiled set of gitignore-style
// patterns. It is NOT safe for concurrent use — each goroutine should create its
// own Matcher via NewMatcher.
//
// Close must be called when the Matcher is no longer needed. This destroys the
// compiled patterns and returns the WASM instance to the pool for reuse.
type Matcher struct {
	eng    *engine
	inst   *wasmInstance
	handle uint32
	// patterns is the newline-joined pattern string passed to NewMatcher. It is
	// retained so that FilterParallel can compile the same pattern set on each
	// additional borrowed instance without requiring the caller to pass it again.
	patterns string
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
//
// On any error (including WASM errors or invalid UTF-8 in the path), Match
// returns false. Use MatchResult if you need to distinguish between "not
// ignored" and an error condition.
func (m *Matcher) Match(path string) bool {
	matched, _ := m.MatchResult(path, false)
	return matched
}

// MatchDir reports whether the given directory path is ignored by the compiled
// patterns.
//
// On any error, MatchDir returns false. Use MatchResult if you need to
// distinguish between "not ignored" and an error condition.
func (m *Matcher) MatchDir(path string) bool {
	matched, _ := m.MatchResult(path, true)
	return matched
}

// MatchResult reports whether path is ignored and surfaces any WASM error.
//
// Return values:
//
//	(true,  nil) — path is ignored (matched an ignore pattern)
//	(false, nil) — path is not ignored (no match, or matched a negation pattern)
//	(false, err) — a WASM or FFI error occurred; err is one of:
//	               ErrInvalidHandle, ErrInvalidPath, ErrPathEncoding, ErrHandleNotFound
//
// For most callers, Match or MatchDir is simpler. Use MatchResult when you need
// to distinguish between "not ignored" and "an error occurred".
func (m *Matcher) MatchResult(path string, isDir bool) (bool, error) {
	m.mustBeOpen()

	ptr, size, err := m.eng.writeString(m.inst, path)
	if err != nil {
		return false, err
	}
	defer m.eng.freeBytes(m.inst, ptr, size)

	isDirArg := uint64(0)
	if isDir {
		isDirArg = 1
	}

	results, err := m.inst.fnIsMatch.Call(m.eng.ctx,
		uint64(m.handle), uint64(ptr), uint64(size), isDirArg)
	if err != nil {
		return false, fmt.Errorf("ignore: is_match call failed: %w", err)
	}

	switch int32(results[0]) {
	case 0: // not matched
		return false, nil
	case 1: // ignored
		return true, nil
	case 2: // whitelisted (negation pattern)
		return false, nil
	case -1:
		return false, ErrInvalidHandle
	case -2:
		return false, ErrInvalidPath
	case -3:
		return false, ErrPathEncoding
	case -4:
		return false, ErrHandleNotFound
	default:
		return false, fmt.Errorf("ignore: is_match returned unexpected code: %d", int32(results[0]))
	}
}

// Filter returns only the paths from the input slice that are NOT ignored by
// the compiled patterns.
//
// It uses the batch_filter WASM export under the hood — a single FFI round-trip
// regardless of how many paths are in the slice.
//
// Paths ending with "/" are treated as directories for matching purposes.
func (m *Matcher) Filter(paths []string) ([]string, error) {
	m.mustBeOpen()

	if len(paths) == 0 {
		return nil, nil
	}

	return batchFilterOnInstance(m.eng, m.inst, m.handle, paths)
}

// batchFilterOnInstance runs batch_filter on a specific instance/handle and
// returns the kept paths. Used by both Filter and FilterParallel workers.
func batchFilterOnInstance(eng *engine, inst *wasmInstance, handle uint32, paths []string) ([]string, error) {
	blob := strings.Join(paths, "\n")

	pathsPtr, pathsSize, err := eng.writeString(inst, blob)
	if err != nil {
		return nil, fmt.Errorf("ignore: failed to write paths to wasm memory: %w", err)
	}
	defer eng.freeBytes(inst, pathsPtr, pathsSize)

	// Allocate 8 bytes for the result info (result_ptr: i32, result_len: i32).
	infoResults, err := inst.fnAlloc.Call(eng.ctx, 8)
	if err != nil {
		return nil, fmt.Errorf("ignore: failed to allocate result info buffer: %w", err)
	}
	infoPtr := uint32(infoResults[0])
	if infoPtr == 0 {
		return nil, fmt.Errorf("ignore: alloc returned null for result info buffer (out of memory)")
	}
	defer eng.freeBytes(inst, infoPtr, 8)

	results, err := inst.fnBatchFilter.Call(eng.ctx,
		uint64(handle), uint64(pathsPtr), uint64(pathsSize), uint64(infoPtr))
	if err != nil {
		return nil, fmt.Errorf("ignore: batch_filter call failed: %w", err)
	}

	count := int32(results[0])
	switch count {
	case -1:
		return nil, ErrInvalidHandle
	case -2:
		return nil, fmt.Errorf("ignore: batch_filter: null result info pointer (internal error)")
	case -3:
		return nil, ErrInvalidPath
	case -4:
		return nil, ErrPathEncoding
	case -5:
		return nil, ErrHandleNotFound
	default:
		if count < 0 {
			return nil, fmt.Errorf("ignore: batch_filter returned unexpected error code: %d", count)
		}
	}
	if count == 0 {
		// Legitimate result: all paths were filtered out.
		return nil, nil
	}

	// Read result pointer and length from the info buffer.
	infoBuf, ok := inst.mod.Memory().Read(infoPtr, 8)
	if !ok {
		return nil, fmt.Errorf("ignore: failed to read result info from wasm memory (ptr=%d, mem=%d)",
			infoPtr, inst.mod.Memory().Size())
	}

	resultPtr := binary.LittleEndian.Uint32(infoBuf[0:4])
	resultLen := binary.LittleEndian.Uint32(infoBuf[4:8])

	if resultPtr == 0 || resultLen == 0 {
		// Defensive: count > 0 but no result buffer — treat as error.
		return nil, fmt.Errorf("ignore: batch_filter reported %d kept paths but result buffer is empty", count)
	}

	// Read the result blob and free it.
	resultBytes, err := eng.readBytes(inst, resultPtr, resultLen)
	eng.freeBytes(inst, resultPtr, resultLen) // always free, even on read error
	if err != nil {
		return nil, fmt.Errorf("ignore: failed to read batch_filter result from wasm memory: %w", err)
	}

	return strings.Split(string(resultBytes), "\n"), nil
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
//
// Note: on each call to FilterParallel, the pattern set is re-compiled on each
// worker instance (chunks 1..N-1). This compilation cost (~1–10µs per worker)
// is paid on every call, not just the first. It is negligible compared to the
// matching time saved on large path lists, but for repeated calls on small lists
// the overhead accumulates — prefer Filter in that case.
func (m *Matcher) FilterParallel(paths []string) ([]string, error) {
	m.mustBeOpen()

	if len(paths) == 0 {
		return nil, nil
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

	// Worker state: one slot per chunk for its results and errors.
	resultSlices := make([][]string, numWorkers)
	errs := make([]error, numWorkers)
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	// Chunk 0 uses the Matcher's own instance.
	go func() {
		defer wg.Done()
		resultSlices[0], errs[0] = batchFilterOnInstance(m.eng, m.inst, m.handle, chunks[0].paths)
	}()

	// Chunks 1..N-1 borrow temporary instances from the pool.
	for i := 1; i < numWorkers; i++ {
		go func(idx int) {
			defer wg.Done()

			inst, err := m.eng.getInstance()
			if err != nil {
				errs[idx] = fmt.Errorf("ignore: FilterParallel worker %d: failed to get instance: %w", idx, err)
				return
			}
			defer m.eng.putInstance(inst)

			handle, err := createMatcherOnInstance(m.eng, inst, m.patterns)
			if err != nil {
				errs[idx] = fmt.Errorf("ignore: FilterParallel worker %d: failed to create matcher: %w", idx, err)
				return
			}
			defer destroyMatcherOnInstance(m.eng, inst, handle)

			resultSlices[idx], errs[idx] = batchFilterOnInstance(m.eng, inst, handle, chunks[idx].paths)
			if errs[idx] != nil {
				errs[idx] = fmt.Errorf("ignore: FilterParallel worker %d: %w", idx, errs[idx])
			}
		}(i)
	}

	wg.Wait()

	// Collect any worker errors.
	var joinedErr error
	for _, err := range errs {
		if err != nil {
			joinedErr = errors.Join(joinedErr, err)
		}
	}
	if joinedErr != nil {
		return nil, joinedErr
	}

	// Merge results in order.
	total := 0
	for _, r := range resultSlices {
		total += len(r)
	}
	if total == 0 {
		return nil, nil
	}
	merged := make([]string, 0, total)
	for _, r := range resultSlices {
		merged = append(merged, r...)
	}
	return merged, nil
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
