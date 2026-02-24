use ignore::gitignore::{Gitignore, GitignoreBuilder};
use std::cell::UnsafeCell;
use std::collections::HashMap;
use std::path::Path;

/// Match result for a single path.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MatchResult {
    None = 0,
    Ignore = 1,
    Whitelist = 2,
}

// SAFETY: WASM is single-threaded; no data races are possible. UnsafeCell + a
// Sync ZST wrapper satisfies `static` requirements without Mutex overhead.
struct SingleThreaded<T>(UnsafeCell<T>);
unsafe impl<T> Sync for SingleThreaded<T> {}

static MATCHERS: SingleThreaded<Option<HashMap<u32, Gitignore>>> =
    SingleThreaded(UnsafeCell::new(None));

pub(crate) fn matchers() -> &'static mut HashMap<u32, Gitignore> {
    // SAFETY: single-threaded WASM; no concurrent access possible.
    let m = unsafe { &mut *MATCHERS.0.get() };
    m.get_or_insert_with(HashMap::new)
}

/// Build a `Gitignore` from a newline-separated pattern byte slice.
/// Lines that fail to parse or are not valid UTF-8 are silently skipped.
pub(crate) fn build_matcher(patterns: &[u8]) -> Result<Gitignore, ignore::Error> {
    let mut builder = GitignoreBuilder::new(Path::new("/"));
    for line_bytes in patterns.split(|&b| b == b'\0') {
        if let Ok(line) = std::str::from_utf8(line_bytes) {
            let _ = builder.add_line(None, line);
        }
    }
    builder.build()
}

/// Match a path against a compiled gitignore matcher.
pub(crate) fn match_path(gitignore: &Gitignore, path: &str, is_dir: bool) -> MatchResult {
    match gitignore.matched_path_or_any_parents(Path::new(path), is_dir) {
        ignore::Match::None => MatchResult::None,
        ignore::Match::Ignore(_) => MatchResult::Ignore,
        ignore::Match::Whitelist(_) => MatchResult::Whitelist,
    }
}

/// Filter a newline-separated path list, returning only non-ignored entries.
/// Paths ending in `/` are treated as directories; empty lines are skipped.
pub(crate) fn filter_paths<'a>(gitignore: &Gitignore, paths: &'a str) -> Vec<&'a str> {
    let mut kept = Vec::new();
    for line in paths.split('\0') {
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

#[cfg(test)]
mod tests {
    use super::*;

    fn matcher(patterns: &[&str]) -> Gitignore {
        build_matcher(patterns.join("\0").as_bytes()).expect("patterns should compile")
    }

    fn matches_file(gi: &Gitignore, path: &str) -> MatchResult {
        match_path(gi, path, false)
    }

    fn matches_dir(gi: &Gitignore, path: &str) -> MatchResult {
        match_path(gi, path, true)
    }

    fn batch(gi: &Gitignore, paths: &[&str]) -> Vec<String> {
        let input = paths.join("\0");
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
        let gi = build_matcher(b"").expect("empty patterns should compile");
        assert!(gi.is_empty());
    }

    #[test]
    fn build_single_pattern() {
        let gi = build_matcher(b"*.log").expect("should compile");
        assert_eq!(gi.num_ignores(), 1);
    }

    #[test]
    fn build_multiple_patterns() {
        let gi = build_matcher(b"*.log\0build/\0temp*").expect("should compile");
        assert_eq!(gi.num_ignores(), 3);
    }

    #[test]
    fn build_with_comments_and_blanks() {
        let gi = build_matcher(b"# this is a comment\0\0*.log\0\0# another comment\0build/")
            .expect("should compile");
        assert_eq!(gi.num_ignores(), 2);
    }

    #[test]
    fn build_with_negation() {
        let gi = build_matcher(b"*.log\0!important.log").expect("should compile");
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
        let input = "a.txt\0\0b.log\0\0c.txt\0";
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
    // MatchResult enum value mapping
    // -----------------------------------------------------------------------

    #[test]
    fn match_result_integer_values() {
        // Verify the discriminant values match what the Go side expects
        assert_eq!(MatchResult::None as i32, 0);
        assert_eq!(MatchResult::Ignore as i32, 1);
        assert_eq!(MatchResult::Whitelist as i32, 2);
    }

    // -----------------------------------------------------------------------
    // Parent-directory matching — matched_path_or_any_parents propagation
    // -----------------------------------------------------------------------

    #[test]
    fn parent_match_target_dir_ignores_children() {
        let gi = matcher(&["target/"]);
        // The directory itself is ignored
        assert_eq!(matches_dir(&gi, "target"), MatchResult::Ignore);
        // Children of an ignored directory are also ignored
        assert_eq!(matches_file(&gi, "target/foo/bar.rs"), MatchResult::Ignore);
        assert_eq!(
            matches_file(&gi, "target/debug/build/output"),
            MatchResult::Ignore
        );
    }

    #[test]
    fn parent_match_node_modules_ignores_children() {
        let gi = matcher(&["node_modules/"]);
        assert_eq!(
            matches_file(&gi, "node_modules/express/index.js"),
            MatchResult::Ignore
        );
        assert_eq!(
            matches_file(&gi, "node_modules/.package-lock.json"),
            MatchResult::Ignore
        );
        // Nested node_modules children too
        assert_eq!(
            matches_dir(&gi, "packages/app/node_modules"),
            MatchResult::Ignore
        );
        assert_eq!(
            matches_file(&gi, "packages/app/node_modules/lodash/index.js"),
            MatchResult::Ignore
        );
    }

    #[test]
    fn parent_match_batch_filter_children_of_ignored_dir() {
        let gi = matcher(&["build/"]);
        let result = batch(
            &gi,
            &[
                "src/main.rs",
                "build/",
                "build/output.o",
                "build/lib/foo.a",
                "README.md",
            ],
        );
        assert_eq!(result, vec!["src/main.rs", "README.md"]);
    }

    #[test]
    fn parent_match_negation_can_whitelist_child() {
        // A negation pattern can re-include a specific file under an ignored
        // directory when using matched_path_or_any_parents.
        let gi = matcher(&["build/", "!build/important.txt"]);
        // The directory itself is ignored
        assert_eq!(matches_dir(&gi, "build"), MatchResult::Ignore);
        // The negation pattern whitelists this specific child
        assert_eq!(
            matches_file(&gi, "build/important.txt"),
            MatchResult::Whitelist
        );
        // Other children are still ignored
        assert_eq!(matches_file(&gi, "build/output.o"), MatchResult::Ignore);
    }
}
