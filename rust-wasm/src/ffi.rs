use crate::matcher::{build_matcher, filter_paths, match_path, matchers};
use std::sync::atomic::{AtomicU32, Ordering};

// NEXT_ID increments monotonically. Once it would exceed i32::MAX the counter
// is left unchanged and create_matcher returns -4, preventing wrap-around
// collisions with still-live handles.
static NEXT_ID: AtomicU32 = AtomicU32::new(1);

/// Atomically allocate the next matcher ID from `counter`, or return `None`
/// if the space is exhausted. Uses a compare-exchange loop so the counter is
/// never incremented past `i32::MAX`, preventing wrap-around collisions with
/// live handles.
fn alloc_id_from(counter: &AtomicU32) -> Option<u32> {
    let mut current = counter.load(Ordering::Relaxed);
    loop {
        if current > i32::MAX as u32 {
            return None;
        }
        match counter.compare_exchange_weak(
            current,
            current + 1,
            Ordering::Relaxed,
            Ordering::Relaxed,
        ) {
            Ok(_) => return Some(current),
            Err(actual) => current = actual,
        }
    }
}

fn alloc_id() -> Option<u32> {
    alloc_id_from(&NEXT_ID)
}

/// Read a byte slice from WASM linear memory.
///
/// Returns `Err` when:
/// - `len` is negative
/// - `len > 0` and `ptr` is null or negative (ptr is not validated when len == 0)
/// - `ptr + len` overflows the address space
///
/// # Safety
/// Caller must guarantee that `ptr..ptr + len` is a live, valid region of
/// WASM linear memory for the duration of the returned slice's lifetime.
unsafe fn wasm_slice<'a>(ptr: i32, len: i32) -> Result<&'a [u8], &'static str> {
    if len < 0 {
        return Err("negative length");
    }
    if len == 0 {
        return Ok(&[]);
    }
    if ptr == 0 {
        return Err("null pointer");
    }
    if ptr < 0 {
        return Err("negative pointer");
    }
    let ptr = ptr as usize as *const u8;
    // Alignment: always true for u8, but guards against future type changes.
    if ptr.align_offset(std::mem::align_of::<u8>()) != 0 {
        return Err("misaligned pointer");
    }
    // Guard against ptr + len wrapping around the address space.
    if (ptr as usize).checked_add(len as usize).is_none() {
        return Err("pointer + length overflows address space");
    }
    Ok(std::slice::from_raw_parts(ptr, len as usize))
}

/// Read a mutable byte slice from WASM linear memory.
///
/// Returns `Err` when:
/// - `ptr` is null or negative (negative values cast to a large usize and may
///   evade the overflow guard)
/// - `ptr + len` overflows the address space
///
/// # Safety
/// Caller must guarantee that `ptr..ptr + len` is a live, valid region of
/// WASM linear memory with no other live references to the same range for
/// the duration of the returned slice's lifetime.
unsafe fn wasm_slice_mut<'a>(ptr: i32, len: usize) -> Result<&'a mut [u8], &'static str> {
    if ptr <= 0 {
        return Err("null or negative pointer");
    }
    let ptr = ptr as usize as *mut u8;
    if ptr.align_offset(std::mem::align_of::<u8>()) != 0 {
        return Err("misaligned pointer");
    }
    if (ptr as usize).checked_add(len).is_none() {
        return Err("pointer + length overflows address space");
    }
    Ok(std::slice::from_raw_parts_mut(ptr, len))
}

/// Allocate `size` bytes in WASM linear memory. Caller must call `dealloc`.
#[no_mangle]
pub extern "C" fn alloc(size: i32) -> i32 {
    if size <= 0 {
        return 0;
    }
    let size = size as usize;
    let mut buf = vec![0u8; size];
    let ptr = buf.as_mut_ptr();
    std::mem::forget(buf);
    ptr as i32
}

/// Free a previously allocated block at `ptr` of `size` bytes.
#[no_mangle]
pub extern "C" fn dealloc(ptr: i32, size: i32) {
    if ptr == 0 || size <= 0 {
        return;
    }
    unsafe {
        let _ = Vec::from_raw_parts(ptr as *mut u8, size as usize, size as usize);
    }
}

/// Create a matcher from null-byte-separated gitignore patterns in WASM memory.
/// Non-UTF-8 lines are silently skipped.
///
/// Returns a handle (> 0) on success, or:
///  -1 = patterns_len is negative
///  -2 = patterns_ptr is null or negative when patterns_len > 0
///  -3 = builder.build() failed
///  -4 = max matchers created on this instance
#[no_mangle]
pub extern "C" fn create_matcher(patterns_ptr: i32, patterns_len: i32) -> i32 {
    if patterns_len < 0 {
        return -1;
    }

    // SAFETY: patterns_len >= 0 is guaranteed by the guard above;
    // ptr validity is the caller's responsibility (WASM linear memory).
    let bytes = match unsafe { wasm_slice(patterns_ptr, patterns_len) } {
        Ok(b) => b,
        Err(_) => return -2,
    };

    let gitignore = match build_matcher(bytes) {
        Ok(gi) => gi,
        Err(_) => return -3,
    };

    let id = match alloc_id() {
        Some(id) => id,
        None => return -4,
    };
    matchers().insert(id, gitignore);
    id as i32
}

/// Destroy a previously created matcher.
#[no_mangle]
pub extern "C" fn destroy_matcher(handle: i32) {
    if handle <= 0 {
        return;
    }
    matchers().remove(&(handle as u32));
}

/// Test whether a path matches the patterns in the given matcher.
/// `is_dir`: 1 if the path is a directory, 0 otherwise.
///
/// Returns:
///   0 = not matched,  1 = ignored,  2 = whitelisted (negation pattern)
///  -1 = handle not positive,  -2 = invalid path_ptr (null or negative) or negative path_len
///  -3 = path not valid UTF-8,  -4 = handle not found
#[no_mangle]
pub extern "C" fn is_match(handle: i32, path_ptr: i32, path_len: i32, is_dir: i32) -> i32 {
    if handle <= 0 {
        return -1;
    }

    // SAFETY: ptr validity is the caller's responsibility (WASM linear memory).
    let bytes = match unsafe { wasm_slice(path_ptr, path_len) } {
        Ok(b) => b,
        Err(_) => return -2,
    };
    let path_str = match std::str::from_utf8(bytes) {
        Ok(s) => s,
        Err(_) => return -3,
    };

    let matchers = matchers();
    let Some(gitignore) = matchers.get(&(handle as u32)) else {
        return -4;
    };

    match_path(gitignore, path_str, is_dir != 0) as i32
}

/// Filter a null-byte-separated path list, keeping only non-ignored entries.
/// `result_info_ptr` points to 8 WASM bytes where the result ptr+len are written;
/// caller must `dealloc(result_ptr, result_len)` after reading (unless count==0).
///
/// Returns count of kept paths (>= 0), or:
///  -1 = handle not positive,  -2 = invalid result_info_ptr (null or negative)
///  -3 = invalid paths_ptr or negative paths_len,  -4 = paths not valid UTF-8
///  -5 = handle not found,  -6 = result count or byte length exceeds i32::MAX
///  -7 = result_info_ptr + 8 overflows address space
#[no_mangle]
pub extern "C" fn batch_filter(
    handle: i32,
    paths_ptr: i32,
    paths_len: i32,
    result_info_ptr: i32,
) -> i32 {
    if handle <= 0 {
        return -1;
    }

    if result_info_ptr <= 0 {
        return -2;
    }

    // SAFETY: ptr validity is the caller's responsibility (WASM linear memory).
    let bytes = match unsafe { wasm_slice(paths_ptr, paths_len) } {
        Ok(b) => b,
        Err(_) => return -3,
    };
    let text = match std::str::from_utf8(bytes) {
        Ok(s) => s,
        Err(_) => return -4,
    };

    let kept = {
        let matchers = matchers();
        let Some(gitignore) = matchers.get(&(handle as u32)) else {
            return -5;
        };
        filter_paths(gitignore, text)
        // guard drops here, releasing the lock
    };

    // SAFETY: result_info_ptr is guaranteed non-null by the guard above;
    // the host always allocates 8 bytes for this output slot.
    let result_info = match unsafe { wasm_slice_mut(result_info_ptr, 8) } {
        Ok(s) => s,
        Err(_) => return -7, // only reachable if result_info_ptr + 8 overflows
    };

    if kept.is_empty() {
        result_info[0..4].copy_from_slice(&0i32.to_le_bytes());
        result_info[4..8].copy_from_slice(&0i32.to_le_bytes());
        return 0;
    }

    let count: i32 = match kept.len().try_into() {
        Ok(n) => n,
        Err(_) => return -6,
    };

    let result_str = kept.join("\0");
    let result_bytes = result_str.into_bytes();
    let result_len: i32 = match result_bytes.len().try_into() {
        Ok(n) => n,
        Err(_) => return -6,
    };

    // Leak the buffer; caller must dealloc via Vec::from_raw_parts.
    let mut result_buf = result_bytes.into_boxed_slice();
    let result_ptr = result_buf.as_mut_ptr();
    std::mem::forget(result_buf);

    result_info[0..4].copy_from_slice(&(result_ptr as i32).to_le_bytes());
    result_info[4..8].copy_from_slice(&result_len.to_le_bytes());

    count
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::matcher::{build_matcher, match_path, matchers, MatchResult};

    // -----------------------------------------------------------------------
    // wasm_slice — pointer/length validation
    // -----------------------------------------------------------------------

    #[test]
    fn wasm_slice_zero_len_returns_empty() {
        // ptr is ignored when len == 0
        assert_eq!(unsafe { wasm_slice(0, 0) }, Ok([].as_slice()));
        assert_eq!(unsafe { wasm_slice(1, 0) }, Ok([].as_slice()));
    }

    #[test]
    fn wasm_slice_negative_len_returns_err() {
        assert!(unsafe { wasm_slice(1, -1) }.is_err());
        assert!(unsafe { wasm_slice(1, i32::MIN) }.is_err());
    }

    #[test]
    fn wasm_slice_null_ptr_with_positive_len_returns_err() {
        assert!(unsafe { wasm_slice(0, 1) }.is_err());
        assert!(unsafe { wasm_slice(0, 100) }.is_err());
    }

    #[test]
    fn wasm_slice_negative_ptr_returns_err() {
        // Negative i32 pointers cast to large usize values and can slip past
        // the overflow guard (e.g. ptr=-256, len=255 does not overflow u32::MAX
        // on a 32-bit target). Reject them before the cast.
        assert!(unsafe { wasm_slice(-1, 1) }.is_err());
        assert!(unsafe { wasm_slice(-256, 255) }.is_err());
        assert!(unsafe { wasm_slice(i32::MIN, 1) }.is_err());
    }

    #[test]
    fn wasm_slice_mut_negative_ptr_returns_err() {
        assert!(unsafe { wasm_slice_mut(-1, 1) }.is_err());
        assert!(unsafe { wasm_slice_mut(-256, 255) }.is_err());
        assert!(unsafe { wasm_slice_mut(i32::MIN, 1) }.is_err());
    }

    // Valid-pointer round-trip tests require a 32-bit address space where
    // pointer values fit in i32 without truncation.
    #[cfg(target_arch = "wasm32")]
    #[test]
    fn wasm_slice_valid_ptr_returns_bytes() {
        let data: &[u8] = b"hello";
        let result = unsafe { wasm_slice(data.as_ptr() as i32, data.len() as i32) };
        assert_eq!(result, Ok(data));
    }

    // -----------------------------------------------------------------------
    // Global state — tested via the thin layer just above the FFI boundary
    // -----------------------------------------------------------------------

    #[test]
    fn store_and_retrieve_matcher() {
        let gi = build_matcher(b"*.log").unwrap();
        let id = NEXT_ID.fetch_add(1, Ordering::Relaxed);
        matchers().insert(id, gi);

        {
            let matchers = matchers();
            let retrieved = matchers.get(&id).expect("matcher should exist");
            assert_eq!(
                match_path(retrieved, "debug.log", false),
                MatchResult::Ignore
            );
        }

        matchers().remove(&id);
        assert!(matchers().get(&id).is_none());
    }

    #[test]
    fn destroy_nonexistent_handle_is_noop() {
        // Shouldn't panic or corrupt state
        let before = matchers().len();
        matchers().remove(&999999);
        assert_eq!(matchers().len(), before);
    }

    #[test]
    fn overflow_id_does_not_advance_counter() {
        // Use a local counter to avoid racing with other tests that call
        // create_matcher. Verifies that alloc_id_from never increments past
        // i32::MAX and returns None once the space is exhausted.
        let counter = AtomicU32::new(i32::MAX as u32 + 1);
        assert!(
            alloc_id_from(&counter).is_none(),
            "exhausted counter must return None"
        );
        assert_eq!(
            counter.load(Ordering::Relaxed),
            i32::MAX as u32 + 1,
            "counter must not increment past i32::MAX"
        );

        // Also verify the last valid ID is allocated correctly.
        let counter = AtomicU32::new(i32::MAX as u32);
        assert_eq!(alloc_id_from(&counter), Some(i32::MAX as u32));
        assert!(
            alloc_id_from(&counter).is_none(),
            "next call must be exhausted"
        );
    }

    // -----------------------------------------------------------------------
    // FFI boundary — create_matcher error paths
    // -----------------------------------------------------------------------

    #[test]
    fn ffi_create_matcher_negative_len_returns_minus_one() {
        assert_eq!(create_matcher(1, -1), -1);
        assert_eq!(create_matcher(1, i32::MIN), -1);
    }

    #[test]
    fn ffi_create_matcher_null_ptr_positive_len_returns_minus_two() {
        assert_eq!(create_matcher(0, 1), -2);
        assert_eq!(create_matcher(0, 100), -2);
    }

    #[test]
    fn ffi_create_matcher_zero_len_succeeds_without_ptr() {
        // len == 0 → no pointer needed; should build an empty matcher
        let handle = create_matcher(0, 0);
        assert!(handle > 0, "expected valid handle, got {handle}");
        destroy_matcher(handle);
    }

    // alloc() casts the heap pointer to i32, which truncates on 64-bit hosts.
    // These tests can only run correctly on the 32-bit WASM target.
    #[cfg(target_arch = "wasm32")]
    #[test]
    fn ffi_create_matcher_valid_ptr_succeeds() {
        let pattern = b"*.log";
        let ptr = alloc(pattern.len() as i32);
        unsafe { std::ptr::copy_nonoverlapping(pattern.as_ptr(), ptr as *mut u8, pattern.len()) };

        let handle = create_matcher(ptr, pattern.len() as i32);
        assert!(handle > 0, "expected valid handle, got {handle}");

        destroy_matcher(handle);
        dealloc(ptr, pattern.len() as i32);
    }

    // -----------------------------------------------------------------------
    // FFI boundary — is_match error paths
    // -----------------------------------------------------------------------

    #[test]
    fn ffi_is_match_null_ptr_positive_len_returns_minus_two() {
        let handle = create_matcher(0, 0);
        assert_eq!(is_match(handle, 0, 1, 0), -2);
        assert_eq!(is_match(handle, 0, 100, 0), -2);
        destroy_matcher(handle);
    }

    #[test]
    fn ffi_is_match_negative_len_returns_minus_two() {
        let handle = create_matcher(0, 0);
        assert_eq!(is_match(handle, 1, -1, 0), -2);
        assert_eq!(is_match(handle, 1, i32::MIN, 0), -2);
        destroy_matcher(handle);
    }

    #[test]
    fn ffi_is_match_zero_len_path_returns_no_match() {
        let handle = create_matcher(0, 0);
        // Empty path with a zero-pattern matcher → no match (0)
        assert_eq!(is_match(handle, 0, 0, 0), 0);
        destroy_matcher(handle);
    }

    // -----------------------------------------------------------------------
    // FFI boundary — batch_filter error paths
    // -----------------------------------------------------------------------

    #[test]
    fn ffi_batch_filter_null_result_info_ptr_returns_minus_two() {
        let handle = create_matcher(0, 0);
        assert_eq!(batch_filter(handle, 0, 0, 0), -2);
        destroy_matcher(handle);
    }

    #[test]
    fn ffi_batch_filter_null_paths_ptr_positive_len_returns_minus_three() {
        let handle = create_matcher(0, 0);
        // The function returns -3 before reading result_info, so any non-null
        // sentinel works without requiring a valid allocation.
        assert_eq!(batch_filter(handle, 0, 1, 1), -3);
        assert_eq!(batch_filter(handle, 0, 100, 1), -3);
        destroy_matcher(handle);
    }

    #[test]
    fn ffi_batch_filter_negative_len_returns_minus_three() {
        let handle = create_matcher(0, 0);
        // Same as above: early return before result_info is touched.
        assert_eq!(batch_filter(handle, 1, -1, 1), -3);
        assert_eq!(batch_filter(handle, 1, i32::MIN, 1), -3);
        destroy_matcher(handle);
    }
}
