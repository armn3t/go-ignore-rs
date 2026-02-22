package ignore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
)

// Sentinel errors corresponding to specific WASM error codes.
var (
	// ErrInvalidHandle is returned when the handle is not positive.
	ErrInvalidHandle = errors.New("ignore: matcher handle is not positive (never a valid handle)")

	// ErrInvalidPath is returned when the path pointer or length is invalid.
	// Should not occur in normal Go usage.
	ErrInvalidPath = errors.New("ignore: path argument has a null pointer or negative length")

	// ErrPathEncoding is returned when the path is not valid UTF-8.
	ErrPathEncoding = errors.New("ignore: path is not valid UTF-8")

	// ErrHandleNotFound is returned when the handle is positive but not in the
	// matcher map, typically because the Matcher was already closed.
	ErrHandleNotFound = errors.New("ignore: matcher handle not found (may have been destroyed)")

	// ErrPatternBuild is returned by NewMatcher when the pattern engine fails
	// to build. Malformed and non-UTF-8 lines are silently skipped, so this
	// is rare in practice.
	ErrPatternBuild = errors.New("ignore: failed to compile patterns")
)

// Matcher holds a borrowed WASM instance with a compiled gitignore pattern set.
// NOT safe for concurrent use. Call Close when done.
type Matcher struct {
	eng      *engine
	inst     *wasmInstance
	handle   uint32
	patterns string // retained for FilterParallel workers
	closed   bool
}

// NewMatcher compiles gitignore-style patterns into a Matcher.
// Borrows a WASM instance from the pool; caller must call Close when done.
//
// Pattern syntax follows standard .gitignore rules:
//   - "*.log"          glob match
//   - "build/"         directory only
//   - "!important.log" negation (whitelist)
//   - "#comment"       ignored line
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

// createMatcherOnInstance compiles patterns on inst and returns the handle.
// Used by NewMatcher and FilterParallel workers.
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

	code := int32(results[0])
	switch code {
	case -1, -2:
		return 0, ErrInvalidPath
	case -3:
		return 0, ErrPatternBuild
	default:
		if code <= 0 {
			return 0, fmt.Errorf("ignore: create_matcher returned unexpected code: %d", code)
		}
	}

	return uint32(code), nil
}

func destroyMatcherOnInstance(eng *engine, inst *wasmInstance, handle uint32) {
	if handle == 0 {
		return
	}
	_, _ = inst.fnDestroyMatcher.Call(eng.ctx, uint64(handle))
}

// Match reports whether path is ignored. Returns false on any error.
// Use MatchResult to distinguish "not ignored" from an error.
func (m *Matcher) Match(path string) bool {
	matched, _ := m.MatchResult(path, false)
	return matched
}

// MatchDir reports whether a directory path is ignored. Returns false on any error.
// Use MatchResult to distinguish "not ignored" from an error.
func (m *Matcher) MatchDir(path string) bool {
	matched, _ := m.MatchResult(path, true)
	return matched
}

// MatchResult reports whether path is ignored and surfaces any error.
//
//	(true,  nil) — ignored
//	(false, nil) — not ignored (no match or negation pattern matched)
//	(false, err) — ErrInvalidHandle, ErrInvalidPath, ErrPathEncoding, or ErrHandleNotFound
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

// Filter returns paths that are NOT ignored. Uses a single batch_filter FFI
// round-trip. Paths ending with "/" are treated as directories.
func (m *Matcher) Filter(paths []string) ([]string, error) {
	m.mustBeOpen()

	if len(paths) == 0 {
		return nil, nil
	}

	return batchFilterOnInstance(m.eng, m.inst, m.handle, paths)
}

// batchFilterOnInstance runs batch_filter on inst/handle. Used by Filter and FilterParallel.
func batchFilterOnInstance(eng *engine, inst *wasmInstance, handle uint32, paths []string) ([]string, error) {
	blob := strings.Join(paths, "\n")

	pathsPtr, pathsSize, err := eng.writeString(inst, blob)
	if err != nil {
		return nil, fmt.Errorf("ignore: failed to write paths to wasm memory: %w", err)
	}
	defer eng.freeBytes(inst, pathsPtr, pathsSize)

	infoResults, err := inst.fnAlloc.Call(eng.ctx, 8) // 8 bytes: result_ptr i32 + result_len i32
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
		return nil, nil
	}

	infoBuf, ok := inst.mod.Memory().Read(infoPtr, 8)
	if !ok {
		return nil, fmt.Errorf("ignore: failed to read result info from wasm memory (ptr=%d, mem=%d)",
			infoPtr, inst.mod.Memory().Size())
	}

	resultPtr := binary.LittleEndian.Uint32(infoBuf[0:4])
	resultLen := binary.LittleEndian.Uint32(infoBuf[4:8])

	if resultPtr == 0 || resultLen == 0 {
		return nil, fmt.Errorf("ignore: batch_filter reported %d kept paths but result buffer is empty", count)
	}

	resultBytes, err := eng.readBytes(inst, resultPtr, resultLen)
	eng.freeBytes(inst, resultPtr, resultLen) // always free, even on read error
	if err != nil {
		return nil, fmt.Errorf("ignore: failed to read batch_filter result from wasm memory: %w", err)
	}

	return strings.Split(string(resultBytes), "\n"), nil
}

// FilterParallel returns paths that are NOT ignored, splitting the list across
// runtime.NumCPU() WASM instances and merging results in order.
// Patterns are re-compiled on each worker (~1–10µs each); prefer Filter for
// small lists (< 10k paths) where parallelism overhead outweighs the savings.
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

	if numWorkers <= 1 {
		return m.Filter(paths)
	}

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
	numWorkers = len(chunks)

	resultSlices := make([][]string, numWorkers)
	errs := make([]error, numWorkers)
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	go func() { // chunk 0 uses the Matcher's own instance
		defer wg.Done()
		resultSlices[0], errs[0] = batchFilterOnInstance(m.eng, m.inst, m.handle, chunks[0].paths)
	}()

	for i := 1; i < numWorkers; i++ { // chunks 1..N-1 borrow temporary instances
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

	var joinedErr error
	for _, err := range errs {
		if err != nil {
			joinedErr = errors.Join(joinedErr, err)
		}
	}
	if joinedErr != nil {
		return nil, joinedErr
	}

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

// Close destroys the matcher and returns the WASM instance to the pool.
// Idempotent; any other method called after Close will panic.
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
