# 57 — `EncodeCwd`: canonicalise via realpath before encoding

## Files to read first

- `pkg/tuidriver/cwd.go` (whole file — 26 lines) — current implementation. The byte-transform body stays; the only new wiring is a canonicalisation step in front of it.
- `pkg/tuidriver/cwd_test.go` (whole file — 33 lines) — existing table-driven test. Some cases need annotation or rewriting because the new behaviour reads the filesystem (see § Existing tests).
- `cmd/spike-long-prompt/README.md:200-201` — the operator-triage bullet to delete (the case-sensitivity warning under § *Surprises / findings*).
- `cmd/spike-one-turn/main.go:280-300` (and any one other spike, e.g. `cmd/probe-first-prompt-hang/main.go:125-140`) — confirm the consumer call shape: every caller is `filepath.Join(home, ".claude", "projects", tuidriver.EncodeCwd(cwd))`. **No consumer edits are needed** — the change is behaviour-compatible because non-existent inputs fall back to today's byte-transform output (AC bullet 2).
- `docs/knowledge/architecture/system-overview.md` § "Concurrency model" + "Key signals" — not load-bearing for this ticket, but useful for orientation if you're new to the library.
- Ticket #57 body's § "Technical Notes" — names three viable canonicalisation mechanisms on macOS. This spec picks one and explains why; if you find a fourth, raise it before implementing.

## Context

`tuidriver.EncodeCwd` today is a pure byte-transform: every non-`[a-zA-Z0-9]` byte maps to `-`. It does not touch the filesystem. When the process is started under a path whose casing differs from the canonical on-disk form (default macOS APFS, default Windows) or under a symlink, the encoded name does not match what `claude` writes under `~/.claude/projects/`, and the deterministic JSONL path computed by every spike binary points at a directory that does not exist. The symptom is the 10 s `os.Stat` poll in each spike timing out with `session JSONL did not appear`. The triage workaround (`cd "$(pwd -P)"`) is documented in `cmd/spike-long-prompt/README.md` as a known gotcha, but it shifts a library defect onto operators.

The fix at source: `EncodeCwd` resolves its input to the on-disk canonical form (symlinks resolved AND case canonicalised on case-insensitive filesystems) before applying the existing byte-by-byte transform. The encoder's contract becomes "produce the path claude actually writes to," which by definition requires a fully canonical input path.

This is a behavioural change but not a wire-format change. The existing 7 spikes and 1 probe call `EncodeCwd` and do not depend on lexical-only semantics. No in-tree consumer relies on the old behaviour. The change is breaking only in the sense that a caller passing a path that exists on disk under a different casing now gets a different (correct) output.

## Design

### Canonicalisation mechanism: `F_GETPATH` on darwin, `EvalSymlinks` elsewhere

Three mechanisms were considered (per ticket § Technical Notes):

| Mechanism | Symlinks | macOS case | Library-safe | Code surface |
|---|---|---|---|---|
| `filepath.EvalSymlinks` | yes | **no** (fails AC 3) | yes | tiny |
| `os.Chdir` + `os.Getwd` | yes | yes | **no** (global state) | tiny |
| Open fd + `F_GETPATH` via `fcntl(2)` | yes (fd was opened follow-symlinks) | yes | yes | small + `unsafe.Pointer` |
| Component-wise `os.ReadDir` walk | needs explicit step | yes | yes | medium |

The library targets POSIX (claude does not run on Windows in practice — there are no `windows` build tags in the repo). The two platforms that matter are darwin and linux. On linux, default ext4/xfs/btrfs are case-sensitive: a differently-cased lookup returns `ENOENT`, the fallback (encode as-passed) fires, and there is nothing to canonicalise — `EvalSymlinks` is sufficient. On darwin, the default case-insensitive APFS root needs an explicit case-canonical step, and `F_GETPATH` is the kernel-blessed way to obtain one in a single syscall (it is what `os.Getwd` itself uses on darwin).

Picked: split implementation by build tag.

- `pkg/tuidriver/cwd_darwin.go` (build tag `//go:build darwin`) — open the path, call `fcntl(fd, F_GETPATH, buf)` via stdlib `syscall.Syscall`.
- `pkg/tuidriver/cwd_other.go` (build tag `//go:build !darwin`) — `filepath.EvalSymlinks(p)`.

Both expose the same internal signature: `func canonicalisePath(p string) (string, bool)`. The shared `cwd.go` calls it before the byte-transform and falls back to the input on `ok == false`.

Why not component walk: it works but is ~30 LOC of pure Go with more edge cases (root handling, relative paths, ReadDir-permission errors mid-walk) than the `F_GETPATH` path. `F_GETPATH` is one syscall, ~15 LOC, and exactly the operation we want; the small `unsafe.Pointer` surface is contained to one platform file. No new module dependency (uses stdlib `syscall`, not `golang.org/x/sys/unix` — keeps `go.mod` clean; see § Open questions).

Why not `os.Chdir` + `os.Getwd`: explicitly ruled out by ticket — mutates process-global state and is unsafe in a library that may be used by long-lived concurrent consumers (the future pyrycode dispatcher is exactly that shape).

### `pkg/tuidriver/cwd.go` — modified body, same exported signature

Same package, same exported function name and signature (`EncodeCwd(string) string`). The diff is one new step in front of the byte-transform loop:

```go
func EncodeCwd(cwd string) string {
    if canonical, ok := canonicalisePath(cwd); ok {
        cwd = canonical
    }
    // existing byte-transform loop, unchanged
}
```

Update the doc comment to document the new contract:

- Resolves the input to its on-disk canonical form (symlinks resolved; case canonicalised on case-insensitive filesystems) before encoding.
- If the path does not exist on disk, encodes the input as-passed (no error path).
- Cross-reference the existing "empirically derived 2026-05-18" note — the byte-transform itself is unchanged.

Total change in `cwd.go`: ~10 lines of body + doc-comment update.

### `pkg/tuidriver/cwd_darwin.go` (new) — `F_GETPATH` canonicalisation

Build tag: `//go:build darwin`.

Single function `canonicalisePath(p string) (canonical string, ok bool)`. Contract sketch:

1. `os.Open(p)` — follows symlinks by default, so the fd points at the resolved inode. On any error (`ENOENT`, `EACCES`, ...), return `"", false`.
2. `defer f.Close()`.
3. Allocate `buf := make([]byte, syscall.MAXPATHLEN)` (1024 on darwin).
4. `_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETPATH, uintptr(unsafe.Pointer(&buf[0])))`. On `errno != 0`, return `"", false`.
5. Trim trailing NUL: `n := bytes.IndexByte(buf, 0); if n < 0 { n = len(buf) }`.
6. Return `string(buf[:n]), true`.

Approximate size: ~25 lines including imports. Tests do not exercise this file directly — they exercise `EncodeCwd`, which routes through it on darwin.

References: [`fcntl(2)` man page on darwin](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/fcntl.2.html) lists `F_GETPATH` with the buffer-of-MAXPATHLEN-bytes contract. `os.Getwd` on darwin uses the same call internally.

### `pkg/tuidriver/cwd_other.go` (new) — `EvalSymlinks` canonicalisation

Build tag: `//go:build !darwin`.

Single function `canonicalisePath(p string) (canonical string, ok bool)`. Contract sketch:

1. `resolved, err := filepath.EvalSymlinks(p)`.
2. On error, return `"", false`.
3. On success, return `resolved, true`.

Approximate size: ~12 lines including imports.

On Linux this gives full canonicalisation because the filesystem is case-sensitive (a differently-cased lookup just returns `ENOENT` and the fallback fires — vacuous, AC 4 is satisfied by the test skipping in that case). On other future platforms (Windows, BSD), the same code path works but case canonicalisation may be incomplete — that is the same gap `EvalSymlinks` has on macOS, accepted here as out of scope (see § Non-goals).

### Existing tests (`pkg/tuidriver/cwd_test.go`) — annotate, do not delete

The existing table covers 9 cases. Walk them with the new behaviour:

| Case | Input | On-disk? | Behaviour after change |
|---|---|---|---|
| empty | `""` | n/a | canonicalisePath fails → fallback → `""` ✅ |
| pure alnum | `"abc123XYZ"` | no | fallback → `"abc123XYZ"` ✅ |
| single slash | `"/"` | **yes (root)** | canonical is `"/"` on both platforms → `"-"` ✅ |
| path with slashes | `"/Users/me/code"` | **probably no, but FS-dependent** | fallback expected → `"-Users-me-code"` ⚠️ |
| adjacent specials | `") ["` | no | fallback → `"---"` ✅ |
| dot and space | `"v1.2 beta"` | no | fallback → `"v1-2-beta"` ✅ |
| underscore | `"snake_case"` | no | fallback → `"snake-case"` ✅ |
| unicode bytes | `"café"` | no | fallback → `"caf--"` ✅ |
| reference case | `"/private/tmp/encode test (with) [brackets] & amp+plus_under"` | no (literal test string) | fallback → byte transform ✅ |

The one FS-dependent case is `"/Users/me/code"`: on a developer's Mac it may or may not exist. Rewrite this case to a path that is reliably non-existent so the test stays deterministic. Suggested form: prefix with `/nonexistent-` or `/tui-driver-test-fixture-does-not-exist-`. The point of the case is "non-alnum bytes map to hyphens through path separators"; the prefix change preserves intent. The commit must call out this rewrite explicitly (per AC bullet 6).

The "single slash" case is fine as-is: `/` exists on every POSIX system, canonicalises to `/`, encodes to `-`. The assertion is preserved by canonicalisation rather than fallback, but the result is identical.

Add a short comment to the test table noting that the remaining cases pass through the fallback because the inputs do not name on-disk directories — readers should understand the table is now checking *both* the byte-transform and the fallback contract.

### New tests in `pkg/tuidriver/cwd_test.go`

Four new top-level test functions (separate from the table) per AC bullet 4. Each uses `t.TempDir()` for isolation. Bullet-pointed scenarios — the developer writes Go test bodies in the existing idiom:

- **`TestEncodeCwd_RealpathHappyPath`** — Create a subdir under `t.TempDir()` (any reasonable name, e.g. `"workdir"`). Call `EncodeCwd(realDir)`. Compute the expected output by calling the byte-transform on the canonicalised path (which on macOS may differ from `realDir` due to `/var → /private/var` symlink resolution, so do not compare against the literal `realDir` byte-transform — resolve it via the same canonicalisation path used by production, e.g. `filepath.EvalSymlinks` for the test's expected value). Assert equality.
- **`TestEncodeCwd_NonexistentPath`** — Construct a deliberately non-existent absolute path under `t.TempDir()` (e.g. `tmp + "/this-does-not-exist"`). Assert that `EncodeCwd(p)` equals the byte-transform of `p` as-passed — i.e. the same output today's implementation produces.
- **`TestEncodeCwd_SymlinkResolution`** — Create a real subdir `tmp/target`. Create a symlink `tmp/link` → `tmp/target` via `os.Symlink`. Assert `EncodeCwd(tmp/link) == EncodeCwd(tmp/target)`. This is portable: it works on both case-sensitive and case-insensitive filesystems because both EvalSymlinks (linux) and F_GETPATH-on-open-fd (darwin) follow symlinks.
- **`TestEncodeCwd_CaseCanonicalisation`** — Gated on a runtime case-insensitivity probe. The probe: create `tmp/CaseProbe`, `os.Stat(tmp/caseprobe)`; if the stat succeeds, the FS is case-insensitive. If the probe says case-sensitive, `t.Skip("case-sensitive filesystem — assertion would be vacuous")`. If case-insensitive: create `tmp/Foo`, call `EncodeCwd(tmp/foo)`, assert the output contains the canonical-case `"Foo"`-encoded substring (`-Foo`), not the lowercased `-foo`. Compare against the expected output by running the byte-transform over the canonicalised path (same approach as the happy-path test).

The probe must come first in the test body — if it returns case-sensitive, `t.Skip` before any other setup. On a Linux CI runner with ext4, the test skips cleanly and the AC is satisfied vacuously (no assertion runs but the test does not fail).

No new test helpers. `t.TempDir`, `os.MkdirAll`, `os.Symlink`, `os.Stat`, and `filepath.EvalSymlinks` are stdlib and cover the surface.

### `cmd/spike-long-prompt/README.md` — remove the operator-triage bullet

Delete the entire bullet at line 200-201:

> - **Encoded-cwd quirk worth noting for operators** — ...

The remaining § *Surprises / findings* bullets stay. The bullet's deletion is the only doc edit in that file.

Out of scope but worth flagging to the documentation phase: `docs/knowledge/codebase/47.md:21` carries the same finding as a "lesson learned" entry. That file is the empirical record of ticket #47 and the entry says "Not a defect — the casing-faithful behaviour matches claude's own filesystem layout." After this ticket merges, the entry becomes stale. The documentation phase for #57 should add a follow-up note to `docs/knowledge/codebase/47.md` (or to the per-ticket file it creates for #57) cross-referencing this ticket as the resolution. Architect does not edit `docs/knowledge/codebase/47.md` here (out of architect scope).

### Behaviour matrix (post-change)

| Input | On-disk shape | macOS output | Linux output |
|---|---|---|---|
| `/` | `/` (always) | `-` | `-` |
| `/tmp` (symlink → `/private/tmp` on darwin) | resolves to `/private/tmp` on darwin | `-private-tmp` | `-tmp` (no symlink) |
| `/Users/me/WorkSpace/foo` (on-disk `Workspace`) | case-insensitive APFS | `-Users-me-Workspace-foo` (canonical case) | n/a |
| `/nonexistent/path` | does not exist | `-nonexistent-path` (fallback) | `-nonexistent-path` (fallback) |

The third row is the bug this ticket fixes. The first, second, and fourth rows match today's behaviour (the second only because `/tmp` happens to be a symlink whose target encoding is the same on the test runner; in general the symlink-resolution column is a behavioural change too, but a wanted one).

## Concurrency model

`EncodeCwd` was a pure synchronous function and remains synchronous after the change. The added FS syscalls (`os.Open` + `fcntl`, or `filepath.EvalSymlinks`) are blocking but bounded and called once per session setup. No new goroutines, channels, or locks. No interaction with the library's existing PTY-reader / JSONL-tailer goroutines.

The function is safe to call from any goroutine. The OS file descriptor allocated in the darwin canonicaliser is closed via `defer` before the function returns; no FD leak surface.

## Error handling

The contract is total: `EncodeCwd(string) string` — no error return, even after the change. Every failure mode in the canonicalisation step (non-existent path, permission denied, FD exhaustion, fcntl failure) collapses into "fallback to encoding the input as-passed." This is required by AC bullet 2 — callers that pass logical paths for testing must keep getting a usable result.

No logging from inside `EncodeCwd`. The library has no logger; adding one for this one function is out of scope. Failures are silent and intentional (the fallback IS the failure handling).

The only observable difference from a caller's perspective is the output value. Callers that today get a path-points-nowhere result will now get a path that does point somewhere when the input exists on disk. Callers that today pass non-existent inputs get the same result.

## Testing strategy

Two layers:

1. **Unit tests in `pkg/tuidriver/cwd_test.go`.** The 9 existing cases (one rewritten for determinism) + 4 new cases described in § "New tests." Run with `go test ./pkg/tuidriver/...`. The case-canonicalisation test self-skips on case-sensitive filesystems; the other three new tests run everywhere.
2. **Integration validation via existing spikes (manual, no automated assertion in this ticket).** After merge, running any `--session-id`-pinning spike (`spike-one-turn`, `spike-multi-turn`, `spike-long-prompt`, ...) under a worktree entered with non-canonical casing on macOS should succeed — today's 10 s stat-poll timeout vanishes. This is the empirical confirmation that the bug is gone but is not encoded as a test (would require shelling out to a real `claude`, which is the e2e harness's territory, not the unit-test suite). The fix is shown to work by the unit tests; the spike-side confirmation is operator-visible smoke-testing.

CI implication: `go test ./...` runs on linux GitHub runners by default. The case-canonicalisation test will skip there. On a macOS developer machine (and any future macOS CI runner), it runs and asserts. AC bullet 4 explicitly carves out the runtime-probe gating, so this is the intended shape.

## Open questions

None blocking. Two judgement calls noted and resolved here:

- **Stdlib `syscall` vs `golang.org/x/sys/unix` for `F_GETPATH`.** Picked stdlib `syscall` to avoid adding a new module dependency (today's `go.mod` has zero `golang.org/x/*` deps). The cost is using `syscall.SYS_FCNTL` + `syscall.F_GETPATH` + `syscall.MAXPATHLEN` directly via `syscall.Syscall`, which requires `unsafe.Pointer` to pass the buffer. The `syscall` package is frozen but these constants are stable on darwin (they reflect kernel ABI), so freeze status is fine. If the developer hits an unforeseen API mismatch, switching to `golang.org/x/sys/unix` is a 1-line `go get` + import change in `cwd_darwin.go` only; revisit if needed.
- **Whether to also update `docs/knowledge/codebase/47.md` line 21.** Declined for the architect phase. The file is documentation-phase territory; the documentation agent for #57 will record the supersession. Architect notes the cross-reference in § "Files touched" and § Files to read first.

## Non-goals

- **Windows canonicalisation.** No Windows build tag in the repo; not exercised. The `cwd_other.go` build tag covers `!darwin` which includes Windows, but `EvalSymlinks` does not canonicalise case on Windows either. If the library ever targets Windows, a `cwd_windows.go` build-tag file using `GetFinalPathNameByHandleW` would be added — that is a separate ticket.
- **Component-wise walk as a fallback for non-existent paths.** Today's fallback returns the byte-transform of the as-passed input. A future enhancement might walk as many parent components as exist on disk to canonicalise the prefix, then fall back to the byte-transform for the trailing non-existent segments. Out of scope — adds complexity for no observed need (the existing failure mode is "process started under wrong casing," which always has an on-disk canonical form to resolve to).
- **Caching canonicalised results.** `EncodeCwd` is called once per session setup. No caching pressure. Adding a `sync.Map` would be premature.
- **Renaming the function.** The contract change ("encode the path claude writes to") matches the function's existing name and doc-comment intent; no rename needed.

## Files touched

| File | Change | Approx LOC |
|---|---|---|
| `pkg/tuidriver/cwd.go` | Add canonicalisation call in front of byte-transform; update doc comment | ~10 |
| `pkg/tuidriver/cwd_darwin.go` | NEW. `//go:build darwin`. `canonicalisePath` via `os.Open` + `fcntl(F_GETPATH)`. | ~25 |
| `pkg/tuidriver/cwd_other.go` | NEW. `//go:build !darwin`. `canonicalisePath` via `filepath.EvalSymlinks`. | ~12 |
| `pkg/tuidriver/cwd_test.go` | Annotate existing table (1 case rewritten for determinism); add 4 new `Test*` functions. | ~75 |
| `cmd/spike-long-prompt/README.md` | Delete the "Encoded-cwd quirk" bullet at lines 200-201. | -2 |

Production-source files (non-test, non-doc): 3 (`cwd.go` modified, `cwd_darwin.go` new, `cwd_other.go` new). Well under the 5-file split threshold. Sized **XS** as PO indicated. No `go.mod` / `go.sum` changes (stdlib `syscall` only).

No consumer-side edits. Every call site (`cmd/spike-*/main.go`, `cmd/probe-first-prompt-hang/main.go`) uses `tuidriver.EncodeCwd(cwd)` directly and continues to work without modification — the change is behaviour-compatible at the call site.
