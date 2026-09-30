package confobs

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sampleYAML = `
# shared contract for every service
version: 1
fields:
  - env: DATABASE_URL
    required: true
    sensitive: true
    description: Connection string   # trailing comment
  - env: PORT
    type: int
    default: "8080"
  - env: LOG_LEVEL
    default: info
  - env: REQUEST_TIMEOUT
    type: duration
    default: 10s
  - env: FEATURE_BETA
    type: bool
    default: 'false'
`

const sampleJSON = `{
  "version": 1,
  "fields": [
    {"env": "DATABASE_URL", "required": true, "sensitive": true, "description": "Connection string"},
    {"env": "PORT", "type": "int", "default": 8080},
    {"env": "LOG_LEVEL", "default": "info"},
    {"env": "REQUEST_TIMEOUT", "type": "duration", "default": "10s"},
    {"env": "FEATURE_BETA", "type": "bool", "default": false}
  ]
}`

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustSchema(t *testing.T, yaml string) *Schema {
	t.Helper()
	s, err := LoadSchema(writeTemp(t, "schema.yaml", yaml))
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	return s
}

func TestLoadSchema_YAMLAndJSONAreEquivalent(t *testing.T) {
	y := mustSchema(t, sampleYAML)
	j, err := LoadSchema(writeTemp(t, "schema.json", sampleJSON))
	if err != nil {
		t.Fatalf("LoadSchema json: %v", err)
	}
	if !reflect.DeepEqual(y, j) {
		t.Errorf("YAML and JSON schemas differ:\nyaml: %+v\njson: %+v", y, j)
	}
	if len(y.Fields) != 5 {
		t.Fatalf("fields = %d, want 5", len(y.Fields))
	}
	db := y.Fields[0]
	if db.Env != "DATABASE_URL" || !db.Required || !db.Sensitive || db.Type != TypeString {
		t.Errorf("DATABASE_URL parsed wrongly: %+v", db)
	}
	if db.Description != "Connection string" {
		t.Errorf("trailing comment leaked into description: %q", db.Description)
	}
	if y.Fields[1].Type != TypeInt || y.Fields[1].Default != "8080" {
		t.Errorf("PORT parsed wrongly: %+v", y.Fields[1])
	}
}

func TestLoadSchema_Errors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // substring of the error
	}{
		{"unknown top-level key, with suggestion", "feilds:\n  - env: A\n", `did you mean "fields"`},
		{"unknown field key, with suggestion", "fields:\n  - env: A\n    requird: true\n", `did you mean "required"`},
		{"tab indentation", "fields:\n\t- env: A\n", "tabs are not allowed"},
		{"bad bool", "fields:\n  - env: A\n    required: maybe\n", "must be true or false"},
		{"flow style", "fields: []\n", "block list"},
		{"flow style value", "fields:\n  - env: A\n    default: [1, 2]\n", "flow-style"},
		{"block scalar", "fields:\n  - env: A\n    description: |\n", "block scalars"},
		{"missing env", "fields:\n  - type: int\n", `missing "env"`},
		{"bad env name", "fields:\n  - env: 9BAD\n", "not a valid environment variable name"},
		{"duplicate", "fields:\n  - env: A\n  - env: A\n", "more than once"},
		{"unknown type", "fields:\n  - env: A\n    type: integer\n", "unknown type"},
		{"default of wrong type", "fields:\n  - env: A\n    type: int\n    default: abc\n", "not a valid int"},
		{"required with default", "fields:\n  - env: A\n    required: true\n    default: x\n", "cannot also have a default"},
		{"no fields", "version: 1\n", "no fields declared"},
		{"misaligned key", "fields:\n  - env: A\n     type: int\n", "misaligned"},
		{"line number reported", "fields:\n  - env: A\n    nope: 1\n", ":3:"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadSchema(writeTemp(t, "schema.yaml", c.body))
			if err == nil {
				t.Fatalf("expected an error containing %q, got none", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

func TestLoadSchema_JSONRejectsUnknownKeys(t *testing.T) {
	_, err := LoadSchema(writeTemp(t, "s.json", `{"fields":[{"env":"A","requird":true}]}`))
	if err == nil || !strings.Contains(err.Error(), "requird") {
		t.Errorf("expected unknown-key error naming requird, got %v", err)
	}
}

func TestLoadSchema_YAMLCommentsAndQuotes(t *testing.T) {
	s := mustSchema(t, `
fields:
- env: URL_WITH_HASH          # list at the same indent as its key is legal YAML
  default: "http://x/#frag"   # '#' inside quotes is not a comment
- env: APOSTROPHE
  description: don't strip this # but do strip this
- env: EMPTY_QUOTED
  default: ''
`)
	if got := s.Fields[0].Default; got != "http://x/#frag" {
		t.Errorf("quoted # mangled: %q", got)
	}
	if got := s.Fields[1].Description; got != "don't strip this" {
		t.Errorf("apostrophe/comment handling wrong: %q", got)
	}
	if s.Fields[2].Default != "" {
		t.Errorf("empty quoted default = %q", s.Fields[2].Default)
	}
}

func TestResolve_DefaultsSourcesAndNormalCase(t *testing.T) {
	s := mustSchema(t, sampleYAML)
	res, result := s.Resolve(map[string]string{"DATABASE_URL": "postgres://x", "PORT": "9090"})
	if result.HasProblems() {
		t.Fatalf("unexpected problems: %v", result)
	}
	want := map[string]string{
		"DATABASE_URL": "env", "PORT": "env",
		"LOG_LEVEL": "default", "REQUEST_TIMEOUT": "default", "FEATURE_BETA": "default",
	}
	if !reflect.DeepEqual(res.Sources, want) {
		t.Errorf("sources = %v, want %v", res.Sources, want)
	}
	if res.Values["PORT"] != "9090" || res.Values["LOG_LEVEL"] != "info" {
		t.Errorf("values = %v", res.Values)
	}
}

func TestResolve_ReportsEveryProblemAtOnce(t *testing.T) {
	s := mustSchema(t, `
fields:
  - env: DATABASE_URL
    required: true
  - env: API_KEY
    required: true
    sensitive: true
  - env: PORT
    type: int
    default: "8080"
  - env: TIMEOUT
    type: duration
    default: 5s
`)
	_, result := s.Resolve(map[string]string{
		"DATBASE_URL": "x",            // typo of DATABASE_URL
		"PORT":        "not-a-number", // bad type
		"TIMEOUT":     "soon",         // bad type
	})
	if len(result.Typos) != 1 || result.Typos[0].Expected != "DATABASE_URL" {
		t.Errorf("typos = %+v", result.Typos)
	}
	if !reflect.DeepEqual(result.Missing, []string{"API_KEY"}) {
		t.Errorf("missing = %v, want [API_KEY]", result.Missing)
	}
	if len(result.Invalid) != 2 {
		t.Fatalf("invalid = %+v, want 2 entries", result.Invalid)
	}
	if !strings.Contains(result.Invalid[0].Reason, `got "not-a-number"`) {
		t.Errorf("reason should echo the bad value: %q", result.Invalid[0].Reason)
	}
	if !strings.Contains(result.Invalid[1].Reason, "duration") {
		t.Errorf("reason should describe the expected duration format: %q", result.Invalid[1].Reason)
	}
}

func TestResolve_NeverEchoesASecretInAnError(t *testing.T) {
	s := mustSchema(t, "fields:\n  - env: TOKEN_TTL\n    type: int\n    sensitive: true\n")
	_, result := s.Resolve(map[string]string{"TOKEN_TTL": "hunter2"})
	if len(result.Invalid) != 1 {
		t.Fatalf("invalid = %+v", result.Invalid)
	}
	if strings.Contains(result.Invalid[0].Reason, "hunter2") || strings.Contains(result.Error(), "hunter2") {
		t.Errorf("sensitive value leaked into error: %q", result.Invalid[0].Reason)
	}
}

func TestTypoHeuristics(t *testing.T) {
	s := mustSchema(t, `
fields:
  - env: DB_HOST
    required: true
  - env: DB_PORT
    default: "5432"
  - env: DATABASE_URL
    required: true
  - env: API_KEY
    required: true
`)
	_, result := s.Resolve(map[string]string{
		"DB_PORT":      "5432", // declared field: must NOT be suggested as a typo of DB_HOST
		"database_url": "x",    // lowercase spelling of a required var: SHOULD be suggested
		"PATH":         "/bin", // unrelated system variable: must not be suggested for API_KEY
		"PAGE":         "1",
	})
	if len(result.Typos) != 1 || result.Typos[0].Expected != "DATABASE_URL" || result.Typos[0].Found != "database_url" {
		t.Errorf("typos = %+v, want lowercase database_url suggested for DATABASE_URL", result.Typos)
	}
	wantMissing := []string{"DB_HOST", "API_KEY"}
	if !reflect.DeepEqual(result.Missing, wantMissing) {
		t.Errorf("missing = %v, want %v (DB_PORT and PATH must not be offered as typos)", result.Missing, wantMissing)
	}
}

func TestTypoThresholdScalesWithLength(t *testing.T) {
	cases := map[string]int{"PORT": 1, "API_KEY": 1, "DATABASE": 2, "DATABASE_URL": 3, "A_VERY_LONG_VARIABLE_NAME": 3}
	for name, want := range cases {
		if got := typoThreshold(name); got != want {
			t.Errorf("typoThreshold(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestRedactAndUnused(t *testing.T) {
	s := mustSchema(t, sampleYAML)
	red := s.Redact(map[string]string{"DATABASE_URL": "postgres://secret", "PORT": "1"})
	if red["DATABASE_URL"] != sensitiveRedactedLabel || red["PORT"] != "1" {
		t.Errorf("redact = %v", red)
	}
	if got := s.Unused([]string{"PORT", "OLD_THING", "DATABASE_URL", "ANOTHER"}); !reflect.DeepEqual(got, []string{"OLD_THING", "ANOTHER"}) {
		t.Errorf("unused = %v", got)
	}
}

func TestParseEnvFile(t *testing.T) {
	p := writeTemp(t, ".env", `# comment
export A=1
B="two words"   
C='single'
D=plain # trailing comment
E=has=equals
F=
G="unterminated
A=overridden
no-equals-line
`)
	ef, err := ParseEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"A": "overridden", "B": "two words", "C": "single", "D": "plain",
		"E": "has=equals", "F": "", "G": `"unterminated`,
	}
	if !reflect.DeepEqual(ef.Values, want) {
		t.Errorf("values = %v\nwant     %v", ef.Values, want)
	}
	if !reflect.DeepEqual(ef.Keys, []string{"A", "B", "C", "D", "E", "F", "G"}) {
		t.Errorf("keys = %v (duplicates must collapse, first-seen order kept)", ef.Keys)
	}
}

func TestSchemaDrift(t *testing.T) {
	s := mustSchema(t, sampleYAML)
	snap := filepath.Join(t.TempDir(), "snap.json")

	base := map[string]string{"DATABASE_URL": "postgres://secret-v1", "PORT": "8080", "LOG_LEVEL": "info"}

	// First look: nothing to compare against, baseline is written.
	d, err := s.Drift(base, snap, true)
	if err != nil || !d.FirstLoad || d.Changed() {
		t.Fatalf("first drift = %+v, err %v", d, err)
	}
	raw, _ := os.ReadFile(snap)
	if strings.Contains(string(raw), "secret-v1") {
		t.Error("snapshot contains the plaintext secret")
	}

	// Identical values, including spellings that normalise the same: no drift.
	same := map[string]string{"DATABASE_URL": "postgres://secret-v1", "PORT": "08080", "LOG_LEVEL": "info"}
	if d, _ = s.Drift(same, snap, true); d.Changed() {
		t.Errorf("08080 vs 8080 reported as drift: %+v", d.Changes)
	}

	// Look without touching the baseline.
	changed := map[string]string{"DATABASE_URL": "postgres://secret-v2", "PORT": "9090", "LOG_LEVEL": "debug"}
	d, _ = s.Drift(changed, snap, false)
	if len(d.Changes) != 3 {
		t.Fatalf("changes = %+v, want 3", d.Changes)
	}
	byField := map[string]Change{}
	for _, c := range d.Changes {
		byField[c.Field] = c
	}
	if c := byField["PORT"]; c.Old != "8080" || c.New != "9090" {
		t.Errorf("PORT change = %+v", c)
	}
	if c := byField["DATABASE_URL"]; c.Old != sensitiveRedactedLabel || c.New != sensitiveRedactedLabel {
		t.Errorf("secret rotation must be reported, redacted: %+v", c)
	}
	// update=false must have left the baseline alone: same drift the second time.
	if d2, _ := s.Drift(changed, snap, false); len(d2.Changes) != 3 {
		t.Errorf("update=false modified the snapshot")
	}
	// update=true adopts the new baseline.
	s.Drift(changed, snap, true)
	if d3, _ := s.Drift(changed, snap, true); d3.Changed() {
		t.Errorf("drift still reported after baseline update: %+v", d3.Changes)
	}

	// A variable that disappears is reported as changed to unset.
	gone := map[string]string{"DATABASE_URL": "postgres://secret-v2", "PORT": "9090"}
	d, _ = s.Drift(gone, snap, false)
	if len(d.Changes) != 1 || d.Changes[0].Field != "LOG_LEVEL" || d.Changes[0].New != "" {
		t.Errorf("removed variable = %+v", d.Changes)
	}
}

func TestLoad_ReportsInvalidValuesAndSetsTheRest(t *testing.T) {
	type cfg struct {
		Port    int    `env:"CT_PORT"`
		Name    string `env:"CT_NAME"`
		Retries int8   `env:"CT_RETRIES"`
	}
	t.Setenv("CT_PORT", "abc")
	t.Setenv("CT_NAME", "svc")
	t.Setenv("CT_RETRIES", "300") // does not fit in int8

	var c cfg
	result, err := Load(&c)
	if err == nil {
		t.Fatal("expected an error")
	}
	if c.Name != "svc" {
		t.Errorf("Name = %q; valid fields must still be set", c.Name)
	}
	if len(result.Invalid) != 1 || result.Invalid[0].Env != "CT_PORT" {
		t.Errorf("invalid = %+v", result.Invalid)
	}
	// int8 overflow is caught by setField, not the type check.
	if !strings.Contains(err.Error(), "CT_RETRIES") && !strings.Contains(err.Error(), "Retries") {
		t.Errorf("overflow not reported: %v", err)
	}
}

func TestParseTags_RejectsUnsupportedAndUnexported(t *testing.T) {
	type badType struct {
		Items []string `env:"ITEMS"`
	}
	if _, err := parseTags(&badType{}); err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Errorf("slice field: err = %v", err)
	}
	type unexported struct {
		secret string `env:"SECRET"`
	}
	if _, err := parseTags(&unexported{}); err == nil || !strings.Contains(err.Error(), "unexported") {
		t.Errorf("unexported field: err = %v", err)
	}
}
