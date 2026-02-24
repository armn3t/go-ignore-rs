package ignore

import (
	"context"
	_ "embed"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed matcher.wasm
var matcherWasm []byte

// wasmInstance wraps a wazero module instance with cached function references.
// Each instance has its own linear memory and is NOT safe for concurrent use.
type wasmInstance struct {
	mod api.Module
	// tainted is set when a wazero Call itself returns a Go error (indicating a
	// WASM trap or runtime fault). This is distinct from a Rust function
	// returning a negative i32 error code, which is a normal, safe return path.
	// After a trap, linear memory state is undefined, so the instance must be
	// closed and discarded rather than returned to the pool.
	tainted bool
	// next is the intrusive linked-list pointer used by the engine's idle list.
	next *wasmInstance

	fnAlloc          api.Function
	fnDealloc        api.Function
	fnCreateMatcher  api.Function
	fnDestroyMatcher api.Function
	fnIsMatch        api.Function
	fnBatchFilter    api.Function
}

// engine holds the compiled WASM module and a bounded pool of instances.
type engine struct {
	ctx             context.Context
	instanceCounter atomic.Uint64

	// lazy WASM compilation — only the first getInstance call pays the cost.
	compileOnce sync.Once
	compileErr  error
	runtime     wazero.Runtime
	compiled    wazero.CompiledModule

	// bounded instance pool (go-re2 pattern):
	//   Get: pop LIFO idle list → create if total < max → cond.Wait
	//   Put: tainted → close + total-- + Signal; else → push front + Signal
	mu           sync.Mutex
	cond         *sync.Cond
	maxInstances int
	total        int
	idle         *wasmInstance
}

// engineConfig holds options accumulated by Option functions.
type engineConfig struct {
	maxInstances int
}

// Option configures an Engine.
type Option func(*engineConfig)

// WithMaxInstances sets the desired maximum number of concurrent WASM instances.
// The effective cap is min(n, runtime.NumCPU()) — additional instances beyond
// the CPU count yield no throughput benefit for this CPU-bound workload.
// Callers that exceed the effective cap block until an instance is returned.
// Defaults to runtime.NumCPU().
func WithMaxInstances(n int) Option {
	return func(c *engineConfig) {
		c.maxInstances = n
	}
}

// Engine holds a compiled WASM module and a bounded pool of instances.
// The zero value is not usable; obtain one via NewEngine.
type Engine struct {
	e *engine
}

// NewEngine creates a new Engine with the given options.
// The WASM module is compiled lazily on the first NewMatcher call.
func NewEngine(opts ...Option) *Engine {
	cfg := &engineConfig{maxInstances: runtime.NumCPU()}
	for _, o := range opts {
		o(cfg)
	}
	max := cfg.maxInstances
	if max < 1 {
		max = 1
	}
	if numCPU := runtime.NumCPU(); max > numCPU {
		max = numCPU
	}
	e := &engine{
		ctx:          context.Background(),
		maxInstances: max,
	}
	e.cond = sync.NewCond(&e.mu)
	return &Engine{e: e}
}

// defaultEngine backs the package-level NewMatcher.
var defaultEngine = NewEngine()

// getEngine returns the internal *engine of the default package-level Engine.
// Used by tests that access engine internals directly.
func getEngine() (*engine, error) {
	return defaultEngine.e, nil
}

// ensureCompiled compiles the embedded WASM module on the first call.
// Subsequent calls are no-ops. Safe for concurrent use.
func (e *engine) ensureCompiled() error {
	e.compileOnce.Do(func() {
		r := wazero.NewRuntime(e.ctx)
		wasi_snapshot_preview1.MustInstantiate(e.ctx, r)
		compiled, err := r.CompileModule(e.ctx, matcherWasm)
		if err != nil {
			_ = r.Close(e.ctx)
			e.compileErr = fmt.Errorf("ignore: failed to compile wasm module: %w", err)
			return
		}
		e.runtime = r
		e.compiled = compiled
	})
	return e.compileErr
}

// newInstance creates a fresh WASM module instance with its own linear memory.
func (e *engine) newInstance() (*wasmInstance, error) {
	id := e.instanceCounter.Add(1)
	name := fmt.Sprintf("matcher_%d", id)

	cfg := wazero.NewModuleConfig().
		WithName(name).
		WithStartFunctions("_initialize")

	mod, err := e.runtime.InstantiateModule(e.ctx, e.compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("ignore: failed to instantiate wasm module: %w", err)
	}

	inst := &wasmInstance{mod: mod}

	inst.fnAlloc = mod.ExportedFunction("alloc")
	inst.fnDealloc = mod.ExportedFunction("dealloc")
	inst.fnCreateMatcher = mod.ExportedFunction("create_matcher")
	inst.fnDestroyMatcher = mod.ExportedFunction("destroy_matcher")
	inst.fnIsMatch = mod.ExportedFunction("is_match")
	inst.fnBatchFilter = mod.ExportedFunction("batch_filter")

	if inst.fnAlloc == nil || inst.fnDealloc == nil ||
		inst.fnCreateMatcher == nil || inst.fnDestroyMatcher == nil ||
		inst.fnIsMatch == nil || inst.fnBatchFilter == nil {
		_ = mod.Close(e.ctx)
		return nil, fmt.Errorf("ignore: wasm module is missing required exports")
	}

	return inst, nil
}

// getInstance returns an idle instance (LIFO), creates one if below the cap,
// or blocks until putInstance signals availability.
func (e *engine) getInstance() (*wasmInstance, error) {
	if err := e.ensureCompiled(); err != nil {
		return nil, err
	}

	e.mu.Lock()
	for {
		// Pop the most-recently-used instance from the idle list.
		if e.idle != nil {
			inst := e.idle
			e.idle = inst.next
			inst.next = nil
			e.mu.Unlock()
			return inst, nil
		}
		// Create a new instance if we haven't reached the cap.
		if e.total < e.maxInstances {
			e.total++
			e.mu.Unlock()
			inst, err := e.newInstance()
			if err != nil {
				e.mu.Lock()
				e.total--
				e.cond.Signal()
				e.mu.Unlock()
				return nil, err
			}
			return inst, nil
		}
		// Cap reached — wait for a putInstance to signal.
		e.cond.Wait()
	}
}

// putInstance returns an instance to the idle pool. Tainted instances are
// closed and their capacity slot is freed. A cond.Signal wakes a waiter
// in getInstance in either case.
func (e *engine) putInstance(inst *wasmInstance) {
	e.mu.Lock()
	if inst.tainted {
		e.total--
		e.mu.Unlock()
		_ = inst.mod.Close(e.ctx)
		e.cond.Signal()
		return
	}
	inst.next = e.idle
	e.idle = inst
	e.cond.Signal()
	e.mu.Unlock()
}

// writeString allocates WASM memory, writes s into it, and returns ptr+size.
// The caller must call freeBytes when done.
func (e *engine) writeString(inst *wasmInstance, s string) (ptr uint32, size uint32, err error) {
	if len(s) == 0 {
		return 0, 0, nil
	}

	size = uint32(len(s))
	results, err := inst.fnAlloc.Call(e.ctx, uint64(size))
	if err != nil {
		inst.tainted = true
		return 0, 0, fmt.Errorf("ignore: alloc failed: %w", err)
	}
	ptr = uint32(results[0])
	if ptr == 0 {
		return 0, 0, fmt.Errorf("ignore: alloc returned null (out of memory)")
	}

	if !inst.mod.Memory().Write(ptr, []byte(s)) {
		e.freeBytes(inst, ptr, size)
		return 0, 0, fmt.Errorf("ignore: memory write out of range (ptr=%d, size=%d, mem=%d)",
			ptr, size, inst.mod.Memory().Size())
	}

	return ptr, size, nil
}

// readBytes reads size bytes from WASM memory at ptr.
func (e *engine) readBytes(inst *wasmInstance, ptr, size uint32) ([]byte, error) {
	if ptr == 0 || size == 0 {
		return nil, nil
	}
	buf, ok := inst.mod.Memory().Read(ptr, size)
	if !ok {
		return nil, fmt.Errorf("ignore: memory read out of range (ptr=%d, size=%d, mem=%d)",
			ptr, size, inst.mod.Memory().Size())
	}
	out := make([]byte, len(buf)) // copy: wazero buffer is only valid until the next call
	copy(out, buf)
	return out, nil
}

// freeBytes deallocates a previously allocated block in the WASM instance.
func (e *engine) freeBytes(inst *wasmInstance, ptr, size uint32) {
	if ptr == 0 || size == 0 {
		return
	}
	// Errors during dealloc are non-fatal — the memory will be reclaimed
	// when the instance is closed. Taint so the instance is not reused.
	if _, err := inst.fnDealloc.Call(e.ctx, uint64(ptr), uint64(size)); err != nil {
		inst.tainted = true
	}
}
