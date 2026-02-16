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

// wasmInstance wraps a single wazero module instance with cached function
// references. Each instance has its own linear memory and is NOT safe for
// concurrent use.
type wasmInstance struct {
	mod api.Module

	// Cached exported function references — avoids map lookup on every call.
	fnAlloc          api.Function
	fnDealloc        api.Function
	fnCreateMatcher  api.Function
	fnDestroyMatcher api.Function
	fnIsMatch        api.Function
	fnBatchFilter    api.Function
}

// engine is the package-level singleton that holds the compiled WASM module
// and a pool of bare instances ready for use.
type engine struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	pool     sync.Pool
	ctx      context.Context

	// instanceCounter provides unique names for module instances.
	// wazero requires unique names when multiple instances coexist
	// under the same runtime.
	instanceCounter atomic.Uint64
}

var (
	globalEngine *engine
	engineOnce   sync.Once
	engineErr    error
)

// getEngine returns the package-level engine singleton, initializing it on
// first call. The WASM module is compiled once and reused for all instances.
func getEngine() (*engine, error) {
	engineOnce.Do(func() {
		globalEngine, engineErr = newEngine()
	})
	return globalEngine, engineErr
}

func newEngine() (*engine, error) {
	ctx := context.Background()

	r := wazero.NewRuntime(ctx)

	// WASI is required because we compiled with wasm32-wasip1.
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	compiled, err := r.CompileModule(ctx, matcherWasm)
	if err != nil {
		r.Close(ctx)
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
			// sync.Pool.New cannot return errors, so we return nil and
			// the caller (getInstance) will detect it and create directly.
			return nil
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

	// Cache all exported function references.
	inst.fnAlloc = mod.ExportedFunction("alloc")
	inst.fnDealloc = mod.ExportedFunction("dealloc")
	inst.fnCreateMatcher = mod.ExportedFunction("create_matcher")
	inst.fnDestroyMatcher = mod.ExportedFunction("destroy_matcher")
	inst.fnIsMatch = mod.ExportedFunction("is_match")
	inst.fnBatchFilter = mod.ExportedFunction("batch_filter")

	// Verify all exports exist.
	if inst.fnAlloc == nil || inst.fnDealloc == nil ||
		inst.fnCreateMatcher == nil || inst.fnDestroyMatcher == nil ||
		inst.fnIsMatch == nil || inst.fnBatchFilter == nil {
		mod.Close(e.ctx)
		return nil, fmt.Errorf("ignore: wasm module is missing required exports")
	}

	return inst, nil
}

// getInstance retrieves a bare WASM instance from the pool, or creates a new
// one if the pool is empty.
func (e *engine) getInstance() (*wasmInstance, error) {
	if v := e.pool.Get(); v != nil {
		return v.(*wasmInstance), nil
	}
	// Pool.New returned nil (error during creation), try directly.
	return e.newInstance()
}

// putInstance returns a bare WASM instance to the pool for reuse.
// The caller must ensure all matchers on this instance have been destroyed.
func (e *engine) putInstance(inst *wasmInstance) {
	e.pool.Put(inst)
}

// writeString allocates memory in the WASM instance, writes the string into
// it, and returns the pointer and length. The caller MUST call freeBytes when
// done with the pointer.
func (e *engine) writeString(inst *wasmInstance, s string) (ptr uint32, size uint32, err error) {
	if len(s) == 0 {
		return 0, 0, nil
	}

	size = uint32(len(s))
	results, err := inst.fnAlloc.Call(e.ctx, uint64(size))
	if err != nil {
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

// readBytes reads `size` bytes from the WASM instance's memory at `ptr`.
func (e *engine) readBytes(inst *wasmInstance, ptr, size uint32) ([]byte, error) {
	if ptr == 0 || size == 0 {
		return nil, nil
	}
	buf, ok := inst.mod.Memory().Read(ptr, size)
	if !ok {
		return nil, fmt.Errorf("ignore: memory read out of range (ptr=%d, size=%d, mem=%d)",
			ptr, size, inst.mod.Memory().Size())
	}
	// Make a copy — the wazero memory buffer is only valid until the next call.
	out := make([]byte, len(buf))
	copy(out, buf)
	return out, nil
}

// freeBytes deallocates a previously allocated block in the WASM instance.
func (e *engine) freeBytes(inst *wasmInstance, ptr, size uint32) {
	if ptr == 0 || size == 0 {
		return
	}
	// Errors during dealloc are non-fatal — the memory will be reclaimed
	// when the instance is closed.
	_, _ = inst.fnDealloc.Call(e.ctx, uint64(ptr), uint64(size))
}
