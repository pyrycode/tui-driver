package tuidriver

import "regexp"

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

// HasMcpFailureBanner reports whether snap contains claude's "N MCP server
// failed" status banner. Useful when a consumer opts to allow MCP servers
// (i.e. NOT passing --strict-mcp-config) and wants to surface failures to
// the user — e.g. a mobile host UI showing "1 of 3 MCP servers offline."
//
// The banner means "claude attempted to connect to N servers and failed";
// it does NOT mean "still trying." There is no benefit to polling for it
// to clear.
func HasMcpFailureBanner(snap []byte) bool {
	return mcpFailureBannerRe.Match(StripANSI(snap))
}

// FailedMcpCount returns the N from the "N MCP server failed" banner, or 0
// if no banner is present. Use HasMcpFailureBanner if you only need a
// boolean — FailedMcpCount does the full regex match either way.
func FailedMcpCount(snap []byte) int {
	m := mcpFailureBannerRe.FindSubmatch(StripANSI(snap))
	if m == nil {
		return 0
	}
	n := 0
	for _, b := range m[1] {
		if b < '0' || b > '9' {
			return 0
		}
		n = n*10 + int(b-'0')
	}
	return n
}
