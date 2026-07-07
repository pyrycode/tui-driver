package tuidriver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectModalClassUnknownOnEmpty(t *testing.T) {
	if got := DetectModalClass(nil); got != ModalClassUnknown {
		t.Errorf("DetectModalClass(nil) = %q, want %q", got, ModalClassUnknown)
	}
	if got := DetectModalClass([]byte("idle TUI bytes")); got != ModalClassUnknown {
		t.Errorf("DetectModalClass(idle) = %q, want %q", got, ModalClassUnknown)
	}
}

// Synthetic anchors classify against the rendered grid. Since #152 the anchors
// key on the space-preserved on-screen form; since #223 the anchor TEXT alone is
// never enough — every class needs its structural co-signal too. So the bare
// header/footer phrases below now classify Unknown: that IS the #223 contract.
// The co-signal-satisfying positives live in TestDetectModalClassRealFixtures and
// TestDetectModalClassSyntheticGridFixtures.
func TestDetectModalClassSyntheticAnchors(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want ModalClass
	}{
		// #223: bare header text with no co-signal (no highlight color for the
		// panels, no ❯-option row for the option dialogs) no longer classifies.
		{"mcp title text alone is NOT the mcp panel", []byte("...Manage MCP servers..."), ModalClassUnknown},
		{"agents header + tab text alone is NOT agents", []byte("...Agents...Running..."), ModalClassUnknown},
		{"model-select text alone is NOT model-select", []byte("...Select model..."), ModalClassUnknown},
		{"ask-user footer alone is NOT ask-user", []byte("...Enter to select..."), ModalClassUnknown},
		{"permissions-config text alone is NOT the config modal", []byte("Permissions...Allow...Ask...Deny"), ModalClassUnknown},
		{
			// #151: a `/`-row with no picker highlight chrome is NOT a
			// picker — a lone command-shaped line (or an absolute path) at
			// idle must not phantom-picker. Chrome is now load-bearing.
			"slash-picker bare line without chrome is NOT a picker",
			[]byte("noise\n/figma-use description\n"),
			ModalClassUnknown,
		},
		{
			// #151: normal indexed foreground (index 246 → gray 148) is not
			// a highlight shade, so this fails the chrome check → Unknown.
			"slash-picker row in normal indexed color (no highlight) is NOT a picker",
			[]byte("noise\n\x1b[38;5;246m/figma-use\x1b[39m\n"),
			ModalClassUnknown,
		},
		{
			// #151: normal truecolor foreground (148,148,148) is not a
			// highlight shade → fails chrome → Unknown.
			"slash-picker row in normal truecolor (no highlight) is NOT a picker",
			[]byte("noise\n\x1b[38;2;148;148;148m/figma-use\x1b[39m\n"),
			ModalClassUnknown,
		},
		{
			// Stays a picker: the mid-row 38;5;153 is a real highlight shade
			// (index 153 → 175,215,255), so chrome is present.
			"slash-picker filtered (multiple colors mid-row)",
			[]byte("noise\n\x1b[38;5;246m/\x1b[38;5;153mp\x1b[38;5;246mlugin desc\n"),
			ModalClassSlashPicker,
		},
		{
			"slash-picker truecolor highlighted row",
			[]byte("noise\n\x1b[38;2;177;185;249m/code-review desc\n"),
			ModalClassSlashPicker,
		},
		{
			"hint-bar text alone is NOT a picker (welcome banner false-positive guard)",
			[]byte("...? for shortcuts · ← for agents..."),
			ModalClassUnknown,
		},
		{
			"slash mid-line is NOT a picker (markdown body false-positive guard)",
			[]byte("see /usr/local/bin for binaries"),
			ModalClassUnknown,
		},
		{
			// #219: trust now needs the dialog shape (header + a pointer-marked
			// option row), not the header phrase alone. A structurally complete
			// grid still classifies; the header-alone forgery is pinned Unknown
			// by TestDetectModalClassTrustRequiresDialogStructure.
			"trust-folder dialog",
			[]byte("Quick safety check: Is this a project you trust?\r\n❯ 1. Yes\r\n  2. No\r\n"),
			ModalClassTrustFolder,
		},
		{"permission spaced", []byte("...Do you want to proceed..."), ModalClassPermission},
		{"permissions header alone is NOT a config modal", []byte("see Permissions docs for details"), ModalClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectModalClass(tc.in); got != tc.want {
				t.Errorf("DetectModalClass(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// #152: the space-stripped anchor forms were a StripANSI artifact (CSI
// cursor-forward moves eating inter-word spaces). The rendered grid restores
// that spacing, so these forms can never appear in it. DetectModalClass now
// matches the grid, so they no longer classify — the dead variants were removed,
// not carried as no-op checks (AC).
func TestDetectModalClassStrippedFormsNoLongerMatch(t *testing.T) {
	for _, in := range []string{
		"...ManageMCPservers...",
		"...NoMCPserversconfigured.Pleaserun/doctor...",
		"...Entertoselect...",
		"...Quicksafetycheck...",
		"...Doyouwanttoproceed...",
		"...Selectmodel...",
	} {
		if got := DetectModalClass([]byte(in)); got != ModalClassUnknown {
			t.Errorf("DetectModalClass(%q) = %q, want Unknown (stripped form must not classify)", in, got)
		}
	}
}

// TestDetectModalClassPermissionRegion is the CRITICAL B regression: an
// identical "Do you want to proceed?" phrase forged in the on-screen transcript
// body ABOVE the overlay must not classify as Permission, while the same phrase
// in the live bottom overlay region must. Uses \r\n so vt10x renders flat rows
// (the #150 grid fixture lesson).
func TestDetectModalClassPermissionRegion(t *testing.T) {
	const prompt = "Do you want to proceed?"
	body := strings.Repeat("transcript body line\r\n", 25)

	// Forged above the overlay: the prompt at the top, pushed far above the
	// bottom region by the transcript body below it.
	forged := []byte(prompt + "\r\n" + body)
	// Sanity: a naive whole-snapshot substring match WOULD misclassify this —
	// the region scoping is exactly what rejects it.
	if !strings.Contains(string(forged), "Do you want to proceed") {
		t.Fatal("fixture lost the forged phrase")
	}
	if got := DetectModalClass(forged); got != ModalClassUnknown {
		t.Errorf("forged-above-region: DetectModalClass = %q, want Unknown", got)
	}

	// Live overlay: the prompt sits in the bottom region, above its options and
	// the Esc footer, exactly as the real modal renders.
	live := []byte(body + prompt + "\r\n❯ 1. Yes\r\n  2. No\r\n(Esc to cancel)\r\n")
	if got := DetectModalClass(live); got != ModalClassPermission {
		t.Errorf("live-overlay: DetectModalClass = %q, want Permission", got)
	}
}

func TestDetectModalClassMatchesControlSequenceWrappedAnchor(t *testing.T) {
	// Spaced anchor wrapped in CSI + OSC noise — the grid render consumes the
	// control sequences and preserves the spacing, so the predicate still
	// matches. The 38;5;153 highlight-color escape supplies the #223 mcp
	// co-signal (index 153 → 175,215,255), so the panel classifies as it would
	// on the real screen.
	in := []byte("\x1b[1m\x1b]0;title text\x07\x1b[38;5;153mManage MCP servers\x1b[0m")
	if got := DetectModalClass(in); got != ModalClassMCP {
		t.Errorf("DetectModalClass(wrapped) = %q, want %q", got, ModalClassMCP)
	}
}

func TestDetectModalClassAgentsNeedsHeaderAndTab(t *testing.T) {
	// "Agents" header alone is NOT enough — claude renders the word in
	// many contexts (e.g. `← for agents` status bar). Requires Running or
	// Library tab adjacency.
	in := []byte("...press ← for agents...")
	if got := DetectModalClass(in); got == ModalClassAgents {
		t.Errorf("DetectModalClass(status-bar only) = %q, want NOT agents", got)
	}
}

// TestDetectModalClassSlashPickerContract covers #151's two guarantees: the
// slash-picker check runs LAST (a specific anchor wins even when picker signals
// are present), and it requires picker chrome (a highlight color), not just an
// on-screen `/`-row. Fixtures use \r\n so vt10x renders flat rows, not a
// staircase (the #150 grid lesson).
func TestDetectModalClassSlashPickerContract(t *testing.T) {
	const (
		hlTrue = "\x1b[38;2;177;185;249m" // truecolor highlight shade
		hlIdx  = "\x1b[38;5;153m"         // indexed highlight (→ 175,215,255)
		reset  = "\x1b[39m"
	)
	cases := []struct {
		name string
		in   []byte
		want ModalClass
	}{
		{
			// Lone absolute path on screen, no highlight → not a picker.
			// Grid-region alone wouldn't reject it (the path IS on screen);
			// chrome is what rejects it.
			"lone absolute path with no chrome is not a picker",
			[]byte("/Users/x/file.go\r\n"),
			ModalClassUnknown,
		},
		{
			// A real permission modal that also has a `/`-path on screen
			// painted in the highlight shade — i.e. BOTH picker signals
			// present. The reorder must let the Permission anchor win.
			"permission modal wins over co-present picker signals",
			[]byte("Do you want to proceed?\r\n" + hlTrue + "/Users/x/file.go" + reset + "\r\n"),
			ModalClassPermission,
		},
		{
			// Single-match filtered picker (one row) painted in the highlight
			// shade → still a picker. Truecolor + indexed twins.
			"single-match picker with truecolor chrome is a picker",
			[]byte(hlTrue + "/figma-use" + reset + "\r\n"),
			ModalClassSlashPicker,
		},
		{
			"single-match picker with indexed chrome is a picker",
			[]byte(hlIdx + "/figma-use" + reset + "\r\n"),
			ModalClassSlashPicker,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectModalClass(tc.in); got != tc.want {
				t.Errorf("DetectModalClass(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// ParsePicker must stay behaviourally unchanged for a real one-item
	// picker (AC: findPickerRows/ParsePicker untouched).
	t.Run("ParsePicker output unchanged for single-match picker", func(t *testing.T) {
		items := ParsePicker([]byte(hlTrue + "/figma-use" + reset + "\r\n"))
		if len(items) != 1 {
			t.Fatalf("ParsePicker returned %d items, want 1", len(items))
		}
		if items[0].Command != "/figma-use" {
			t.Errorf("ParsePicker command = %q, want %q", items[0].Command, "/figma-use")
		}
		if !items[0].Highlighted {
			t.Errorf("ParsePicker highlighted = false, want true")
		}
	})
}

// Fixture-based regression: real captured snapshots must classify correctly
// against the grid. #152 adds permission-snapshot.bin and trust-folder-
// snapshot.bin — both classes it ports have a real fixture, so pin them to real
// coverage, not just synthetic.
func TestDetectModalClassRealFixtures(t *testing.T) {
	cases := []struct {
		fixture string
		want    ModalClass
	}{
		{"mcp-snapshot.bin", ModalClassMCP},
		// #223: mcp-empty-snapshot.bin is NOT a modal — it is the inline `/mcp`
		// result ("No MCP servers configured…") echoed on an otherwise idle
		// screen (❯ input prompt and the status bar are at the bottom). #223
		// removed the empty-state anchor, so it now classifies Unknown and reads
		// idle. TestDetectModalClassMcpEmptyIsIdleNotModal pins that below.
		{"mcp-empty-snapshot.bin", ModalClassUnknown},
		{"agents-snapshot.bin", ModalClassAgents},
		{"picker-snapshot.bin", ModalClassSlashPicker},
		{"picker-truecolor-snapshot.bin", ModalClassSlashPicker},
		{"permission-snapshot.bin", ModalClassPermission},
		{"trust-folder-snapshot.bin", ModalClassTrustFolder},
		// #222: real-screen fixtures captured live off claude 2.1.199 via
		// spike-multiselect (/model, /permissions) and spike-ask-user. These
		// three classes had no rendered fixture before — ask-user's only .bin was
		// a JSONL dump that classifies Unknown (still used by ask_user_test.go for
		// parsing), and model-select/permissions-config had none.
		{"ask-user-question-screen-snapshot.bin", ModalClassAskUserQuestion},
		{"model-select-snapshot.bin", ModalClassModelSelect},
		{"permissions-config-snapshot.bin", ModalClassPermissionsConfig},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			snap, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			if got := DetectModalClass(snap); got != tc.want {
				t.Errorf("DetectModalClass(%s) = %q, want %q", tc.fixture, got, tc.want)
			}
		})
	}
}

// TestDetectModalClassSyntheticGridFixtures keeps hand-built grids for the three
// classes captured for real in #222, retained as minimal controls that also
// exercise each #223 co-signal: the ❯-marked option row for ask-user and
// model-select, and the picker highlight color (the 38;5;153 escape → 175,215,255)
// for permissions-config. Built with \r\n so vt10x renders flat rows.
func TestDetectModalClassSyntheticGridFixtures(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want ModalClass
	}{
		{
			"ask-user-question grid",
			[]byte("Which option do you prefer?\r\n❯ 1. Alpha\r\n  2. Beta\r\n(Enter to select · Esc to cancel)\r\n"),
			ModalClassAskUserQuestion,
		},
		{
			"model-select grid",
			[]byte("Select model\r\n❯ 1. Default (recommended)\r\n  2. Opus\r\n  3. Sonnet\r\n"),
			ModalClassModelSelect,
		},
		{
			"permissions-config grid with Allow/Ask/Deny tabs",
			[]byte("Permissions\r\n\x1b[38;5;153mAllow\x1b[39m   Ask   Deny\r\n  No rules configured\r\n"),
			ModalClassPermissionsConfig,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectModalClass(tc.in); got != tc.want {
				t.Errorf("DetectModalClass(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDetectModalClassMCPAnchorScrolledOffNotModal is the #155 content-forgery
// regression for the mcp panel (CRITICAL B: "N MCP servers failed" in a pasted
// log forges the modal). mcp is a full-panel class matched across the whole
// visible grid — its title renders ~24 rows from the bottom, so a bottom-region
// window can't be used (#152). The guard for panels is therefore the grid
// excluding scrolled-off history: a real MCP anchor printed as a transcript/log
// line that has scrolled off the top of the visible screen must not classify.
// A naive bytes/strings.Contains over the raw buffer WOULD forge it.
//
// The permission body-forgery case (AC #2) is pinned separately by
// TestDetectModalClassPermissionRegion; the idle/busy pair by #153 in
// state_test.go. Do not duplicate them here.
func TestDetectModalClassMCPAnchorScrolledOffNotModal(t *testing.T) {
	// The real mcp title AND a picker-highlight escape, as a log line — both
	// #223 co-signals present — so grid exclusion is the sole reason it must not
	// classify once it scrolls off the visible screen.
	rows := []string{"\x1b[38;5;153mManage MCP servers\x1b[39m"} // real anchor + highlight, as a log line
	for i := 0; i < 45; i++ {                                     // enough output to scroll it off a 40-row screen
		rows = append(rows, "transcript body line")
	}
	rows = append(rows, "❯ ") // idle prompt at the bottom; no modal is up
	forged := gridRows(rows...)

	// Contrast that makes the test non-vacuous: the anchor IS in the raw bytes,
	// so a naive whole-buffer substring match would forge MCP. The grid never
	// sees it because it scrolled off the visible screen.
	if !strings.Contains(string(forged), "Manage MCP servers") {
		t.Fatal("fixture lost the forged anchor — the forgery contrast is void")
	}
	if got := DetectModalClass(forged); got != ModalClassUnknown {
		t.Errorf("scrolled-off mcp anchor: DetectModalClass = %q, want Unknown", got)
	}

	// Positive control: the genuine mcp panel still classifies.
	snap, err := os.ReadFile(filepath.Join("testdata", "mcp-snapshot.bin"))
	if err != nil {
		t.Fatalf("read mcp fixture: %v", err)
	}
	if got := DetectModalClass(snap); got != ModalClassMCP {
		t.Errorf("mcp-snapshot.bin positive control: DetectModalClass = %q, want MCP", got)
	}
}

// TestDetectModalClassMcpEmptyIsIdleNotModal pins the #223 decision on the
// inline `/mcp` empty-state echo. mcp-empty-snapshot.bin is the "No MCP servers
// configured…" result of running /mcp with strict MCP, sitting on an otherwise
// idle screen (the ❯ input prompt and status bar render at the bottom). It is
// NOT a modal. Before #223 the "No MCP servers configured" anchor classified it
// as mcp, which held the modal axis on an idle screen and suppressed idle and
// thinking edge events. #223 removed the anchor: the screen now reads Unknown
// (no modal) and idle.
func TestDetectModalClassMcpEmptyIsIdleNotModal(t *testing.T) {
	snap := loadFixture(t, "mcp-empty-snapshot.bin")
	if got := DetectModalClass(snap); got != ModalClassUnknown {
		t.Errorf("DetectModalClass(mcp-empty) = %q, want Unknown (inline echo, not a modal)", got)
	}
	if !IsIdle(snap) {
		t.Errorf("IsIdle(mcp-empty) = false, want true (the screen is idle, ready for input)")
	}
}

// TestDetectModalClassTrustRequiresDialogStructure is the #219 content-forgery
// regression for the trust-folder class. The trust header ("Quick safety check")
// is a full-panel anchor with no bottom-region scope, so a bare grid-wide match
// forges the modal from any on-screen line that quotes it — a ticket body or a
// source-file read. Because the runner treats a mid-run trust detection as
// fatal, that forgery aborted real runs (ticket 217; the 2026-07-06 developer
// abort). The fix requires the dialog's structural co-signal: a pointer-marked
// numbered option row within a few rows below the header. Prose does not
// reproduce that shape.
//
// The genuine dialog is pinned by TestDetectModalClassRealFixtures
// (trust-folder-snapshot.bin) and TestHasTrustModalDetectsRealFixture. \r\n so
// vt10x renders flat rows (the #150 grid fixture lesson).
func TestDetectModalClassTrustRequiresDialogStructure(t *testing.T) {
	// (a) Transcript-echo: the header quoted in prose, no dialog-shaped option
	// row below it. A naive whole-grid match WOULD forge trust here.
	echo := []byte("assistant: the folder-trust prompt reads \"Quick safety check: Is\r\n" +
		"this a project you created or one you trust?\" and offers yes or no.\r\n")
	if !strings.Contains(string(echo), "Quick safety check") {
		t.Fatal("fixture lost the header phrase — the forgery contrast is void")
	}
	if got := DetectModalClass(echo); got != ModalClassUnknown {
		t.Errorf("transcript-echo forgery: DetectModalClass = %q, want Unknown", got)
	}

	// (b) Source-read with a line-number gutter, as an agent reading
	// permission.go would render it.
	source := []byte("52\t// anchorTrustHeaderSpaced is the trust-folder header line\r\n" +
		"54\tvar anchorTrustHeaderSpaced = []byte(\"Quick safety check\")\r\n")
	if !strings.Contains(string(source), "Quick safety check") {
		t.Fatal("fixture lost the header phrase — the forgery contrast is void")
	}
	if got := DetectModalClass(source); got != ModalClassUnknown {
		t.Errorf("source-read forgery: DetectModalClass = %q, want Unknown", got)
	}

	// (c) A numbered list quoting the header but WITHOUT the pointer marker is
	// still content, not a dialog — the ❯ is the load-bearing signal.
	numbered := []byte("Steps for the Quick safety check flow:\r\n" +
		"1. Read the header\r\n" +
		"2. Pick an option\r\n")
	if got := DetectModalClass(numbered); got != ModalClassUnknown {
		t.Errorf("unmarked-numbered forgery: DetectModalClass = %q, want Unknown", got)
	}

	// Positive control: the genuine dialog shape still classifies.
	live := []byte("Quick safety check: Is this a project you created or one you trust?\r\n" +
		"❯ 1. Yes, I trust this folder\r\n  2. No, I selected this folder by mistake\r\n(Esc to cancel)\r\n")
	if got := DetectModalClass(live); got != ModalClassTrustFolder {
		t.Errorf("genuine trust dialog: DetectModalClass = %q, want TrustFolder", got)
	}
}
