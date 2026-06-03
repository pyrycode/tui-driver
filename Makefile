# tui-driver — top-level targets.
#
# The headline target is `make e2e`: builds the runner, every spike, and the
# probe to ./bin/, then runs the runner. Exits non-zero on any check
# failure; emits ./e2e-report.json either way (single CI artifact).

BIN_DIR    := ./bin
REPORT     := ./e2e-report.json
GO         ?= go

SPIKES     := spike-one-turn spike-multi-turn spike-cancel spike-permission spike-multiselect spike-ask-user spike-long-prompt spike-short-prompt
PROBES     := probe-first-prompt-hang
CHECKERS   := e2e-snapshot-check
RUNNER     := e2e-runner

ALL_BINS   := $(SPIKES) $(PROBES) $(CHECKERS) $(RUNNER)
BIN_PATHS  := $(addprefix $(BIN_DIR)/,$(ALL_BINS))

# Optional model / effort overrides. Unset → spike+probe inherit the
# operator's interactive Claude config (Max-subscription path). Set →
# the runner exports TUIDRIVER_CLAUDE_{MODEL,EFFORT}, EnsureClaudeEnv
# appends --model/--effort onto every spike+probe child. CI uses
# MODEL=haiku EFFORT=low to cap metered-API spend.
MODEL      ?=
EFFORT     ?=

.PHONY: e2e build-bin clean-bin clean-report rerecord-snapshots check vet test

# `make check` is the fast, claude-free gate run on every PR (see
# .github/workflows/check.yml). `make e2e` remains the live-claude harness and
# is operator/CI-driven separately. vet + race test compile every package —
# library, spikes, runner — so a breaking API change surfaces here.
check: vet test

vet:
	$(GO) vet ./...

test:
	$(GO) test -race ./...

e2e: build-bin
	$(if $(MODEL),TUIDRIVER_CLAUDE_MODEL=$(MODEL)) $(if $(EFFORT),TUIDRIVER_CLAUDE_EFFORT=$(EFFORT)) $(BIN_DIR)/$(RUNNER) -bin-dir $(BIN_DIR) -report $(REPORT)

# Re-record the two snapshot-drift JSON fixtures under pkg/tuidriver/testdata/.
# Operator-driven, NOT invoked by `make e2e`. Review with `git diff` before commit;
# bump claude-version.lock `version=` to match `claude --version` in the same commit.
rerecord-snapshots: build-bin
	$(BIN_DIR)/e2e-snapshot-check -record -bin-dir $(BIN_DIR)

build-bin: $(BIN_PATHS)

$(BIN_DIR)/%: FORCE
	@mkdir -p $(BIN_DIR)
	go build -o $@ ./cmd/$*

FORCE:

clean-bin:
	rm -rf $(BIN_DIR)

clean-report:
	rm -f $(REPORT)
