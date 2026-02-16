use ignore::gitignore::{Gitignore, GitignoreBuilder};
use std::cell::UnsafeCell;
use std::collections::HashMap;
use std::path::Path;
use std::sync::atomic::{AtomicU32, Ordering};

static NEXT_ID: AtomicU32 = AtomicU32::new(1);

// SAFETY: WASM is single-threaded — there is only one thread of execution,
// so no data races are possible. We wrap in UnsafeCell + a ZST wrapper that
// implements Sync to satisfy the `static` requirements without pulling in
// Mutex overhead.
struct SingleThreaded<T>(UnsafeCell<T>);
unsafe impl<T> Sync for SingleThreaded<T> {}

static MATCHERS: SingleThreaded<Option<HashMap<u32, Gitignore>>> =
    SingleThreaded(UnsafeCell::new(None));

fn matchers() -> &'static mut HashMap<u32, Gitignore> {
    // SAFETY: WASM is single-threaded; no concurrent access is possible.
    let m = unsafe { &mut *MATCHERS.0.get() };
    if m.is_none() {
        *m = Some(HashMap::new());
    }
    m.as_mut().unwrap()
}

// ---------------------------------------------------------------------------
// Core logic (testable, no FFI/pointer concerns)
// ---------------------------------------------------------------------------

/// The result of matching a single path against a gitignore matcher.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MatchResult {
    /// The path did not match any pattern.
    None = 0,
    /// The path matched an ignore pattern and should be excluded.
    Ignore = 1,
    /// The path matched a negation (`!`) pattern and should be kept.
    Whitelist = 2,
}

/// Build a `Gitignore` matcher from a newline-separated pattern string.
///
/// Individual lines that fail to parse are silently skipped, matching the
/// real gitignore behaviour where one bad line doesn't invalidate the file.
fn build_matcher(patterns: &str) -> Result<Gitignore, ignore::Error> {
    let mut builder = GitignoreBuilder::new(Path::new("/"));
    for line in patterns.lines() {
        let _ = builder.add_line(None, line);
    }
    builder.build()
}

/// Match a single path against a compiled gitignore matcher.
fn match_path(gitignore: &Gitignore, path: &str, is_dir: bool) -> MatchResult {
    match gitignore.matched(Path::new(path), is_dir) {
        ignore::Match::None => MatchResult::None,
        ignore::Match::Ignore(_) => MatchResult::Ignore,
        ignore::Match::Whitelist(_) => MatchResult::Whitelist,
    }
}

/// Filter a newline-separated list of paths, returning only those that are
/// NOT ignored (i.e. `None` or `Whitelist`).
///
/// Paths ending in `/` are treated as directories for matching purposes.
/// Empty lines are skipped.
fn filter_paths<'a>(gitignore: &Gitignore, paths: &'a str) -> Vec<&'a str> {
    let mut kept = Vec::new();
    for line in paths.split('\n') {
        if line.is_empty() {
            continue;
        }

        let (path_str, is_dir) = if let Some(stripped) = line.strip_suffix('/') {
            (stripped, true)
        } else {
            (line, false)
        };

        match match_path(gitignore, path_str, is_dir) {
            MatchResult::None | MatchResult::Whitelist => kept.push(line),
            MatchResult::Ignore => {}
        }
    }
    kept
}

// ---------------------------------------------------------------------------
// Memory management exports
// ---------------------------------------------------------------------------

/// Allocate `size` bytes in WASM linear memory and return the pointer.
/// The caller (host) owns this memory and must call `dealloc` to free it.
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
        // Vec is dropped here, freeing the memory
    }
}

// ---------------------------------------------------------------------------
// Matcher lifecycle exports
// ---------------------------------------------------------------------------

/// Create a matcher from newline-separated gitignore patterns stored at
/// `patterns_ptr` for `patterns_len` bytes in WASM memory.
///
/// Returns a handle ID (> 0) on success, or 0 on error.
#[no_mangle]
pub extern "C" fn create_matcher(patterns_ptr: i32, patterns_len: i32) -> i32 {
    if patterns_ptr == 0 || patterns_len < 0 {
        return 0;
    }

    let bytes =
        unsafe { std::slice::from_raw_parts(patterns_ptr as *const u8, patterns_len as usize) };

    let text = match std::str::from_utf8(bytes) {
        Ok(s) => s,
        Err(_) => return 0,
    };

    let gitignore = match build_matcher(text) {
        Ok(gi) => gi,
        Err(_) => return 0,
    };

    let id = NEXT_ID.fetch_add(1, Ordering::Relaxed);
    matchers().insert(id, gitignore);
    id as i32
}

/// Destroy a previously created matcher, freeing its resources.
#[no_mangle]
pub extern "C" fn destroy_matcher(handle: i32) {
    if handle <= 0 {
        return;
    }
    matchers().remove(&(handle as u32));
}

// ---------------------------------------------------------------------------
// Single-path matching export
// ---------------------------------------------------------------------------

/// Test whether a single path matches the patterns in the given matcher.
///
/// `is_dir`: pass 1 if the path is a directory, 0 otherwise.
///
/// Returns:
///   0 = not matched (path is NOT ignored)
///   1 = ignored (path matches an ignore pattern)
///   2 = whitelisted (path matches a negation pattern like `!keep.log`)
///  -1 = error (bad handle or invalid input)
#[no_mangle]
pub extern "C" fn is_match(handle: i32, path_ptr: i32, path_len: i32, is_dir: i32) -> i32 {
    if handle <= 0 || path_ptr == 0 || path_len < 0 {
        return -1;
    }

    let bytes = unsafe { std::slice::from_raw_parts(path_ptr as *const u8, path_len as usize) };

    let path_str = match std::str::from_utf8(bytes) {
        Ok(s) => s,
        Err(_) => return -1,
    };

    let gitignore = match matchers().get(&(handle as u32)) {
        Some(gi) => gi,
        None => return -1,
    };

    match_path(gitignore, path_str, is_dir != 0) as i32
}

// ---------------------------------------------------------------------------
// Batch filtering export
// ---------------------------------------------------------------------------

/// Filter a newline-separated list of paths through the matcher, keeping only
/// paths that are NOT ignored (i.e. Match::None or Match::Whitelist).
///
/// Input:
///   - `handle`: matcher handle from `create_matcher`
///   - `paths_ptr` / `paths_len`: newline-separated paths blob in WASM memory
///   - `result_info_ptr`: pointer to 8 bytes in WASM memory where this function
///     will write two i32 values:
///     bytes 0..4  →  pointer to the result blob (newline-separated kept paths)
///     bytes 4..8  →  length of the result blob in bytes
///     The caller MUST `dealloc(result_ptr, result_len)` after reading.
///
/// Returns: number of kept paths (>= 0) on success, or -1 on error.
///
/// If zero paths are kept, result_ptr and result_len are both set to 0 and
/// the caller should NOT call dealloc.
#[no_mangle]
pub extern "C" fn batch_filter(
    handle: i32,
    paths_ptr: i32,
    paths_len: i32,
    result_info_ptr: i32,
) -> i32 {
    if handle <= 0 || paths_ptr == 0 || paths_len < 0 || result_info_ptr == 0 {
        return -1;
    }

    let bytes = unsafe { std::slice::from_raw_parts(paths_ptr as *const u8, paths_len as usize) };

    let text = match std::str::from_utf8(bytes) {
        Ok(s) => s,
        Err(_) => return -1,
    };

    let gitignore = match matchers().get(&(handle as u32)) {
        Some(gi) => gi,
        None => return -1,
    };

    let kept = filter_paths(gitignore, text);

    let result_info = unsafe { std::slice::from_raw_parts_mut(result_info_ptr as *mut u8, 8) };

    let count = kept.len() as i32;

    if kept.is_empty() {
        // Write zeros: no result buffer allocated
        result_info[0..4].copy_from_slice(&0i32.to_le_bytes());
        result_info[4..8].copy_from_slice(&0i32.to_le_bytes());
        return 0;
    }

    // Join kept paths with newlines into a result blob
    let result_str = kept.join("\n");
    let result_bytes = result_str.into_bytes();
    let result_len = result_bytes.len();

    // Leak the result buffer so it persists for the caller to read.
    // Box<[u8]> has the same layout as Vec with len == capacity,
    // so the caller can dealloc via Vec::from_raw_parts.
    let mut result_buf = result_bytes.into_boxed_slice();
    let result_ptr = result_buf.as_mut_ptr();
    std::mem::forget(result_buf);

    result_info[0..4].copy_from_slice(&(result_ptr as i32).to_le_bytes());
    result_info[4..8].copy_from_slice(&(result_len as i32).to_le_bytes());

    count
}

// ---------------------------------------------------------------------------
// Tests — exercise the core logic functions directly (no FFI / pointer
// concerns, so these run on any host target via `cargo test`).
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    // -----------------------------------------------------------------------
    // Helpers
    // -----------------------------------------------------------------------

    /// Shorthand: build a matcher from a slice of pattern strings.
    fn matcher(patterns: &[&str]) -> Gitignore {
        build_matcher(&patterns.join("\n")).expect("patterns should compile")
    }

    /// Shorthand: match a file path (is_dir = false).
    fn matches_file(gi: &Gitignore, path: &str) -> MatchResult {
        match_path(gi, path, false)
    }

    /// Shorthand: match a directory path (is_dir = true).
    fn matches_dir(gi: &Gitignore, path: &str) -> MatchResult {
        match_path(gi, path, true)
    }

    /// Run batch filter and return the kept paths as a Vec<&str>.
    fn batch(gi: &Gitignore, paths: &[&str]) -> Vec<String> {
        let input = paths.join("\n");
        filter_paths(gi, &input)
            .into_iter()
            .map(String::from)
            .collect()
    }

    // -----------------------------------------------------------------------
    // build_matcher
    // -----------------------------------------------------------------------

    #[test]
    fn build_empty_patterns() {
        let gi = build_matcher("").expect("empty patterns should compile");
        assert!(gi.is_empty());
    }

    #[test]
    fn build_single_pattern() {
        let gi = build_matcher("*.log").expect("should compile");
        assert_eq!(gi.num_ignores(), 1);
    }

    #[test]
    fn build_multiple_patterns() {
        let gi = build_matcher("*.log\nbuild/\ntemp*").expect("should compile");
        assert_eq!(gi.num_ignores(), 3);
    }

    #[test]
    fn build_with_comments_and_blanks() {
        let gi = build_matcher("# this is a comment\n\n*.log\n\n# another comment\nbuild/")
            .expect("should compile");
        assert_eq!(gi.num_ignores(), 2);
    }

    #[test]
    fn build_with_negation() {
        let gi = build_matcher("*.log\n!important.log").expect("should compile");
        assert_eq!(gi.num_ignores(), 1);
        assert_eq!(gi.num_whitelists(), 1);
    }

    // -----------------------------------------------------------------------
    // match_path — basic globs
    // -----------------------------------------------------------------------

    #[test]
    fn match_star_extension() {
        let gi = matcher(&["*.log"]);
        assert_eq!(matches_file(&gi, "debug.log"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "error.log"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "app.txt"), MatchResult::None);
        assert_eq!(matches_file(&gi, "src/debug.log"), MatchResult::Ignore);
    }

    #[test]
    fn match_exact_filename() {
        let gi = matcher(&["Thumbs.db"]);
        assert_eq!(matches_file(&gi, "Thumbs.db"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "src/Thumbs.db"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "thumbs.db"), MatchResult::None);
    }

    #[test]
    fn match_prefix_star() {
        let gi = matcher(&["temp*"]);
        assert_eq!(matches_file(&gi, "tempfile"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "temporary.txt"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "atemp"), MatchResult::None);
    }

    #[test]
    fn match_double_star() {
        let gi = matcher(&["**/logs"]);
        assert_eq!(matches_file(&gi, "logs"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "a/logs"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "a/b/logs"), MatchResult::Ignore);
    }

    #[test]
    fn match_double_star_with_extension() {
        let gi = matcher(&["**/*.log"]);
        assert_eq!(matches_file(&gi, "debug.log"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "a/debug.log"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "a/b/c/debug.log"), MatchResult::Ignore);
    }

    #[test]
    fn match_question_mark() {
        let gi = matcher(&["debug?.log"]);
        assert_eq!(matches_file(&gi, "debug0.log"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "debugA.log"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "debug.log"), MatchResult::None);
        assert_eq!(matches_file(&gi, "debug10.log"), MatchResult::None);
    }

    #[test]
    fn match_character_class() {
        let gi = matcher(&["debug[0-9].log"]);
        assert_eq!(matches_file(&gi, "debug0.log"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "debug9.log"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "debugA.log"), MatchResult::None);
    }

    // -----------------------------------------------------------------------
    // match_path — directory patterns
    // -----------------------------------------------------------------------

    #[test]
    fn match_directory_trailing_slash_pattern() {
        // Pattern "build/" should only match directories, not files named "build"
        let gi = matcher(&["build/"]);
        assert_eq!(matches_dir(&gi, "build"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "build"), MatchResult::None);
        assert_eq!(matches_dir(&gi, "src/build"), MatchResult::Ignore);
    }

    #[test]
    fn match_directory_without_trailing_slash_pattern() {
        // Pattern "build" without trailing slash matches both files and dirs
        let gi = matcher(&["build"]);
        assert_eq!(matches_file(&gi, "build"), MatchResult::Ignore);
        assert_eq!(matches_dir(&gi, "build"), MatchResult::Ignore);
    }

    #[test]
    fn match_nested_directory_pattern() {
        let gi = matcher(&["logs/**/debug.log"]);
        assert_eq!(matches_file(&gi, "logs/debug.log"), MatchResult::Ignore);
        assert_eq!(
            matches_file(&gi, "logs/monday/debug.log"),
            MatchResult::Ignore
        );
        assert_eq!(
            matches_file(&gi, "logs/monday/pm/debug.log"),
            MatchResult::Ignore
        );
    }

    // -----------------------------------------------------------------------
    // match_path — negation patterns
    // -----------------------------------------------------------------------

    #[test]
    fn negation_basic() {
        let gi = matcher(&["*.log", "!important.log"]);
        assert_eq!(matches_file(&gi, "debug.log"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "error.log"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "important.log"), MatchResult::Whitelist);
    }

    #[test]
    fn negation_order_matters() {
        // In gitignore, later patterns override earlier ones
        let gi = matcher(&["*.log", "!important.log", "important.log"]);
        // The last pattern re-ignores important.log
        assert_eq!(matches_file(&gi, "important.log"), MatchResult::Ignore);
    }

    #[test]
    fn negation_of_directory() {
        let gi = matcher(&["build/", "!build/"]);
        assert_eq!(matches_dir(&gi, "build"), MatchResult::Whitelist);
    }

    // -----------------------------------------------------------------------
    // match_path — rooted / anchored patterns
    // -----------------------------------------------------------------------

    #[test]
    fn rooted_pattern_with_leading_slash() {
        // A leading slash anchors the pattern to the root
        let gi = matcher(&["/build"]);
        assert_eq!(matches_file(&gi, "build"), MatchResult::Ignore);
        // Should NOT match in subdirectories
        assert_eq!(matches_file(&gi, "src/build"), MatchResult::None);
    }

    #[test]
    fn pattern_with_middle_slash_is_anchored() {
        // A pattern containing a slash (other than trailing) is anchored
        let gi = matcher(&["doc/frotz"]);
        assert_eq!(matches_file(&gi, "doc/frotz"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "a/doc/frotz"), MatchResult::None);
    }

    // -----------------------------------------------------------------------
    // match_path — edge cases
    // -----------------------------------------------------------------------

    #[test]
    fn empty_matcher_matches_nothing() {
        let gi = matcher(&[]);
        assert_eq!(matches_file(&gi, "anything.txt"), MatchResult::None);
        assert_eq!(matches_dir(&gi, "anydir"), MatchResult::None);
    }

    #[test]
    fn comments_only_matcher_matches_nothing() {
        let gi = matcher(&["# just a comment", "# another comment"]);
        assert_eq!(matches_file(&gi, "anything.txt"), MatchResult::None);
    }

    #[test]
    fn escaped_hash_is_literal() {
        let gi = matcher(&["\\#file"]);
        assert_eq!(matches_file(&gi, "#file"), MatchResult::Ignore);
    }

    #[test]
    fn escaped_bang_is_literal() {
        let gi = matcher(&["\\!important"]);
        assert_eq!(matches_file(&gi, "!important"), MatchResult::Ignore);
    }

    #[test]
    fn trailing_spaces_are_ignored() {
        // Gitignore spec: trailing spaces are ignored unless escaped with backslash
        let gi = matcher(&["*.log   "]);
        assert_eq!(matches_file(&gi, "debug.log"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "debug.log   "), MatchResult::None);
    }

    #[test]
    fn match_deeply_nested_path() {
        let gi = matcher(&["*.log"]);
        assert_eq!(
            matches_file(&gi, "a/b/c/d/e/f/g/deep.log"),
            MatchResult::Ignore
        );
        assert_eq!(
            matches_file(&gi, "a/b/c/d/e/f/g/deep.txt"),
            MatchResult::None
        );
    }

    // -----------------------------------------------------------------------
    // match_path — common real-world patterns
    // -----------------------------------------------------------------------

    #[test]
    fn node_modules_pattern() {
        let gi = matcher(&["node_modules/"]);
        assert_eq!(matches_dir(&gi, "node_modules"), MatchResult::Ignore);
        assert_eq!(
            matches_dir(&gi, "packages/app/node_modules"),
            MatchResult::Ignore
        );
        // File named node_modules (weird but possible) should NOT match
        assert_eq!(matches_file(&gi, "node_modules"), MatchResult::None);
    }

    #[test]
    fn dotfile_pattern() {
        let gi = matcher(&[".*"]);
        assert_eq!(matches_file(&gi, ".gitignore"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, ".env"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "visible.txt"), MatchResult::None);
    }

    #[test]
    fn complex_gitignore() {
        let gi = matcher(&[
            "# Build outputs",
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
        ]);

        assert_eq!(matches_dir(&gi, "build"), MatchResult::Ignore);
        assert_eq!(matches_dir(&gi, "dist"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "main.o"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "lib.a"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "app.log"), MatchResult::Ignore);
        assert_eq!(matches_dir(&gi, "node_modules"), MatchResult::Ignore);
        assert_eq!(matches_dir(&gi, "vendor"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi, "src/main.rs"), MatchResult::None);
        assert_eq!(matches_file(&gi, "README.md"), MatchResult::Whitelist);
    }

    // -----------------------------------------------------------------------
    // filter_paths — batch filtering
    // -----------------------------------------------------------------------

    #[test]
    fn filter_basic() {
        let gi = matcher(&["*.log", "build/"]);
        let result = batch(
            &gi,
            &[
                "src/main.go",
                "debug.log",
                "error.log",
                "build/",
                "README.md",
            ],
        );
        assert_eq!(result, vec!["src/main.go", "README.md"]);
    }

    #[test]
    fn filter_with_negation() {
        let gi = matcher(&["*.log", "!important.log"]);
        let result = batch(
            &gi,
            &["debug.log", "important.log", "error.log", "src/main.go"],
        );
        assert_eq!(result, vec!["important.log", "src/main.go"]);
    }

    #[test]
    fn filter_all_ignored() {
        let gi = matcher(&["*"]);
        let result = batch(&gi, &["a.txt", "b.txt", "c.txt"]);
        assert!(result.is_empty());
    }

    #[test]
    fn filter_none_ignored() {
        let gi = matcher(&["*.log"]);
        let result = batch(&gi, &["a.txt", "b.rs", "c.go"]);
        assert_eq!(result, vec!["a.txt", "b.rs", "c.go"]);
    }

    #[test]
    fn filter_empty_input() {
        let gi = matcher(&["*.log"]);
        let result = batch(&gi, &[]);
        assert!(result.is_empty());
    }

    #[test]
    fn filter_preserves_order() {
        let gi = matcher(&["*.log"]);
        let result = batch(&gi, &["z.txt", "a.txt", "m.txt", "debug.log", "b.txt"]);
        assert_eq!(result, vec!["z.txt", "a.txt", "m.txt", "b.txt"]);
    }

    #[test]
    fn filter_directory_detection_via_trailing_slash() {
        // "build/" pattern only matches directories.
        // In batch_filter, entries ending with "/" are treated as directories.
        let gi = matcher(&["build/"]);
        let result = batch(
            &gi,
            &[
                "build/", // directory → should be ignored
                "build",  // file → should NOT be ignored
                "src/main.go",
            ],
        );
        assert_eq!(result, vec!["build", "src/main.go"]);
    }

    #[test]
    fn filter_skips_empty_lines() {
        let gi = matcher(&["*.log"]);
        // Simulate empty lines in the input (would appear as "" between newlines)
        let input = "a.txt\n\nb.log\n\nc.txt\n";
        let result: Vec<&str> = filter_paths(&gi, input);
        assert_eq!(result, vec!["a.txt", "c.txt"]);
    }

    #[test]
    fn filter_large_pattern_set() {
        // Simulate a realistic .gitignore with many patterns
        let patterns: Vec<&str> = vec![
            "*.o",
            "*.a",
            "*.so",
            "*.dylib",
            "*.dll",
            "*.exe",
            "*.log",
            "*.tmp",
            "*.swp",
            "*.swo",
            "*.bak",
            "*.orig",
            "build/",
            "dist/",
            "target/",
            "out/",
            "node_modules/",
            "vendor/",
            ".git/",
            ".DS_Store",
            "Thumbs.db",
            "*.pyc",
            "__pycache__/",
        ];
        let gi = matcher(&patterns);

        let paths = vec![
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
        ];

        let result = batch(&gi, &paths);
        assert_eq!(
            result,
            vec![
                "src/main.rs",
                "src/lib.rs",
                "Cargo.toml",
                "README.md",
                "src/utils.rs",
                "docs/guide.md",
                "tests/test_main.rs",
            ]
        );
    }

    // -----------------------------------------------------------------------
    // Multiple matchers coexisting
    // -----------------------------------------------------------------------

    #[test]
    fn multiple_matchers_independent() {
        let gi1 = matcher(&["*.log"]);
        let gi2 = matcher(&["*.txt"]);

        assert_eq!(matches_file(&gi1, "debug.log"), MatchResult::Ignore);
        assert_eq!(matches_file(&gi1, "readme.txt"), MatchResult::None);

        assert_eq!(matches_file(&gi2, "debug.log"), MatchResult::None);
        assert_eq!(matches_file(&gi2, "readme.txt"), MatchResult::Ignore);
    }

    // -----------------------------------------------------------------------
    // Global state (matchers HashMap) — tested via the thin layer just
    // above the FFI boundary that we can call safely in tests.
    // -----------------------------------------------------------------------

    #[test]
    fn store_and_retrieve_matcher() {
        let gi = build_matcher("*.log").unwrap();
        let id = NEXT_ID.fetch_add(1, Ordering::Relaxed);
        matchers().insert(id, gi);

        let retrieved = matchers().get(&id).expect("matcher should exist");
        assert_eq!(
            match_path(retrieved, "debug.log", false),
            MatchResult::Ignore
        );

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

    // -----------------------------------------------------------------------
    // MatchResult enum value mapping
    // -----------------------------------------------------------------------

    #[test]
    fn match_result_integer_values() {
        // Verify the discriminant values match what the Go side expects
        assert_eq!(MatchResult::None as i32, 0);
        assert_eq!(MatchResult::Ignore as i32, 1);
        assert_eq!(MatchResult::Whitelist as i32, 2);
    }
}
