package ignore

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Go→WASM memory: pointer and size correctness
// ---------------------------------------------------------------------------

// TestWriteStringByteAccuracy verifies that writeString allocates exactly
// len(s) bytes and that the bytes written into WASM linear memory are a
// bit-for-bit copy of the input string.
func TestWriteStringByteAccuracy(t *testing.T) {
	eng, err := getEngine()
	require.NoError(t, err)

	inst, err := eng.getInstance()
	require.NoError(t, err)
	defer eng.putInstance(inst)

	input := "*.log\nbuild/\nnode_modules/"

	ptr, size, err := eng.writeString(inst, input)
	require.NoError(t, err)
	defer eng.freeBytes(inst, ptr, size)

	assert.Equal(t, uint32(len(input)), size, "allocated size must equal len(input)")
	assert.NotZero(t, ptr, "ptr must be non-zero")

	got, ok := inst.mod.Memory().Read(ptr, size)
	require.True(t, ok, "memory read must be in range")
	assert.Equal(t, []byte(input), got, "bytes in WASM memory must match input exactly")
}

// TestWriteStringEmptyNoAlloc verifies that an empty string returns (0, 0)
// without touching the WASM allocator.
func TestWriteStringEmptyNoAlloc(t *testing.T) {
	eng, err := getEngine()
	require.NoError(t, err)

	inst, err := eng.getInstance()
	require.NoError(t, err)
	defer eng.putInstance(inst)

	ptr, size, err := eng.writeString(inst, "")
	require.NoError(t, err)
	assert.Zero(t, ptr, "empty string must return ptr=0")
	assert.Zero(t, size, "empty string must return size=0")
	assert.False(t, inst.tainted, "empty string must not taint the instance")
}

// TestWriteSizeMatchesStringLength checks several string lengths to ensure
// the reported size always equals the UTF-8 byte length of the string.
func TestWriteSizeMatchesStringLength(t *testing.T) {
	eng, err := getEngine()
	require.NoError(t, err)

	inst, err := eng.getInstance()
	require.NoError(t, err)
	defer eng.putInstance(inst)

	cases := []string{
		"a",
		"hello",
		strings.Repeat("x", 1024),
		"unicode: \u00e9\u00e0\u00fc", // multi-byte UTF-8
	}

	for _, s := range cases {
		ptr, size, err := eng.writeString(inst, s)
		require.NoError(t, err, "input: %q", s)
		assert.Equal(t, uint32(len(s)), size, "size mismatch for input: %q", s)
		eng.freeBytes(inst, ptr, size)
	}
}

// TestReadBytesRoundTrip writes known bytes via writeString and reads them
// back via readBytes, verifying the two copies are identical.
func TestReadBytesRoundTrip(t *testing.T) {
	eng, err := getEngine()
	require.NoError(t, err)

	inst, err := eng.getInstance()
	require.NoError(t, err)
	defer eng.putInstance(inst)

	input := "src/main.go\ndebug.log\nbuild/"

	ptr, size, err := eng.writeString(inst, input)
	require.NoError(t, err)
	defer eng.freeBytes(inst, ptr, size)

	got, err := eng.readBytes(inst, ptr, size)
	require.NoError(t, err)
	assert.Equal(t, []byte(input), got)
}

// TestReadBytesZeroPtrOrSize verifies that readBytes treats ptr=0 or size=0
// as "nothing to read" and returns nil without error.
func TestReadBytesZeroPtrOrSize(t *testing.T) {
	eng, err := getEngine()
	require.NoError(t, err)

	inst, err := eng.getInstance()
	require.NoError(t, err)
	defer eng.putInstance(inst)

	got, err := eng.readBytes(inst, 0, 16)
	require.NoError(t, err)
	assert.Nil(t, got, "ptr=0 should return nil")

	got, err = eng.readBytes(inst, 1, 0)
	require.NoError(t, err)
	assert.Nil(t, got, "size=0 should return nil")
}

// TestFreeBytesZeroPtrIsNoop verifies that freeBytes with ptr=0 does not
// taint the instance or cause a trap.
func TestFreeBytesZeroPtrIsNoop(t *testing.T) {
	eng, err := getEngine()
	require.NoError(t, err)

	inst, err := eng.getInstance()
	require.NoError(t, err)
	defer eng.putInstance(inst)

	eng.freeBytes(inst, 0, 0)
	eng.freeBytes(inst, 0, 16)
	assert.False(t, inst.tainted, "freeBytes with null ptr must not taint instance")
}

// TestPointerRoundTripCreateAndMatch exercises the full Go→WASM→Go pointer
// path: allocate pattern bytes, pass ptr+size to create_matcher, allocate path
// bytes, pass ptr+size+handle to is_match, and verify the result codes.
func TestPointerRoundTripCreateAndMatch(t *testing.T) {
	eng, err := getEngine()
	require.NoError(t, err)

	inst, err := eng.getInstance()
	require.NoError(t, err)
	defer eng.putInstance(inst)

	pPtr, pSize, err := eng.writeString(inst, "*.log\nbuild/")
	require.NoError(t, err)

	res, err := inst.fnCreateMatcher.Call(eng.ctx, uint64(pPtr), uint64(pSize))
	eng.freeBytes(inst, pPtr, pSize)
	require.NoError(t, err)
	handle := int32(res[0])
	require.Positive(t, handle, "create_matcher must return a positive handle")
	defer func() { _, _ = inst.fnDestroyMatcher.Call(eng.ctx, uint64(handle)) }()

	cases := []struct {
		path   string
		isDir  bool
		expect int32
	}{
		{"debug.log", false, 1}, // ignored
		{"app.go", false, 0},    // not matched
		{"build", true, 1},      // ignored (directory pattern)
		{"build", false, 0},     // not matched (file, pattern is "build/")
	}

	for _, tc := range cases {
		pathPtr, pathSize, err := eng.writeString(inst, tc.path)
		require.NoError(t, err)

		var isDirArg uint64
		if tc.isDir {
			isDirArg = 1
		}

		got, err := inst.fnIsMatch.Call(eng.ctx,
			uint64(handle), uint64(pathPtr), uint64(pathSize), isDirArg)
		eng.freeBytes(inst, pathPtr, pathSize)
		require.NoError(t, err)

		assert.Equal(t, tc.expect, int32(got[0]), "path=%q isDir=%v", tc.path, tc.isDir)
	}
}

// TestBatchFilterResultInfoSize verifies that batch_filter writes exactly 8
// bytes into the result_info buffer (two little-endian i32 fields: ptr, len)
// and that the encoded result pointer and length are consistent with the data
// readable from WASM memory.
func TestBatchFilterResultInfoSize(t *testing.T) {
	eng, err := getEngine()
	require.NoError(t, err)

	inst, err := eng.getInstance()
	require.NoError(t, err)
	defer eng.putInstance(inst)

	pPtr, pSize, err := eng.writeString(inst, "*.log")
	require.NoError(t, err)

	res, err := inst.fnCreateMatcher.Call(eng.ctx, uint64(pPtr), uint64(pSize))
	eng.freeBytes(inst, pPtr, pSize)
	require.NoError(t, err)
	handle := int32(res[0])
	require.Positive(t, handle)
	defer func() { _, _ = inst.fnDestroyMatcher.Call(eng.ctx, uint64(handle)) }()

	bPtr, bSize, err := eng.writeString(inst, "src/main.go\ndebug.log\nREADME.md")
	require.NoError(t, err)
	defer eng.freeBytes(inst, bPtr, bSize)

	// Allocate the 8-byte result_info slot.
	infoRes, err := inst.fnAlloc.Call(eng.ctx, 8)
	require.NoError(t, err)
	infoPtr := uint32(infoRes[0])
	require.NotZero(t, infoPtr)
	defer eng.freeBytes(inst, infoPtr, 8)

	count, err := inst.fnBatchFilter.Call(eng.ctx,
		uint64(handle), uint64(bPtr), uint64(bSize), uint64(infoPtr))
	require.NoError(t, err)
	assert.EqualValues(t, 2, int32(count[0]), "two paths should survive filtering")

	// Read and decode the 8-byte result_info: [resultPtr i32LE][resultLen i32LE].
	info, ok := inst.mod.Memory().Read(infoPtr, 8)
	require.True(t, ok)
	require.Len(t, info, 8, "result_info must be exactly 8 bytes")

	resultPtr := binary.LittleEndian.Uint32(info[0:4])
	resultLen := binary.LittleEndian.Uint32(info[4:8])

	assert.NotZero(t, resultPtr, "result ptr must be non-zero when paths are kept")
	assert.NotZero(t, resultLen, "result len must be non-zero when paths are kept")

	resultBytes, err := eng.readBytes(inst, resultPtr, resultLen)
	require.NoError(t, err)
	assert.Equal(t, "src/main.go\nREADME.md", string(resultBytes))

	eng.freeBytes(inst, resultPtr, resultLen)
}
