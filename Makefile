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
TOOLS      := corpus-replay repro-permission-flake

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

.PHONY: e2e build-bin clean-bin clean-report check vet test corpus-replay corpus-assert repro-permission-flake

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

# The operator-run detection GATE (#259): replay the corpus at full fidelity
# (stride 1 forced) and exit non-zero if any detection-health invariant is
# violated — a stray modal/banner fire, a missing idle, or a detector flapping
# past the edge ceiling in a production ok-tagged run. Hand-run, expected tens of
# minutes on the full corpus, NOT a CI gate and NOT a make check step (org rule).
# See cmd/corpus-replay/README.md. Override CORPUS_DIR / EDGE_CEILING as needed.
corpus-assert: $(BIN_DIR)/corpus-replay
	$(BIN_DIR)/corpus-replay -dir $(CORPUS_DIR) -assert $(if $(EDGE_CEILING),-assert-edge-ceiling $(EDGE_CEILING))

# Reproduce + diagnose spike-permission's "modal not detected within 30s" load
# flake (#253 slice A). Hand-run diagnostic, live-claude, minutes-long — builds
# the spike, then drives it in concurrent waves under artificial load and writes
# a durable diagnosis artifact. NOT a make-e2e check (would spawn PxW live
# claude). See cmd/repro-permission-flake/README.md. Override CONCURRENCY /
# WAVES / CPU_BURN as needed.
repro-permission-flake: $(BIN_DIR)/repro-permission-flake $(BIN_DIR)/spike-permission
	$(BIN_DIR)/repro-permission-flake -bin $(BIN_DIR)/spike-permission \
		$(if $(CONCURRENCY),-concurrency $(CONCURRENCY)) \
		$(if $(WAVES),-waves $(WAVES)) \
		$(if $(CPU_BURN),-cpu-burn $(CPU_BURN))

build-bin: $(BIN_PATHS)

$(BIN_DIR)/%: FORCE
	@mkdir -p $(BIN_DIR)
	go build -o $@ ./cmd/$*

FORCE:

clean-bin:
	rm -rf $(BIN_DIR)

clean-report:
	rm -f $(REPORT)
