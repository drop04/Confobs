package confobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

// Change describes one field whose value differed between the previous
// snapshot and the current config state. Sensitive fields are redacted in
// both Old and New — the fact that a sensitive value changed is still
// reported, just not what it changed to/from. An empty string means "not set".
type Change struct {
	Field string `json:"field"`
	Old   string `json:"old"`
	New   string `json:"new"`
}

// DriftResult summarizes what changed since the last snapshot.
type DriftResult struct {
	FirstLoad bool     `json:"first_load"` // true if there was no prior snapshot to compare against
	Changes   []Change `json:"changes"`    // empty if nothing changed (or on first load)
}

// Changed reports whether any field differs from the last snapshot.
func (d *DriftResult) Changed() bool {
	return len(d.Changes) > 0
}

// snapshot is the on-disk representation: a flat map of env-var-name ->
// stringified value (a fingerprint for sensitive fields), which is enough to
// diff against without knowing anything about the program that wrote it.
type snapshot map[string]string

// sensitiveRedactedLabel is what's shown in a Change for a sensitive field.
// The on-disk snapshot never stores this literal string as the comparison
// value (see fingerprint) — if it did, two different secrets would both redact
// to the same value and drift would go undetected.
const sensitiveRedactedLabel = "***REDACTED***"

// fingerprint returns a short SHA-256 fingerprint of a secret. A changed secret
// still produces a different snapshot value (detectable drift) without the
// secret itself ever touching disk.
func fingerprint(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// snapshotFieldString returns what gets written to the snapshot file for one
// struct field. Non-sensitive fields are stored as plain text.
func snapshotFieldString(v reflect.Value, fi fieldInfo) string {
	raw := rawFieldString(v, fi)
	if !fi.Sensitive {
		return raw
	}
	return fingerprint(raw)
}

// diffKey names one comparable item: Key indexes the snapshot, Label is what a
// human sees (the Go field name for structs, the variable name for schemas).
type diffKey struct {
	Key       string
	Label     string
	Sensitive bool
}

// diffSnapshots is the one diff implementation shared by Reload (struct tags)
// and Schema.Drift (schema files).
func diffSnapshots(keys []diffKey, prev, current snapshot) []Change {
	var changes []Change
	for _, k := range keys {
		oldVal, existed := prev[k.Key]
		newVal := current[k.Key]
		if existed && oldVal == newVal {
			continue
		}
		if !existed && newVal == "" {
			continue // a field added to the schema but still unset is not a change
		}
		changes = append(changes, Change{
			Field: k.Label,
			Old:   displayValue(oldVal, k.Sensitive),
			New:   displayValue(newVal, k.Sensitive),
		})
	}
	return changes
}

func displayValue(v string, sensitive bool) string {
	if sensitive && v != "" {
		return sensitiveRedactedLabel
	}
	return v
}

// Reload re-reads cfg from the current environment (identical semantics to
// Load — defaults, type coercion, required-field/typo checking) and then
// diffs the resulting values against the last snapshot stored at
// snapshotPath, writing an updated snapshot afterward.
//
// If snapshotPath doesn't exist yet, Reload treats this as the first load:
// it writes the initial snapshot and returns a DriftResult with
// FirstLoad=true and no changes.
//
// Reload also returns the same *Result as Load — callers that only care about
// drift and are confident their env is valid can ignore it, but it's surfaced
// so a broken required var isn't silently swallowed during a reload.
func Reload(cfg interface{}, snapshotPath string) (*DriftResult, *Result, error) {
	// Even if required vars are missing, we still attempt to diff whatever
	// did resolve, so the drift log doesn't go silent during a partially
	// broken config change — loadResult is non-nil either way.
	loadResult, _ := Load(cfg)

	fields, err := parseTags(cfg)
	if err != nil {
		return nil, loadResult, err
	}
	v := reflect.ValueOf(cfg)
	current := make(snapshot, len(fields))
	keys := make([]diffKey, len(fields))
	for i, fi := range fields {
		current[fi.EnvName] = snapshotFieldString(v, fi)
		keys[i] = diffKey{Key: fi.EnvName, Label: fi.Name, Sensitive: fi.Sensitive}
	}

	drift, err := compareAndStore(keys, current, snapshotPath, true)
	return drift, loadResult, err
}

// Snapshot writes the current field values of cfg to snapshotPath without
// performing a reload or diff first. Useful for establishing a baseline
// right after a known-good Load, independent of the Reload lifecycle.
func Snapshot(cfg interface{}, snapshotPath string) error {
	fields, err := parseTags(cfg)
	if err != nil {
		return err
	}
	v := reflect.ValueOf(cfg)
	current := make(snapshot, len(fields))
	for _, fi := range fields {
		current[fi.EnvName] = snapshotFieldString(v, fi)
	}
	return writeSnapshot(snapshotPath, current)
}

// Drift compares the resolved values of a schema-governed program against the
// snapshot at snapshotPath. It is the schema-file counterpart of Reload and
// uses the same snapshot format and diff logic.
//
// values is Resolved.Values (fields that did not resolve count as unset). When
// update is true the snapshot is rewritten to the current state afterwards;
// callers pass false to look without touching the baseline, or when the
// configuration is invalid and should not become the new baseline.
func (s *Schema) Drift(values map[string]string, snapshotPath string, update bool) (*DriftResult, error) {
	current := make(snapshot, len(s.Fields))
	keys := make([]diffKey, len(s.Fields))
	for i, f := range s.Fields {
		val := ""
		if raw, ok := values[f.Env]; ok {
			val = normalizeValue(f.Type, raw)
			if f.Sensitive && val != "" {
				val = fingerprint(val)
			}
		}
		current[f.Env] = val
		keys[i] = diffKey{Key: f.Env, Label: f.Env, Sensitive: f.Sensitive}
	}
	return compareAndStore(keys, current, snapshotPath, update)
}

// compareAndStore loads the previous snapshot, diffs it against current, and
// optionally writes current back as the new baseline.
func compareAndStore(keys []diffKey, current snapshot, path string, update bool) (*DriftResult, error) {
	prev, err := readSnapshot(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("confobs: reading snapshot: %w", err)
		}
		// First load: nothing to diff against.
		if update {
			if werr := writeSnapshot(path, current); werr != nil {
				return nil, werr
			}
		}
		return &DriftResult{FirstLoad: true}, nil
	}

	drift := &DriftResult{Changes: diffSnapshots(keys, prev, current)}
	if update {
		if err := writeSnapshot(path, current); err != nil {
			return drift, err
		}
	}
	return drift, nil
}

func readSnapshot(path string) (snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("confobs: parsing snapshot at %s: %w", path, err)
	}
	return snap, nil
}

func writeSnapshot(path string, snap snapshot) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("confobs: creating snapshot dir: %w", err)
		}
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("confobs: encoding snapshot: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("confobs: writing snapshot: %w", err)
	}
	return nil
}
