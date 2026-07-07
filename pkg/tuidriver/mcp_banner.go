package tuidriver

import (
	"regexp"
	"strconv"
)

// bannerRegionRows bounds how far up from the bottom of the rendered screen a
// status-area banner (network-failure, mcp-failure) may sit and still count.
// Claude draws these just above the input box; the captured network fixture
// (network-failure-snapshot.bin) renders its line 6 rows from the bottom, and a
// trailing chrome line (e.g. "● high · /effort") can push it a row higher, so 8
// covers it with slack. Slightly larger than the idle/busy statusRegionRows
// (state.go) for that reason. Scoping the match to this window is what rejects
// the same phrase forged higher up in the on-screen transcript body — the
// #173 / #220 fix. Shared by network.go and mcp_banner.go.
const bannerRegionRows = 8

// mcpFailureBannerRe matches claude's status-bar message rendered when one
// or more configured MCP servers fail to connect. Observed forms:
//
//	"1 MCP server failed · /mcp"
//	"2 MCP servers failed · /mcp"
//
// The "· /mcp" suffix is a clickable hint pointing the user at the /mcp
// modal for details. Capture group 1 is the integer count.
//
// The banner appears in the lower-status area, not as a full modal — so it
// doesn't trigger DetectModalClass. State-detection predicates can include
// HasMcpFailureBanner alongside isIdle.
//
// Recommended sidestep: pass --strict-mcp-config to claude to skip all
// configured MCP servers entirely. This eliminates the banner without any
// runtime detection. See package-level comment in tuidriver.go.
var mcpFailureBannerRe = regexp.MustCompile(`(\d+)\s*MCP\s*servers?\s*failed`)

// mcpBannerMatchInRegion runs mcpFailureBannerRe over the bottom status region
// of the rendered grid and returns the first matching row's submatches, or nil.
// Region-scoping (#220) is what stops the banner phrase quoted in the transcript
// body from matching — it fires only where claude actually draws the banner.
func mcpBannerMatchInRegion(g *Grid) []string {
	for _, row := range g.LastRows(bannerRegionRows) {
		if m := mcpFailureBannerRe.FindStringSubmatch(row); m != nil {
			return m
		}
	}
	return nil
}

// HasMcpFailureBanner reports whether snap shows claude's "N MCP server failed"
// status banner in the bottom status region of the rendered screen. Useful when
// a consumer opts to allow MCP servers (i.e. NOT passing --strict-mcp-config)
// and wants to surface failures to the user — e.g. a mobile host UI showing
// "1 of 3 MCP servers offline."
//
// ADVISORY signal. The banner means "claude attempted to connect to N servers
// and failed"; it does NOT mean "still trying." There is no benefit to polling
// for it to clear. Since #220 it is matched on the rendered grid and scoped to
// the status region, so a quotation of the phrase in the transcript body no
// longer fires it.
func HasMcpFailureBanner(snap []byte) bool {
	return mcpBannerMatchInRegion(NewGrid(snap, 0, 0)) != nil
}

// FailedMcpCount returns the N from the "N MCP server failed" banner, or 0
// if no banner is present in the status region. Use HasMcpFailureBanner if you
// only need a boolean — FailedMcpCount does the full region-scoped match either
// way.
func FailedMcpCount(snap []byte) int {
	return mcpCountFromGrid(NewGrid(snap, 0, 0))
}

// mcpCountFromGrid is FailedMcpCount over a Grid the caller already rendered.
// WaitReady's one-shot classification renders the post-idle snapshot once and
// shares that grid across the trust/mcp/network/unknown checks (#225);
// FailedMcpCount stays the thin single-snapshot wrapper.
func mcpCountFromGrid(g *Grid) int {
	m := mcpBannerMatchInRegion(g)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}
