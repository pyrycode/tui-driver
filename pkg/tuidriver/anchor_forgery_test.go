package tuidriver

import (
	"strings"
	"testing"
)

// anchor_forgery_test.go is the #221 negative regression suite: every literal
// the detection layer matches, rendered as ordinary screen CONTENT rather than
// as claude's own chrome, checked against the detector that owns it.
//
// Why this file exists. On 2026-07-07 two production runs aborted because a
// detector could not tell claude's own dialog from a quotation of it. Ticket
// 173 (an agent displaying the network detector's own source) tripped the
// network banner; ticket 217 (an anchor-inventory ticket body) tripped the
// trust modal. The runner treats a mid-run trust or network detection as fatal,
// so each forgery killed the run. #219 (trust) and #220 (network + mcp banner)
// fixed those two anchors. This suite locks both fixes as regressions and, more
// importantly, makes the NEXT anchor safe by default: an anchor added without a
// structural co-signal fails here.
//
// The three content forms an anchor can be forged in, from the review:
//   - transcript echo — the phrase quoted inside a prose line.
//   - source read     — the phrase inside a quoted string with a line-number
//                       gutter, how an agent displays the detector's own code.
//   - status region   — fresh content passing through claude's bottom status
//                       window, where the region-scoped detectors look.
//
// Anchor sites enumerated here (keep in sync when adding or changing an anchor):
//   - modal.go       anchorMCPSpaced, anchorMCPEmptySpaced, the agents pair,
//                    anchorAskUserSpaced, anchorPermissionSpaced,
//                    anchorModelSelectSpaced, the permissions-config pair.
//   - permission.go  anchorTrustHeaderSpaced.
//   - network.go     networkFailureAnchors.
//   - mcp_banner.go  mcpFailureBannerRe.
//   - state.go       spinnerGlyphs, InterruptHint, IdleGlyph.
//
// Two protection tiers exist today, so the suite asserts different outcomes:
//
//  1. STRUCTURALLY PROTECTED — trust (the option-row shape, #219), the two
//     status banners, the permission overlay and the busy/idle axis (bottom-
//     region scoping, #220/#153). A transcript-body forgery of these MUST fire
//     nothing. These are the teeth; they pass now and encode 173 and 217.
//  2. NOT YET PROTECTED — the whole-grid panel classes (mcp, model-select,
//     permissions-config, ask-user, agents) still classify from one on-screen
//     content line, because their structural co-signal is #223's job and #223
//     is blocked by this ticket. TestWholeGridAnchorsStillForgePending223 pins
//     that gap as current behaviour, so when #223 lands a co-signal the
//     assertion flips and forces the class up into tier 1.

// forgedForm is one realistic rendering of a forged anchor: a label and the
// snapshot bytes that carry the anchor as screen content.
type forgedForm struct {
	name string
	snap []byte
}

// forgedBodyForms renders anchor as transcript-body content in the two body
// forms (transcript echo, source read), each pushed ABOVE the bottom status
// region by filler rows below it. aboveRegionFiller (20) exceeds the largest
// region window (permissionRegionRows = 12) so the anchor row never lands in any
// region scope. \r\n rows via gridRows so vt10x renders flat rows.
func forgedBodyForms(anchor string) []forgedForm {
	filler := make([]string, 20)
	for i := range filler {
		filler[i] = "transcript body line"
	}
	echo := append([]string{
		"user: what does the dialog say?",
		"assistant: it reads \"" + anchor + "\" and then waits for input.",
	}, filler...)
	source := append([]string{
		"52\t// the detector matches the literal below",
		"54\tvar theAnchor = []byte(\"" + anchor + "\")",
	}, filler...)
	return []forgedForm{
		{"transcript-echo", gridRows(echo...)},
		{"source-read", gridRows(source...)},
	}
}

// TestModalPhraseAnchorsRejectBodyForgery is the tier-1 teeth for the modal
// classes that DO have a structural guard: trust (option-row shape) and the
// permission overlay (bottom-region scope). The header/prompt quoted as body
// content must not classify as its modal. Encodes ticket 217.
func TestModalPhraseAnchorsRejectBodyForgery(t *testing.T) {
	cases := []struct {
		name   string
		anchor string
		fires  func([]byte) bool
	}{
		{
			"trust-folder header (#217)",
			string(anchorTrustHeaderSpaced),
			func(b []byte) bool { return HasTrustModal(b) || DetectModalClass(b) == ModalClassTrustFolder },
		},
		{
			"permission prompt",
			string(anchorPermissionSpaced),
			func(b []byte) bool { return DetectModalClass(b) == ModalClassPermission },
		},
	}
	for _, tc := range cases {
		for _, f := range forgedBodyForms(tc.anchor) {
			t.Run(tc.name+"/"+f.name, func(t *testing.T) {
				if !strings.Contains(string(f.snap), tc.anchor) {
					t.Fatalf("fixture lost anchor %q — forgery contrast void", tc.anchor)
				}
				if tc.fires(f.snap) {
					t.Errorf("%s quoted as %s content fired its detector, want no fire", tc.name, f.name)
				}
			})
		}
	}
}

// TestBannerAnchorsRejectBodyForgery is the tier-1 teeth for the two status-area
// banners: network-unreachable (#173) and mcp-failure. Both are bottom-region
// scoped (#220), so the phrase quoted in the transcript body must not fire.
func TestBannerAnchorsRejectBodyForgery(t *testing.T) {
	cases := []struct {
		name   string
		anchor string
		fires  func([]byte) bool
	}{
		{"network phrase (#173)", networkFailureAnchors[0], HasNetworkFailure},
		{
			"mcp-failure banner",
			"2 MCP servers failed · /mcp",
			func(b []byte) bool { return HasMcpFailureBanner(b) || FailedMcpCount(b) != 0 },
		},
	}
	for _, tc := range cases {
		for _, f := range forgedBodyForms(tc.anchor) {
			t.Run(tc.name+"/"+f.name, func(t *testing.T) {
				if !strings.Contains(string(f.snap), tc.anchor) {
					t.Fatalf("fixture lost anchor %q — forgery contrast void", tc.anchor)
				}
				if tc.fires(f.snap) {
					t.Errorf("%s quoted as %s content fired its detector, want no fire", tc.name, f.name)
				}
			})
		}
	}
}

// TestBusyIdleAnchorsRejectRegionForgery is the tier-1 teeth for the idle/busy
// axis. Every glyph in claude's spinner animation cycle and the interrupt hint,
// quoted as transcript body content above the status region, must not flip the
// busy predicate (region scoping, #153). A stale ❯ scrolled into the body must
// likewise not forge idle.
func TestBusyIdleAnchorsRejectRegionForgery(t *testing.T) {
	busy := make([]string, 0, len(spinnerGlyphs)+1)
	for _, g := range spinnerGlyphs {
		busy = append(busy, string(g))
	}
	busy = append(busy, InterruptHint)
	for _, anchor := range busy {
		for _, f := range forgedBodyForms(anchor) {
			t.Run("busy "+anchor+"/"+f.name, func(t *testing.T) {
				if IsThinking(f.snap) {
					t.Errorf("busy anchor %q as %s content forged thinking, want false", anchor, f.name)
				}
			})
		}
	}

	// A ❯ high in the body, with only transcript below it in the status region,
	// must not read as idle: idle requires ❯ IN the region.
	filler := make([]string, 21)
	filler[0] = string(IdleGlyph) + " stale scrollback prompt"
	for i := 1; i < len(filler); i++ {
		filler[i] = "transcript body line"
	}
	if IsIdle(gridRows(filler...)) {
		t.Error("stale ❯ in scrollback forged idle, want false")
	}
}

// TestTrustAnchorRejectsStatusRegionQuotation covers trust's third content form.
// Trust is shape-gated (#219), so even the header quoted in the bottom status
// region — where the region-scoped detectors look — still needs a pointer-marked
// option row to classify. A bare in-region quotation does not.
func TestTrustAnchorRejectsStatusRegionQuotation(t *testing.T) {
	filler := make([]string, 20)
	for i := range filler {
		filler[i] = "transcript body line"
	}
	// Header on the very last row, no option row anywhere.
	snap := gridRows(append(filler, "log: prompt was \""+string(anchorTrustHeaderSpaced)+"\"")...)
	if HasTrustModal(snap) || DetectModalClass(snap) == ModalClassTrustFolder {
		t.Error("in-region trust-header quotation classified as trust, want no fire")
	}
}

// TestWholeGridAnchorsStillForgePending223 pins the tier-2 KNOWN, TRACKED gap.
// The whole-grid panel classes still classify from a single on-screen content
// line, because their structural co-signal is #223's job and #223 is blocked by
// this ticket. This test asserts that forgery as CURRENT behaviour so the gap is
// visible and cannot change silently.
//
// Handoff protocol: when #223 lands a co-signal for one of these classes, the
// matching assertion here flips (DetectModalClass returns Unknown). Move that
// class into the fire-nothing suite above and delete its row here. This is how
// #223 consumes the suite.
func TestWholeGridAnchorsStillForgePending223(t *testing.T) {
	cases := []struct {
		name string
		line string
		want ModalClass
	}{
		{"mcp title", "the /mcp screen shows Manage MCP servers at the top", ModalClassMCP},
		{"mcp empty-state", "it printed No MCP servers configured after /doctor", ModalClassMCP},
		{"ask-user footer", "the picker footer reads Enter to select at the bottom", ModalClassAskUserQuestion},
		{"model-select title", "run /model to open the Select model picker", ModalClassModelSelect},
		{"agents header+tab", "the Agents modal lists a Running and a Library tab", ModalClassAgents},
		{"permissions-config", "the Permissions screen has Allow, Ask and Deny tabs", ModalClassPermissionsConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := gridRows(tc.line) // one on-screen prose line, no dialog chrome
			got := DetectModalClass(snap)
			if got != tc.want {
				t.Errorf("%s: DetectModalClass = %q, want %q (the known pre-#223 forgery). "+
					"If #223 added this class's structural co-signal, this now reads Unknown — "+
					"move the class into the fire-nothing suite and delete this row.", tc.name, got, tc.want)
			}
		})
	}
}

// TestNegativeSuitePositiveControls is the co-signal safety check: a guard a fix
// like #223 adds must not break real detection. Every committed real-screen
// fixture must still fire its own detector.
func TestNegativeSuitePositiveControls(t *testing.T) {
	if got := DetectModalClass(loadFixture(t, "mcp-snapshot.bin")); got != ModalClassMCP {
		t.Errorf("mcp fixture: DetectModalClass = %q, want MCP", got)
	}
	trust := loadFixture(t, "trust-folder-snapshot.bin")
	if got := DetectModalClass(trust); got != ModalClassTrustFolder {
		t.Errorf("trust fixture: DetectModalClass = %q, want TrustFolder", got)
	}
	if !HasTrustModal(trust) {
		t.Error("trust fixture: HasTrustModal = false, want true")
	}
	if got := DetectModalClass(loadFixture(t, "permission-snapshot.bin")); got != ModalClassPermission {
		t.Errorf("permission fixture: DetectModalClass = %q, want Permission", got)
	}
	if !HasNetworkFailure(loadFixture(t, "network-failure-snapshot.bin")) {
		t.Error("network fixture: HasNetworkFailure = false, want true")
	}
}
