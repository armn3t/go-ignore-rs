package ignore

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Engine construction and option validation
// ---------------------------------------------------------------------------

func TestNewEngineDefaultCap(t *testing.T) {
	eng := NewEngine()
	assert.Equal(t, runtime.NumCPU(), eng.e.maxInstances,
		"default cap should equal runtime.NumCPU()")
}

func TestWithMaxInstancesClamping(t *testing.T) {
	numCPU := runtime.NumCPU()
	tests := []struct {
		n    int
		want int
	}{
		{-1, 1},                // below minimum → clamp to 1
		{0, 1},                 // zero → clamp to 1
		{1, 1},                 // exactly 1
		{numCPU, numCPU},       // at CPU count → no change
		{numCPU + 100, numCPU}, // above CPU count → clamp to numCPU
	}
	for _, tc := range tests {
		eng := NewEngine(WithMaxInstances(tc.n))
		assert.Equal(t, tc.want, eng.e.maxInstances,
			"WithMaxInstances(%d)", tc.n)
	}
}

// ---------------------------------------------------------------------------
// (*Engine).NewMatcher — basic correctness
// ---------------------------------------------------------------------------

func TestEngineNewMatcherBasic(t *testing.T) {
	eng := NewEngine()
	m, err := eng.NewMatcher([]string{"*.log", "build/"})
	require.NoError(t, err)
	defer func() { _ = m.Close() }()

	assert.True(t, m.Match("debug.log"))
	assert.False(t, m.Match("main.go"))
	assert.True(t, m.MatchDir("build"))
}

// ---------------------------------------------------------------------------
// Instance cap enforcement — NewMatcher blocks when cap is reached
// ---------------------------------------------------------------------------

func TestEngineCapBlocksAndUnblocks(t *testing.T) {
	// Single-instance engine: only one Matcher may hold an instance at a time.
	eng := NewEngine(WithMaxInstances(1))

	m1, err := eng.NewMatcher([]string{"*.log"})
	require.NoError(t, err)

	// A second NewMatcher must block while m1 is open.
	acquired := make(chan struct{})
	go func() {
		m2, err := eng.NewMatcher([]string{"*.tmp"})
		if err != nil {
			return
		}
		defer func() { _ = m2.Close() }()
		close(acquired)
	}()

	// Give the goroutine enough time to reach getInstance and block.
	select {
	case <-acquired:
		t.Fatal("second NewMatcher must block while first holds the only instance")
	case <-time.After(50 * time.Millisecond):
		// expected: goroutine is waiting on cond.Wait
	}

	// Releasing m1 must unblock the goroutine.
	require.NoError(t, m1.Close())

	select {
	case <-acquired:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("second NewMatcher did not unblock after first was closed")
	}
}

// ---------------------------------------------------------------------------
// Pool reuse — total never exceeds maxInstances
// ---------------------------------------------------------------------------

func TestEngineCapNeverExceeded(t *testing.T) {
	const cap = 3
	eng := NewEngine(WithMaxInstances(cap))
	e := eng.e

	// Open cap matchers concurrently.
	matchers := make([]*Matcher, cap)
	for i := range matchers {
		m, err := eng.NewMatcher([]string{"*.log"})
		require.NoError(t, err)
		matchers[i] = m
	}

	// All cap slots should be in use.
	e.mu.Lock()
	assert.Equal(t, cap, e.total)
	assert.Nil(t, e.idle)
	e.mu.Unlock()

	// Return them all.
	for _, m := range matchers {
		require.NoError(t, m.Close())
	}

	// All slots should now be idle.
	e.mu.Lock()
	assert.Equal(t, cap, e.total)
	idleCount := 0
	for node := e.idle; node != nil; node = node.next {
		idleCount++
	}
	assert.Equal(t, cap, idleCount)
	e.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Tainted instance recovery — total decrements and cap slot is freed
// ---------------------------------------------------------------------------

func TestEngineTaintedInstanceFreesSlot(t *testing.T) {
	eng := NewEngine(WithMaxInstances(1))
	e := eng.e

	inst, err := e.getInstance()
	require.NoError(t, err)

	e.mu.Lock()
	assert.Equal(t, 1, e.total)
	e.mu.Unlock()

	inst.tainted = true
	e.putInstance(inst)

	e.mu.Lock()
	assert.Equal(t, 0, e.total, "tainted instance must free its slot")
	e.mu.Unlock()

	// A new Matcher can now be created.
	m, err := eng.NewMatcher([]string{"*.log"})
	require.NoError(t, err)
	defer func() { _ = m.Close() }()
	assert.True(t, m.Match("debug.log"))
}
