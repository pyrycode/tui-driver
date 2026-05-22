# 79 — Makefile: per-binary build rules have no source dependencies

## Files to read first

- `Makefile` (whole file, 41 lines) — the single file being changed. Specifically:
  - `Makefile:10-16` — `SPIKES`, `PROBES`, `CHECKERS`, `RUNNER`, `ALL_BINS`, `BIN_PATHS` — variable composition that the fix re-uses.
  - `Makefile:26` — the existing `.PHONY` line, the one and only line being edited.
  - `Makefile:33-35` — the pattern rule `$(BIN_DIR)/%:` whose lack of prerequisites is the root cause.
- Issue body (#79) — already paraphrased below in **Context**; no need to fetch.

No other files in the repo need to be read for this change. There is no Go code, no test, no documentation update.

## Context

`make e2e` does not rebuild a binary when its source files change. The top-level pattern rule:

```make
$(BIN_DIR)/%:
	@mkdir -p $(BIN_DIR)
	go build -o $@ ./cmd/$*
```

has no prerequisites. `make` sees `bin/<name>` already exists and short-circuits before calling `go build` — even though `go build` itself would have rebuilt correctly if invoked, because its build cache tracks Go sources transitively. The net result: developers silently run yesterday's binary after editing today's source. Discovered during #69, where bumping a timeout in `cmd/e2e-runner/main.go` had no effect on `make e2e` until `make clean-bin` was run.

## Design

**The fix is a single-token change to `Makefile:26`:**

```make
# Before
.PHONY: e2e build-bin clean-bin clean-report

# After
.PHONY: e2e build-bin clean-bin clean-report $(BIN_PATHS)
```

That is the entire production change. No new rules, no per-binary boilerplate, no `find`-based source enumeration.

### Why this shape

`.PHONY: $(BIN_PATHS)` tells `make` to always execute the pattern rule's recipe for every `bin/<name>` target, regardless of whether the file already exists. The recipe then invokes `go build`, which performs its own incremental rebuild via the Go build cache. The cache understands the full transitive Go dependency graph (anything `cmd/<name>` imports from `pkg/tuidriver/`, indirect modules, the standard library) — which is what AC 2 requires.

A `go build` no-op on a cache hit takes ~100ms and is silent. With 9 binaries (`SPIKES` + `PROBES` + `CHECKERS` + `RUNNER`), the steady-state overhead per `make e2e` invocation is roughly 1 second of cache lookups — negligible against e2e runtime (minutes).

### Alternatives considered

- **`find cmd/<name> -name '*.go'` as prerequisites.** Rejected: misses transitive `pkg/` deps, which AC 2 explicitly requires. Reproducing `go list -deps` inside `make` reimplements what `go build` already does, badly.
- **`FORCE` target pattern** (`$(BIN_DIR)/%: FORCE` with an empty `FORCE` rule). Functionally equivalent to `.PHONY`. Less idiomatic in Go projects; no benefit.
- **Add an explicit `$(BIN_DIR)/%: $(shell go list -f ...)` prerequisite.** Couples `make` to `go list` output and runs `go list` on every parse of the Makefile. Strictly worse than letting `go build` handle it.

### Side-effects to confirm

- `make build-bin` will now always invoke `go build` for every binary (≈1s total on cache hits). Acceptable; no longer a no-op when nothing changed, but the previous no-op was the bug.
- No other rule in the Makefile consumes binary mtimes as a dependency, so making the binary paths PHONY does not cascade. Grep result: `bin/<name>` appears as a target prereq only via `build-bin: $(BIN_PATHS)` and is invoked at runtime as `$(BIN_DIR)/$(RUNNER)` (string expansion, not a make prereq).

## Concurrency model

N/A — Makefile change, no runtime concurrency.

## Error handling

N/A — Makefile change. `go build` failure semantics are unchanged.

## Testing strategy

Manual verification (no automated test for this — adding one would mean adding a Make-test harness, out of scope and disproportionate). The developer should run, in order:

1. **Clean happy path.** `make clean-bin && make e2e` builds all 9 binaries and runs the e2e suite. AC 3.
2. **Editing `cmd/` triggers rebuild.** Pick a small, low-impact spike (e.g. `cmd/spike-one-turn/main.go`). Add a benign comment line. Run `make e2e`. Confirm the binary's `mtime` (`stat -f %m bin/spike-one-turn` on macOS, `stat -c %Y` on Linux) advances. AC 1.
3. **Editing `pkg/` triggers rebuild.** Add a benign comment line to a file under `pkg/tuidriver/` that the spike imports. Run `make e2e`. Confirm every dependent binary's `mtime` advances. AC 2.
4. **No-op invocation still cheap.** Run `make e2e` twice in a row with no edits. The second invocation completes the build-bin phase in roughly a second or less (cache hits across all binaries).
5. **Pattern rule stays one rule.** `grep -c '^$(BIN_DIR)/' Makefile` should still report exactly `1`. AC 4.

Revert the benign comment edits before opening the PR.

## Open questions

None. The technical notes in the issue already point at this shape; the design simply confirms and commits to it.

## Out of scope (restating from ticket)

- No Makefile restructuring beyond this one line.
- No `make test` / `make vet` / `make lint` targets.
- No per-binary build flags or cross-compilation.
