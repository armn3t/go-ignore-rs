package ignore

import (
	"context"
	_ "embed"
	"fmt"
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

	fnAlloc          api.Function
	fnDealloc        api.Function
	fnCreateMatcher  api.Function
	fnDestroyMatcher api.Function
	fnIsMatch        api.Function
	fnBatchFilter    api.Function
}

// engine is the package-level singleton holding the compiled WASM module and
// a pool of bare instances ready for use.
type engine struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	pool     sync.Pool
	ctx      context.Context

	// instanceCounter generates unique module names (wazero requires them).
	instanceCounter atomic.Uint64
}

var (
	globalEngine *engine
	engineOnce   sync.Once
	engineErr    error
)

// getEngine returns the singleton engine, compiling the WASM module on first call.
func getEngine() (*engine, error) {
	engineOnce.Do(func() {
		globalEngine, engineErr = newEngine()
	})
	return globalEngine, engineErr
}

func newEngine() (*engine, error) {
	ctx := context.Background()

	r := wazero.NewRuntime(ctx)

	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	compiled, err := r.CompileModule(ctx, matcherWasm)
	if err != nil {
		_ = r.Close(ctx)
		return nil, fmt.Errorf("ignore: failed to compile wasm module: %w", err)
	}

	e := &engine{
		runtime:  r,
		compiled: compiled,
		ctx:      ctx,
	}

	e.pool.New = func() any {
		inst, err := e.newInstance()
		if err != nil {
			return nil // getInstance will retry directly on nil
		}
		return inst
	}

	return e, nil
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

// getInstance retrieves a WASM instance from the pool, or creates one if empty.
func (e *engine) getInstance() (*wasmInstance, error) {
	if v := e.pool.Get(); v != nil {
		return v.(*wasmInstance), nil
	}
	return e.newInstance()
}

// putInstance returns an instance to the pool. All matchers on it must have
// been destroyed first. Linear memory grows but never shrinks; the GC reclaims
// idle instances automatically via sync.Pool eviction.
// Tainted instances (those that experienced a wazero-level Call error) are
// closed and discarded instead.
func (e *engine) putInstance(inst *wasmInstance) {
	if inst.tainted {
		_ = inst.mod.Close(e.ctx)
		return
	}
	e.pool.Put(inst)
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
