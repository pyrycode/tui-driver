package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// anchorDetector maps a content anchor label to the structural detector that is
// meant to gate it, so the report can show how often the anchor appeared in
// content WITHOUT the detector firing — the suppressed-forgery count.
var anchorDetector = map[string]string{
	"trust-header":      "modal:trust-folder",
	"network-current":   "network-failure",
	"network-retired":   "network-failure",
	"permission-prompt": "modal:permission",
}

// bucketOf is the (segment, tag) column a cast falls in. Unknown-segment or
// untagged casts collapse into "other" so the four real columns stay clean.
func bucketOf(r castResult) string {
	switch {
	case r.segment == "prod" && r.tag == "ok":
		return "prod-ok"
	case r.segment == "prod" && r.tag == "err":
		return "prod-err"
	case r.segment == "e2e" && r.tag == "ok":
		return "e2e-ok"
	case r.segment == "e2e" && r.tag == "err":
		return "e2e-err"
	default:
		return "other"
	}
}

var buckets = []string{"prod-ok", "prod-err", "e2e-ok", "e2e-err", "other"}

// isSuspect reports whether a fire of detector name in a healthy production run
// is a false-positive suspect. idle and thinking fire in normal runs and are not
// suspects; every modal/banner/shape detector is.
func isSuspect(name string) bool {
	return name != "idle" && name != "thinking"
}

// report writes the aggregate audit to w.
func report(w io.Writer, results []castResult, dir string, stride int, perCast bool) {
	var ok, errc, untagged int
	segCount := map[string]int{}
	for _, r := range results {
		switch r.tag {
		case "ok":
			ok++
		case "err":
			errc++
		default:
			untagged++
		}
		segCount[r.segment]++
	}

	fmt.Fprintf(w, "corpus-replay: %d casts (%d ok, %d err, %d untagged) from %s  [stride=%d]\n",
		len(results), ok, errc, untagged, dir, stride)
	fmt.Fprintf(w, "segments: prod=%d, e2e=%d, unknown=%d\n\n", segCount["prod"], segCount["e2e"], segCount["unknown"])

	// detector -> bucket -> count of casts that fired it.
	fires := map[string]map[string]int{}
	detNames := map[string]bool{}
	for _, r := range results {
		b := bucketOf(r)
		for name := range r.fired {
			detNames[name] = true
			if fires[name] == nil {
				fires[name] = map[string]int{}
			}
			fires[name][b]++
		}
	}

	fmt.Fprintln(w, "structural detector fires (casts in which the detector fired at least one sampled tick):")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  DETECTOR\tprod-ok\tprod-err\te2e-ok\te2e-err\tother")
	for _, name := range sortedDetectors(detNames) {
		row := "  " + name
		for _, b := range buckets {
			row += fmt.Sprintf("\t%d", fires[name][b])
		}
		fmt.Fprintln(tw, row)
	}
	tw.Flush()

	// False-positive suspects: a suspect detector firing in a production -ok run.
	fmt.Fprintln(w, "\nfalse-positive suspects (a modal/banner/shape detector fired in a production -ok run):")
	suspects := 0
	for _, name := range sortedDetectors(detNames) {
		if !isSuspect(name) {
			continue
		}
		var hits []string
		for _, r := range results {
			if bucketOf(r) == "prod-ok" && r.fired[name] {
				hits = append(hits, shortName(r.name))
			}
		}
		if len(hits) == 0 {
			continue
		}
		suspects++
		fmt.Fprintf(w, "  %s: %d cast(s)  e.g. %s\n", name, len(hits), strings.Join(first(hits, 3), ", "))
	}
	if suspects == 0 {
		fmt.Fprintln(w, "  none")
	}

	// Anchor-in-content vs structural fire: suppressed forgeries and drift.
	fmt.Fprintln(w, "\nanchor-in-content (a detection anchor appeared in the run's content):")
	for _, a := range contentAnchors {
		var appeared, alsoFired int
		det := anchorDetector[a.label]
		for _, r := range results {
			if !r.anchors[a.label] {
				continue
			}
			appeared++
			if det != "" && r.fired[det] {
				alsoFired++
			}
		}
		tag := ""
		if a.retired {
			tag = " [retired anchor — the current detector no longer matches it]"
		}
		fmt.Fprintf(w, "  %s: %d cast(s), structural detector %q also fired in %d — %d suppressed%s\n",
			a.label, appeared, det, alsoFired, appeared-alsoFired, tag)
	}

	if perCast {
		fmt.Fprintln(w, "\nper-cast:")
		ptw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range results {
			fmt.Fprintf(ptw, "  %s\t%s\t%s\t%dx%d\tfired=[%s]\tanchors=[%s]\n",
				shortName(r.name), r.tag, r.segment, r.cols, r.rows,
				strings.Join(sortedKeys(r.fired), " "), strings.Join(sortedKeys(r.anchors), " "))
		}
		ptw.Flush()
	}
}

// sortedDetectors orders detector keys: idle, thinking, then the rest sorted, so
// the two normal-activity axes lead and the suspects group after.
func sortedDetectors(set map[string]bool) []string {
	var rest []string
	for k := range set {
		if k == "idle" || k == "thinking" {
			continue
		}
		rest = append(rest, k)
	}
	sort.Strings(rest)
	var out []string
	if set["idle"] {
		out = append(out, "idle")
	}
	if set["thinking"] {
		out = append(out, "thinking")
	}
	return append(out, rest...)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// shortName trims the .cast suffix for compact listing.
func shortName(name string) string { return strings.TrimSuffix(name, ".cast") }

func first(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
