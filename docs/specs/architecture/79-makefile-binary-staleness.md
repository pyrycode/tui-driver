# 79 — Makefile: per-binary build rules have no source dependencies

## Files to read first

- `Makefile` (whole file, 41 lines) — the single file being changed. Specifically:
  - `Makefile:10-16` — `SPIKES`, `PROBES`, `CHECKERS`, `RUNNER`, `ALL_BINS`, `BIN_PATHS` — variable composition; not modified, but useful to confirm `BIN_PATHS` is the right set.
  - `Makefile:26` — the existing `.PHONY` line. **Unchanged** in this rework — the previous spec's `$(BIN_PATHS)` addition here was the failure mode (see *Alternatives considered*).
  - `Makefile:33-35` — the pattern rule `$(BIN_DIR)/%:` whose lack of prerequisites is the root cause; this is where the fix goes.
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

### Why this is a rework (architect rework #4)

The previous spec prescribed adding `$(BIN_PATHS)` to the `.PHONY` line. Empirical testing in the developer phase (and re-verified during this rework against GNU Make 3.81 on macOS) showed this **does not work**: declaring the binary paths phony causes `make` to skip implicit/pattern-rule search for them, so the `$(BIN_DIR)/%:` recipe never fires and `make build-bin` reports *"Nothing to be done"* from a clean tree. From the GNU Make manual, *§ 4.6 Phony Targets*:

> Since it knows that phony targets do not name actual files that could be remade from other files, make can skip the implicit rule search for phony targets.

The previous spec's *Alternatives considered* section also claimed the `FORCE` prereq pattern was "functionally equivalent to `.PHONY`" — that was the falsified claim. `FORCE` is a real, recipe-less, always-out-of-date *prerequisite*; the pattern rule still applies because the target itself is not phony. `.PHONY` is a *target-level designation* that suppresses pattern-rule matching for the target. Different mechanism, different outcome.

This rework switches to the `FORCE` prereq pattern, which is the canonical Go-Makefile idiom for this exact problem.

## Design

**The fix is two changes inside the pattern rule region of `Makefile`, lines 33–35.** Leave `.PHONY` at `Makefile:26` exactly as it is today.

```make
# Before (Makefile:33-35)
$(BIN_DIR)/%:
	@mkdir -p $(BIN_DIR)
	go build -o $@ ./cmd/$*

# After (Makefile:33-36, with a new FORCE rule appended)
$(BIN_DIR)/%: FORCE
	@mkdir -p $(BIN_DIR)
	go build -o $@ ./cmd/$*

FORCE:
```

Concretely, two edits:

1. Append ` FORCE` to the existing target line `$(BIN_DIR)/%:` (after the colon, space-separated).
2. Add a `FORCE:` rule (target `FORCE`, no prerequisites, no recipe) somewhere in the file. Conventional placement: immediately after the pattern rule (between the recipe block and `clean-bin:`).

That is the entire production change — no `.PHONY` edit, no per-binary boilerplate, no `find`-based source enumeration. Net diff is ~3 lines added.

### Why this shape

`FORCE` is a target with no recipe and no prerequisites. `make` considers any target with no prerequisites and no recipe to be **always out-of-date** — so any rule that lists `FORCE` as a prereq has its recipe re-run on every invocation. Applied to `$(BIN_DIR)/%: FORCE`: the pattern rule still matches `$(BIN_DIR)/<name>` targets via normal implicit-rule search (the target itself is not phony), but the recipe always fires because `FORCE` is "newer" than any existing `bin/<name>`. The recipe then invokes `go build`, which performs its own incremental rebuild via the Go build cache. The cache understands the full transitive Go dependency graph (anything `cmd/<name>` imports from `pkg/tuidriver/`, indirect modules, the standard library) — which is what AC 2 requires.

A `go build` no-op on a cache hit takes ~100ms and is silent. With 10 binaries (`SPIKES` + `PROBES` + `CHECKERS` + `RUNNER`: 7 + 1 + 1 + 1), the steady-state overhead per `make e2e` invocation is roughly 1 second of cache lookups — negligible against e2e runtime (minutes).

Note: do **not** add `.PHONY: FORCE`. `FORCE` works precisely because it's a normal recipe-less target — phonying it would also work in practice, but it's idiomatically left as a plain target across Go and Linux-kernel Makefiles. Match prior art; don't invent a variant.

### Alternatives considered

- **`.PHONY: $(BIN_PATHS)` (the previous spec's prescription).** Falsified. GNU Make skips implicit/pattern-rule search for phony targets (manual § 4.6, § 10.5). Result on this Makefile: `make build-bin` from a clean tree reports *"Nothing to be done"* and produces zero binaries. Listed here so the trap is on the record — see also the *Lessons* section below.
- **Static pattern rule** (`$(BIN_PATHS): $(BIN_DIR)/%: ...` plus `.PHONY: $(BIN_PATHS)`). Works, because static pattern rules are *explicit* rules and `.PHONY` only suppresses *implicit* rule matching. Rejected over `FORCE` for two reasons: (a) static pattern rules are a less-recognized Make construct than `FORCE`, raising the cognitive cost on future readers; (b) it duplicates `$(BIN_PATHS)` (both in the target list and in `.PHONY`), which is the kind of synchronization debt that drifts as binaries are added/removed. `FORCE` requires zero per-binary maintenance.
- **`find cmd/<name> -name '*.go'` as prerequisites.** Rejected: misses transitive `pkg/` deps, which AC 2 explicitly requires. Reproducing `go list -deps` inside `make` reimplements what `go build` already does, badly.
- **`$(BIN_DIR)/%: $(shell go list -f ...)` prerequisite.** Couples `make` to `go list` output and runs `go list` on every parse of the Makefile. Strictly worse than letting `go build` handle it.

### Side-effects to confirm

- `make build-bin` will now always invoke `go build` for every binary (≈1s total on cache hits). Acceptable; no longer a no-op when nothing changed, but the previous no-op was the bug.
- The new `FORCE` target is recipe-less and prerequisite-less. It has no side effects on its own; it only exists to be listed as a prerequisite.
- No other rule in the Makefile becomes dependent on `FORCE` or on `bin/<name>` mtimes. Confirmed by reading the whole 41-line file: `bin/<name>` appears as a target prereq only via `build-bin: $(BIN_PATHS)`, and is invoked at runtime as `$(BIN_DIR)/$(RUNNER)` (shell expansion of a variable, not a make-level prereq).

## Concurrency model

N/A — Makefile change, no runtime concurrency.

## Error handling

N/A — Makefile change. `go build` failure semantics are unchanged.

## Testing strategy

Manual verification (no automated test for this — adding one would mean adding a Make-test harness, out of scope and disproportionate). The developer should run, in order:

1. **Clean happy path.** `make clean-bin && make e2e` builds all 10 binaries and runs the e2e suite. AC 3.
2. **Editing `cmd/` triggers rebuild.** Pick a small, low-impact spike (e.g. `cmd/spike-one-turn/main.go`). Add a benign comment line. Run `make build-bin` (faster than `make e2e` for this check). Confirm the binary's `mtime` (`stat -f %m bin/spike-one-turn` on macOS, `stat -c %Y` on Linux) advances. AC 1.
3. **Editing `pkg/` triggers rebuild.** Add a benign comment line to a file under `pkg/tuidriver/` that the spikes import. Run `make build-bin`. Confirm every dependent binary's `mtime` advances. AC 2.
4. **No-op invocation still cheap.** Run `make build-bin` twice in a row with no edits. The second invocation completes in roughly a second or less (cache hits across all binaries). The recipe now always runs — what's being checked is that the Go build cache short-circuits inside `go build`, not at the make level.
5. **Pattern rule stays one rule.** `grep -c '^\$(BIN_DIR)/' Makefile` should still report exactly `1` (the new `FORCE:` line does not match this anchored pattern). AC 4.
6. **Pre-flight smoke** (recommended *before* the full e2e run): apply the diff, `make clean-bin && make build-bin`, confirm `ls bin/` shows all 10 binaries. This catches a repeat of the previous-rework failure mode (where the build silently produced zero binaries) in seconds rather than after a multi-minute e2e run.

Revert the benign comment edits before opening the PR.

## Lessons (for documentation phase to pick up)

The PHONY-on-pattern-target trap is worth recording in `docs/lessons.md` or a knowledge note during the documentation phase. Short form:

> **`.PHONY` does not force pattern-rule recipes to run.** GNU Make skips implicit and pattern-rule search for phony targets (manual § 4.6, § 10.5). To force a pattern rule to always re-run, list a recipe-less always-out-of-date target (`FORCE`) as a prerequisite of the pattern, not as the target's phony designation. `FORCE` ≠ `.PHONY`.

Architect cannot edit `docs/lessons.md` per pipeline rules; flagging here so the documentation phase has the material.

## Open questions

None. The mechanism is empirically verified (FORCE pattern produces all 10 binaries on a clean tree; PHONY pattern produces zero). The previous open question — "does `.PHONY: $(BIN_PATHS)` work?" — is now closed: no, it does not, and the corrected design avoids it.

## Out of scope (restating from ticket)

- No Makefile restructuring beyond this one fix.
- No `make test` / `make vet` / `make lint` targets.
- No per-binary build flags or cross-compilation.
