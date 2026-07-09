# tui-driver — top-level targets.
#
# The headline target is `make e2e`: builds the runner, every spike, and the
# probe to ./bin/, then runs the runner. Exits non-zero on any check
# failure; emits ./e2e-report.json either way (single CI artifact).

BIN_DIR    := ./bin
REPORT     := ./e2e-report.json
GO         ?= go

SPIKES     := spike-one-turn spike-multi-turn spike-cancel spike-permission spike-multiselect spike-ask-user spike-long-prompt spike-short-prompt spike-queued-modals
PROBES     := probe-first-prompt-hang probe-cwd-encoding
RUNNER     := e2e-runner
TOOLS      := corpus-replay

ALL_BINS   := $(SPIKES) $(PROBES) $(RUNNER) $(TOOLS)
BIN_PATHS  := $(addprefix $(BIN_DIR)/,$(ALL_BINS))

# Directory of .cast recordings for `make corpus-replay`, defaulting to the
# pyry agent-run flight recorder's location; override for another corpus.
# STRIDE, if set, samples every Nth output event.
CORPUS_DIR ?= $(HOME)/.local/share/pyry-recordings

# Optional model / effort overrides. Unset → spike+probe inherit the
# operator's interactive Claude config (Max-subscription path). Set →
# the runner exports TUIDRIVER_CLAUDE_{MODEL,EFFORT}, EnsureClaudeEnv
# appends --model/--effort onto every spike+probe child. CI uses
# MODEL=haiku EFFORT=low to cap metered-API spend.
MODEL      ?=
EFFORT     ?=

.PHONY: e2e build-bin clean-bin clean-report check vet test corpus-replay

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

# Replay the recording corpus through the detectors and print the fire report.
# Offline audit tool, claude-free, NOT a CI gate (org rule). See
# cmd/corpus-replay/README.md. Override CORPUS_DIR / STRIDE as needed.
corpus-replay: $(BIN_DIR)/corpus-replay
	$(BIN_DIR)/corpus-replay -dir $(CORPUS_DIR) $(if $(STRIDE),-stride $(STRIDE))

build-bin: $(BIN_PATHS)

$(BIN_DIR)/%: FORCE
	@mkdir -p $(BIN_DIR)
	go build -o $@ ./cmd/$*

FORCE:

clean-bin:
	rm -rf $(BIN_DIR)

clean-report:
	rm -f $(REPORT)
