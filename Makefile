# tui-driver — top-level targets.
#
# The headline target is `make e2e`: builds the runner, every spike, and the
# probe to ./bin/, then runs the runner. Exits non-zero on any check
# failure; emits ./e2e-report.json either way (single CI artifact).

BIN_DIR    := ./bin
REPORT     := ./e2e-report.json

SPIKES     := spike-one-turn spike-multi-turn spike-cancel spike-permission spike-multiselect spike-ask-user
PROBES     := probe-first-prompt-hang
RUNNER     := e2e-runner

ALL_BINS   := $(SPIKES) $(PROBES) $(RUNNER)
BIN_PATHS  := $(addprefix $(BIN_DIR)/,$(ALL_BINS))

.PHONY: e2e build-bin clean-bin clean-report

e2e: build-bin
	$(BIN_DIR)/$(RUNNER) -bin-dir $(BIN_DIR) -report $(REPORT)

build-bin: $(BIN_PATHS)

$(BIN_DIR)/%:
	@mkdir -p $(BIN_DIR)
	go build -o $@ ./cmd/$*

clean-bin:
	rm -rf $(BIN_DIR)

clean-report:
	rm -f $(REPORT)
