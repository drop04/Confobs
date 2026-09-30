package main

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"confobs"
)

// ---- JSON shapes ----------------------------------------------------------

// checkJSON is the machine-readable form of a check. Sensitive values are always
// redacted, so this output is safe to log or to hand to any program.
type checkJSON struct {
	OK      bool                     `json:"ok"`
	Schema  string                   `json:"schema"`
	Missing []string                 `json:"missing"`
	Typos   []confobs.TypoSuggestion `json:"typos"`
	Invalid []confobs.InvalidValue   `json:"invalid"`
	Unused  []string                 `json:"unused"`
	Values  map[string]string        `json:"values"`
	Sources map[string]string        `json:"sources"`
}

type diffJSON struct {
	checkJSON
	FirstLoad       bool             `json:"first_load"`
	Changed         bool             `json:"changed"`
	Changes         []confobs.Change `json:"changes"`
	Snapshot        string           `json:"snapshot"`
	SnapshotUpdated bool             `json:"snapshot_updated"`
}

// nonNil makes empty lists encode as [] rather than null, so consumers in any
// language can iterate without a nil check.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func newCheckJSON(l *loaded) checkJSON {
	return checkJSON{
		OK:      !l.result.HasProblems(),
		Schema:  l.schemaPath,
		Missing: nonNil(l.result.Missing),
		Typos:   nonNil(l.result.Typos),
		Invalid: nonNil(l.result.Invalid),
		Unused:  nonNil(l.result.Unused),
		Values:  l.schema.Redact(l.resolved.Values),
		Sources: l.resolved.Sources,
	}
}

func writeJSON(w io.Writer, v interface{}) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ---- human output ---------------------------------------------------------

func problemCount(r *confobs.Result) int {
	return len(r.Missing) + len(r.Typos) + len(r.Invalid)
}

// writeProblems lists every error, then every warning.
func writeProblems(w io.Writer, r *confobs.Result) {
	for _, name := range r.Missing {
		fmt.Fprintf(w, "  error    %-22s required, but not set\n", name)
	}
	for _, t := range r.Typos {
		fmt.Fprintf(w, "  error    %-22s required, but not set - %s is set, though. Did you mean %s?\n", t.Expected, t.Found, t.Expected)
	}
	for _, iv := range r.Invalid {
		fmt.Fprintf(w, "  error    %-22s %s\n", iv.Env, iv.Reason)
	}
	writeWarnings(w, r)
}

func writeWarnings(w io.Writer, r *confobs.Result) {
	for _, u := range r.Unused {
		fmt.Fprintf(w, "  warning  %-22s set in the env file, but not declared in the schema\n", u)
	}
}

func writeValues(w io.Writer, l *loaded) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	redacted := l.schema.Redact(l.resolved.Values)
	for _, f := range l.schema.Fields {
		if val, ok := redacted[f.Env]; ok {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", f.Env, l.resolved.Sources[f.Env], val)
		} else {
			fmt.Fprintf(tw, "  %s\t-\t(unset)\n", f.Env)
		}
	}
	tw.Flush()
}

func showValue(v string) string {
	if v == "" {
		return "(unset)"
	}
	return v
}

func writeChanges(w io.Writer, changes []confobs.Change) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range changes {
		fmt.Fprintf(tw, "  %s\t%s\t->\t%s\n", c.Field, showValue(c.Old), showValue(c.New))
	}
	tw.Flush()
}

// ---- check ----------------------------------------------------------------

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	var c commonFlags
	var asJSON bool
	fs := newFlagSet("check", stderr)
	c.register(fs)
	fs.BoolVar(&asJSON, "json", false, "machine-readable output")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}

	l, err := load(c)
	if err != nil {
		return fail(stderr, err)
	}

	code := exitOK
	if l.result.HasProblems() {
		code = exitInvalid
	}

	if asJSON {
		writeJSON(stdout, newCheckJSON(l))
		return code
	}

	if l.result.HasProblems() {
		fmt.Fprintf(stdout, "confobs: FAILED - %d problem(s) against %s\n\n", problemCount(l.result), l.schemaPath)
		writeProblems(stdout, l.result)
		return code
	}
	fmt.Fprintf(stdout, "confobs: OK - %d fields valid against %s\n\n", len(l.schema.Fields), l.schemaPath)
	writeValues(stdout, l)
	if len(l.result.Unused) > 0 {
		fmt.Fprintln(stdout)
		writeWarnings(stdout, l.result)
	}
	return code
}

// ---- diff -----------------------------------------------------------------

const defaultSnapshot = ".confobs-snapshot.json"

func cmdDiff(args []string, stdout, stderr io.Writer) int {
	var c commonFlags
	var asJSON, noUpdate, exitOnDrift bool
	var snapshotPath string
	fs := newFlagSet("diff", stderr)
	c.register(fs)
	fs.BoolVar(&asJSON, "json", false, "machine-readable output")
	fs.StringVar(&snapshotPath, "snapshot", defaultSnapshot, "baseline file")
	fs.BoolVar(&noUpdate, "no-update", false, "compare only; do not rewrite the baseline")
	fs.BoolVar(&exitOnDrift, "exit-code", false, "exit with status 3 when drift is found")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}

	l, err := load(c)
	if err != nil {
		return fail(stderr, err)
	}

	// The baseline only ever advances to a configuration that validates, so it
	// always means "last known good" - a broken deploy cannot become the norm.
	valid := !l.result.HasProblems()
	update := valid && !noUpdate
	drift, err := l.schema.Drift(l.resolved.Values, snapshotPath, update)
	if err != nil {
		return fail(stderr, err)
	}

	code := exitOK
	switch {
	case !valid:
		code = exitInvalid
	case exitOnDrift && drift.Changed():
		code = exitDrift
	}

	if asJSON {
		writeJSON(stdout, diffJSON{
			checkJSON:       newCheckJSON(l),
			FirstLoad:       drift.FirstLoad,
			Changed:         drift.Changed(),
			Changes:         nonNil(drift.Changes),
			Snapshot:        snapshotPath,
			SnapshotUpdated: update,
		})
		return code
	}

	if !valid {
		fmt.Fprintf(stdout, "confobs: FAILED - %d problem(s) against %s\n\n", problemCount(l.result), l.schemaPath)
		writeProblems(stdout, l.result)
		fmt.Fprintln(stdout)
	}
	writeDriftSummary(stdout, drift, snapshotPath, update)
	return code
}

func writeDriftSummary(w io.Writer, drift *confobs.DriftResult, path string, updated bool) {
	switch {
	case drift.FirstLoad && updated:
		fmt.Fprintf(w, "confobs: no earlier snapshot - baseline written to %s\n", path)
	case drift.FirstLoad:
		fmt.Fprintf(w, "confobs: no earlier snapshot at %s (baseline not written)\n", path)
	case !drift.Changed():
		fmt.Fprintf(w, "confobs: no drift since the last snapshot (%s)\n", path)
	default:
		fmt.Fprintf(w, "confobs: config drift - %d change(s) since the last snapshot (%s):\n\n", len(drift.Changes), path)
		writeChanges(w, drift.Changes)
	}
}
