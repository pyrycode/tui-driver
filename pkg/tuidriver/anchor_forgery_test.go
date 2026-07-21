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
//   - modal.go       anchorMCPSpaced, anchorAskUserSpaced,
//                    anchorPermissionSpaced, anchorModelSelectSpaced, the
//                    permissions-config pair. (The agents anchors were deleted
//                    when the class's classifier arm was retired in #245.)
//   - permission.go  anchorTrustHeaderSpaced.
//   - network.go     networkFailureAnchors.
//   - mcp_banner.go  mcpFailureBannerRe.
//   - apiretry.go    apiRetryRowRe (the "API error" + "Retrying in" two-token anchor).
//   - midresponse.go midResponseErrorAnchors (the "Connection closed
//                    mid-response" partial-output token).
//   - compacting.go  compactingPhraseRe + compactingBarRe (the "Compacting
//                    conversation" phrase paired with the ▱▰ progress-bar run).
//   - state.go       spinnerGlyphs, InterruptHint, IdleGlyph.
//
// Two protection tiers exist today, so the suite asserts different outcomes:
//
//  1. STRUCTURALLY PROTECTED — trust (the option-row shape, #219), the
//     permission overlay (bottom-region scoping AND the option-row shape, #242),
//     the two status banners and the busy/idle axis (bottom-region scoping,
//     #220/#153). A transcript-body forgery of these MUST fire nothing. These are
//     the teeth; they pass now and encode 173 and 217.
//  2. Formerly NOT YET PROTECTED — the whole-grid panel classes (mcp,
//     model-select, permissions-config, ask-user). #223 gave each a structural
//     co-signal (a pointer-marked option row for model-select and ask-user, the
//     picker highlight color for mcp/permissions-config); #244 then bound the
//     mcp/permissions-config co-signal to rendered-panel structure — a
//     row-opening highlight for both, plus the three tab words on one row for
//     permissions-config — because the anywhere-highlight was a general light
//     blue present in most frames and added almost nothing. So they too fire
//     nothing on a one-line content forgery. agents was in this set too, but its
//     classifier arm was retired entirely in #245 — its case below now holds
//     because there is no arm, not because a co-signal is unmet. The former
//     characterization test that pinned the gap is now
//     TestWholeGridAnchorsRejectContentForgery, a fire-nothing regression.

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
		// #303: the api-error retry line as transcript-body content. The phrase
		// carries the full two-token structure (API error … Retrying in) plus a
		// counter, so the forgery is non-vacuous — a bare "API error" with no
		// "Retrying in" would trivially not fire for the wrong reason. Region
		// scoping (bannerRegionRows) is the guard.
		{"api-error retry (#303)", "API error · Retrying in 1s · attempt 3/10", HasApiRetry},
		// #304: the mid-response partial-output error line as transcript-body
		// content. The full line carries the distinctive "Connection closed
		// mid-response" token, so the forgery is non-vacuous — a bare "API Error:"
		// with no partial-output token would trivially not fire for the wrong
		// reason. Region scoping (bannerRegionRows) is the guard.
		{"mid-response error (#304)", "API Error: Connection closed mid-response. The response above may be incomplete.", HasMidResponseError},
		// #298: the auto-compaction banner as transcript-body content. The forged
		// string carries the FULL banner — the phrase AND the ▱ progress-bar run —
		// so the forgery is non-vacuous: the bar co-signal is present, and only
		// region scoping (bannerRegionRows) rejects it. This anchor matters more
		// than the others because the phrase "Compacting conversation" is KNOWN to
		// appear in real agent transcripts (this very ticket's body did), which is
		// exactly why the detector pairs the phrase with the bar co-signal.
		{"compaction banner (#298)", "Compacting conversation… ▱▱▱▱▱▱▱▱▱▱ 0%", HasCompacting},
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
	busy := make([]string, 0, len(spinnerGlyphs)+2)
	for _, g := range spinnerGlyphs {
		busy = append(busy, string(g))
	}
	busy = append(busy, InterruptHint)
	// The dot-frame row (#243): the plain dot spinner quoted as transcript body
	// must fire nothing on the busy axis, exactly like the sparkle glyphs and the
	// hint. Both guards reject it — the region scope (it renders above the window)
	// and the row-start shape (embedded mid-prose, it never leads a row). Leading
	// dot as a byte escape (\xc2\xb7 = U+00B7) per the screen-literal discipline.
	busy = append(busy, "\xc2\xb7 Simmering…")
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

// TestPermissionAnchorRejectsStatusRegionQuotation covers permission's in-region
// content form, the #242 fix. Permission is now shape-gated like trust (#219):
// even the prompt phrase quoted in the bottom status region — where the region-
// scoped detector looks — still needs a pointer-marked numbered option row
// directly below it to classify. A bare in-region quotation does not fire. This
// is the exact production forgery the ticket documents: the phrase scrolling
// through the bottom window as prose, with no dialog on screen. Mirrors
// TestTrustAnchorRejectsStatusRegionQuotation; references the anchor by symbol,
// never the literal (the ticket's self-reference discipline).
func TestPermissionAnchorRejectsStatusRegionQuotation(t *testing.T) {
	filler := make([]string, 20)
	for i := range filler {
		filler[i] = "transcript body line"
	}
	// The prompt phrase quoted as content on the very last row: in-region (inside
	// permissionRegionRows of the bottom) with NO option row below it anywhere.
	anchorRow := "log: prompt was \"" + string(anchorPermissionSpaced) + "\""

	forged := gridRows(append(append([]string{}, filler...), anchorRow)...)
	if !strings.Contains(string(forged), string(anchorPermissionSpaced)) {
		t.Fatal("fixture lost the permission anchor — forgery contrast void")
	}
	if got := DetectModalClass(forged); got == ModalClassPermission {
		t.Error("in-region permission-prompt quotation classified as permission, want no fire")
	}

	// Non-vacuity positive control (guards the new lookahead path independent of
	// the .bin fixture): the SAME filler and anchor row, plus a ❯-marked numbered
	// option row directly below it, all in the bottom region — the real dialog
	// shape. The only difference from the forgery above is the added option row,
	// so this proves the lookahead fires on the shape, not on the phrase alone.
	live := gridRows(append(append([]string{}, filler...), anchorRow, "❯ 1. Yes")...)
	if got := DetectModalClass(live); got != ModalClassPermission {
		t.Errorf("in-region prompt + ❯-option row: DetectModalClass = %q, want Permission", got)
	}
}

// TestWholeGridAnchorsRejectContentForgery closes the #221→#223 handoff. Before
// #223 these whole-grid panel classes classified from a single on-screen content
// line, and this test (then TestWholeGridAnchorsStillForgePending223) pinned that
// gap as tracked behaviour. #223 gave each class a structural co-signal, so one
// prose line quoting a class's header text now fires nothing — the assertion
// flipped from "forges" to "Unknown", exactly the handoff the #221 comment
// promised.
func TestWholeGridAnchorsRejectContentForgery(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"mcp title", "the /mcp screen shows Manage MCP servers at the top"},
		{"mcp empty-state", "it printed No MCP servers configured after /doctor"},
		{"ask-user footer", "the picker footer reads Enter to select at the bottom"},
		{"model-select title", "run /model to open the Select model picker"},
		{"agents header+tab", "the Agents modal lists a Running and a Library tab"}, // #245: arm retired, so Unknown by construction (no arm), not by an unmet co-signal
		{"permissions-config", "the Permissions screen has Allow, Ask and Deny tabs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// One on-screen prose line: no ❯-marked option row and no picker
			// highlight color, so neither co-signal is satisfied.
			if got := DetectModalClass(gridRows(tc.line)); got != ModalClassUnknown {
				t.Errorf("%s: DetectModalClass = %q, want Unknown (content forgery must not classify)", tc.name, got)
			}
		})
	}
}

// TestPermissionsConfigTabForgeryRejectedByRowBoundColor is the #244
// content-forgery regression for the permissions-config class. Before #244 the
// arm matched the "Permissions" header and any ONE of Allow/Ask/Deny anywhere on
// the grid, plus the highlight shade ANYWHERE in the snapshot
// (snapHasPickerHighlight). Because that shade is a general light blue claude
// paints on paths and links — present in a large fraction of frames — plain
// prose forged the class: the recording the ticket documents classified
// permissions-config four times on ordinary transcript prose. #244 binds both
// co-signals to rendered-panel structure — the three tab words on ONE rendered
// row (gridRowContainsAll) AND a row-opening highlight (snapHasRowOpeningHighlight).
// Prose reproduces neither. Anchors referenced by symbol, never the literal (the
// ticket's self-reference discipline); \r\n so vt10x renders flat rows (the #150
// grid fixture lesson).
func TestPermissionsConfigTabForgeryRejectedByRowBoundColor(t *testing.T) {
	const (
		hl    = "\x1b[38;5;153m" // index 153 → 175,215,255, the general highlight shade
		reset = "\x1b[39m"
	)

	// (a) The one-line tab forgery. The header AND all three tab words render on
	// one prose line, so the header-anywhere guard and the three-tabs-on-one-row
	// guard are BOTH satisfied; the highlight shade is present only MID-line
	// elsewhere (light blue on a prose path, as a real frame carries it), so the
	// row opens in plain text. Only the row-opening color guard rejects it.
	tabLine := "assistant: the " + string(anchorPermissionsHeader) + " view has " +
		string(anchorPermissionsTabAllow) + ", " + string(anchorPermissionsTabAsk) +
		" and " + string(anchorPermissionsTabDeny) + " tabs"
	pathLine := "see " + hl + "/Users/x/settings.json" + reset + " for config"
	forged := gridRows(tabLine, pathLine)

	// Non-vacuity: the old anywhere-highlight IS present (mirrors the
	// snapHasPickerHighlight guard at the slash-picker case above), so the new
	// row-opening check is provably what rejects the frame — not a missing shade.
	if !snapHasPickerHighlight(forged) {
		t.Fatal("fixture lost the highlight chrome — the forgery contrast is void")
	}
	// And the tab compound holds on one row, so the color guard is not passing
	// vacuously on a missing tab word.
	if !gridRowContainsAll(NewGrid(forged, 0, 0), anchorPermissionsTabAllow, anchorPermissionsTabAsk, anchorPermissionsTabDeny) {
		t.Fatal("fixture lost the one-row tab compound — the color guard would pass vacuously")
	}
	if got := DetectModalClass(forged); got != ModalClassUnknown {
		t.Errorf("one-line tab forgery: DetectModalClass = %q, want Unknown", got)
	}

	// (b) Strengthening for the tab-compound guard, independent of (a): the three
	// tab words scattered across SEPARATE lines, with a genuine row-opening
	// highlight present (a border-rule-shaped line). Here the color guard passes,
	// so the single-row tab compound is the sole rejecter — three tabs never
	// share a row. Leading upper-block glyph as bytes (\xe2\x96\x94 = U+2594),
	// mirroring the real panel's border rule.
	scattered := gridRows(
		hl+"\xe2\x96\x94\xe2\x96\x94\xe2\x96\x94\xe2\x96\x94"+reset, // opens in the highlight shade
		string(anchorPermissionsHeader)+" panel overview",
		"the "+string(anchorPermissionsTabAllow)+" tab grants access",
		"the "+string(anchorPermissionsTabAsk)+" tab prompts first",
		"the "+string(anchorPermissionsTabDeny)+" tab blocks",
	)
	if !snapHasRowOpeningHighlight(scattered) {
		t.Fatal("fixture lost the row-opening highlight — the tab-compound guard would pass vacuously")
	}
	if got := DetectModalClass(scattered); got != ModalClassUnknown {
		t.Errorf("scattered-tabs forgery: DetectModalClass = %q, want Unknown", got)
	}
}

// TestSlashPickerRejectsBodyForgery is the #237 content-forgery regression for
// the slash-picker class. Its two signals are both individually weak: the picker
// highlight color (xterm-256 index 153 and its truecolor twin) is a general
// light blue claude paints on paths, links and markers, so the chrome half is
// present in a large fraction of frames; and before #237 the other half — a
// rendered row beginning `/<letter>` — matched ANYWHERE on the grid, so an
// ordinary transcript line that merely starts with a slash forged the picker.
// The #227 corpus-replay harness caught this firing in 124 production-ok runs,
// the largest modal-class false-fire source. The three forms below are the ones
// sampled from that corpus: an absolute path, a markdown-link fragment, and a
// command name in a diff, each starting a rendered line in the transcript body.
//
// #237 region-scopes the `/`-row signal to the bottom input window
// (pickerRegionRows), where a real picker's command list reaches. A `/`-line in
// the scrolled transcript body renders far above that window, so it no longer
// classifies. Each case carries a real picker-highlight escape, so the chrome
// half IS satisfied: the region scope, not a missing color, is what rejects it.
// \r\n so vt10x renders flat rows (the #150 grid fixture lesson).
func TestSlashPickerRejectsBodyForgery(t *testing.T) {
	const hl = "\x1b[38;5;153m" // index 153 → 175,215,255, a real picker-highlight shade
	forms := []struct{ name, line string }{
		{"absolute-path", hl + "/Users/x/Projects/architecture/README.md"},
		{"markdown-link", hl + "/load](#session-boundary) — see the design note"},
		{"command-in-diff", hl + "/staticcheck passes on the current tree"},
	}
	filler := make([]string, 20) // push the /-line above the bottom region
	for i := range filler {
		filler[i] = "transcript body line"
	}
	for _, f := range forms {
		t.Run(f.name, func(t *testing.T) {
			snap := gridRows(append([]string{f.line}, filler...)...)
			// Non-vacuous: the highlight chrome IS present, so only the region
			// scope stops the fire. A naive whole-grid `/`-row match WOULD forge it.
			if !snapHasPickerHighlight(snap) {
				t.Fatal("fixture lost the highlight chrome — the forgery contrast is void")
			}
			if isSlashPicker(snap) {
				t.Errorf("%s: isSlashPicker = true, want false (transcript-body /-line)", f.name)
			}
			if got := DetectModalClass(snap); got == ModalClassSlashPicker {
				t.Errorf("%s: DetectModalClass = SlashPicker, want not-picker", f.name)
			}
		})
	}

	// Positive control: a genuine picker — a BLOCK of `/`-command rows
	// (≥pickerRowBlockMin, #296) in the bottom region — still classifies; the
	// region scope must not silence a real picker (#237 AC). Same 20-row transcript
	// above it as the forgeries, so the ONLY difference from a forgery is the
	// in-region `/`-row block (two command rows, not one).
	live := gridRows(append(append([]string{}, filler...),
		hl+"/figma-use  (figma) invoke this skill first",
		hl+"/code-review  (dev) review the pending diff")...)
	if !isSlashPicker(live) {
		t.Error("in-region picker block: isSlashPicker = false, want true")
	}
	if got := DetectModalClass(live); got != ModalClassSlashPicker {
		t.Errorf("in-region picker block: DetectModalClass = %q, want SlashPicker", got)
	}
}

// TestSlashPickerRejectsSingleRowForgery is the #296 content-forgery regression:
// the residual class #237's region scope could not reach — a SINGLE bottom-region
// `/`-row. A shell-command output line wrapping so its continuation begins with a
// path token, or claude's footer tip wrapping to a `/`-command line, lands INSIDE
// the input window (unlike the #237 body forgeries, which the region scope pushes
// above it). Each takes the exact shape the pre-#296 detector accepted: one
// in-region `/<word>` row (gridHasPickerRow) plus a picker-highlight shade
// anywhere on the frame (snapHasPickerHighlight) — isSlashPicker's whole pre-#296
// test. #296 binds the row half to picker SHAPE: a real picker is a BLOCK of
// ≥pickerRowBlockMin command rows (gridHasPickerRowBlock), so a single stray
// `/`-continuation no longer forges it.
//
// Mirrors the #244 non-vacuity idiom (TestPermissionsConfigTabForgeryRejectedByRowBoundColor):
// assert both pre-fix weak signals ARE present, then assert the new structural
// co-signal is the rejecter. Fixtures hand-built in Go via gridRows (\r\n flat
// rows), never `.bin` bytes with raw ESC (Go json rejects 0x1b). The trigger
// tokens appear only as test fixture byte data, never in prose (the arc's
// self-reference discipline).
func TestSlashPickerRejectsSingleRowForgery(t *testing.T) {
	const (
		hl    = "\x1b[38;5;153m" // index 153 → 175,215,255, a real picker-highlight shade
		reset = "\x1b[39m"
	)
	// Each token is the leading text of a single wrapped continuation row that
	// begins `/<word>` — the path forms and the footer-tip form the ticket names.
	for _, tok := range []string{
		"/Users/x/Projects/architecture/README.md",
		"/Workspace/pyrycode/tui-driver/pkg",
		"/dev/disk/by-id/nvme-eui.0025",
		"/gradlew assembleRelease --stacktrace",
		"/btw to ask a quick side question",
	} {
		t.Run(tok, func(t *testing.T) {
			// One `/`-row INSIDE the bottom region (the last row), non-`/` context
			// rows above it, and the highlight shade painted on the `/`-row itself
			// (as claude paints a real path). The opposite placement to
			// TestSlashPickerRejectsBodyForgery, whose `/`-line is pushed ABOVE the
			// region by 20 filler rows.
			snap := gridRows(
				"output from the running shell command:",
				"$ find /repo -type f | head",
				hl+tok+reset,
			)
			g := NewGrid(snap, 0, 0)

			// Non-vacuity — both pre-#296 weak signals ARE present, so the new block
			// co-signal (not a missing colour or an out-of-region row) is provably
			// what rejects the frame (mirrors the snapHasPickerHighlight guard in
			// TestSlashPickerRejectsBodyForgery).
			if !gridHasPickerRow(g) {
				t.Fatal("fixture lost the in-region /-row — the forgery contrast is void")
			}
			if !snapHasPickerHighlight(snap) {
				t.Fatal("fixture lost the highlight chrome — the forgery contrast is void")
			}
			// Sanity — the `/`-row is a SINGLE in-region row, not a block, so the
			// gate is not passing vacuously on zero rows.
			if got := gridPickerRowCount(g); got != 1 {
				t.Fatalf("fixture has %d in-region /-rows, want exactly 1 (single-row forgery shape)", got)
			}

			// Rejected: the single-row forgery is not a picker under #296.
			if isSlashPicker(snap) {
				t.Errorf("isSlashPicker = true, want false (single-row /-continuation forgery)")
			}
			if got := DetectModalClass(snap); got == ModalClassSlashPicker {
				t.Errorf("DetectModalClass = SlashPicker, want not-picker")
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
	// #300: the real claude 2.1.199 trust dialog (header 7 rows above its option
	// row) must fire BOTH the startup safety net (HasTrustModal) and the class.
	// The compact constructed fixture above never exercised the widened lookahead.
	trust2 := loadFixture(t, "trust-folder-2.1.199-snapshot.bin")
	if !HasTrustModal(trust2) {
		t.Error("trust 2.1.199 fixture: HasTrustModal = false, want true")
	}
	if got := DetectModalClass(trust2); got != ModalClassTrustFolder {
		t.Errorf("trust 2.1.199 fixture: DetectModalClass = %q, want TrustFolder", got)
	}
	if got := DetectModalClass(loadFixture(t, "permission-snapshot.bin")); got != ModalClassPermission {
		t.Errorf("permission fixture: DetectModalClass = %q, want Permission", got)
	}
	if !HasNetworkFailure(loadFixture(t, "network-failure-snapshot.bin")) {
		t.Error("network fixture: HasNetworkFailure = false, want true")
	}
	// #243 — the extracted dot-frame fixture fires the busy axis. Its only busy
	// anchor is the dot-frame row (no sparkle, no hint in region), so this is the
	// positive control for the dotSpinnerRe arm alongside the negative forgery
	// case in TestBusyIdleAnchorsRejectRegionForgery.
	if !IsThinking(loadFixture(t, "dot-spinner-snapshot.bin")) {
		t.Error("dot-spinner fixture: IsThinking = false, want true")
	}
}
