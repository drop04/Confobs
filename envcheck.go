package confobs

import (
	"fmt"
	"os"
	"reflect"
	"strings"
)

// TypoSuggestion is emitted when a required env var is missing but another
// currently-set env var is a close string match — the classic
// DATABASE_URL / DATBASE_URL situation.
type TypoSuggestion struct {
	Expected string `json:"expected"` // the env var confobs was looking for
	Found    string `json:"found"`    // the env var that's actually set, and looks like a typo of Expected
	Distance int    `json:"distance"` // edit distance between the two
}

func (t TypoSuggestion) String() string {
	return fmt.Sprintf("env var %q is not set, but %q is — did you mean %q?", t.Expected, t.Found, t.Expected)
}

// Result collects everything confobs noticed about the environment relative to
// a schema (or a tagged config struct).
type Result struct {
	Missing []string         `json:"missing"` // required vars with no value and no plausible typo match
	Typos   []TypoSuggestion `json:"typos"`   // required vars that are probably just misspelled
	Invalid []InvalidValue   `json:"invalid"` // vars that are set but not of the declared type
	Unused  []string         `json:"unused"`  // keys present in an env file but not declared anywhere
}

// HasProblems reports whether the result contains anything worth stopping for
// (missing required vars, likely typos, or badly typed values). Unused vars
// are informational and excluded on purpose.
func (r *Result) HasProblems() bool {
	return len(r.Missing) > 0 || len(r.Typos) > 0 || len(r.Invalid) > 0
}

// Error implements the error interface so a *Result can be returned directly
// from Load when something is wrong, while still giving callers access to the
// structured detail via errors.As.
func (r *Result) Error() string {
	var parts []string
	if len(r.Missing) > 0 {
		parts = append(parts, "missing required env vars: "+strings.Join(r.Missing, ", "))
	}
	for _, t := range r.Typos {
		parts = append(parts, t.String())
	}
	for _, iv := range r.Invalid {
		parts = append(parts, fmt.Sprintf("invalid value for %s: %s", iv.Env, iv.Reason))
	}
	return strings.Join(parts, "; ")
}

// schemaFromFields converts the struct-tag view of a config into a Schema, so
// the struct-tag API runs on exactly the same engine as schema files.
func schemaFromFields(fields []fieldInfo) *Schema {
	s := &Schema{}
	for _, fi := range fields {
		s.Fields = append(s.Fields, SchemaField{
			Env:       fi.EnvName,
			Type:      fi.Type,
			Required:  fi.Required,
			Default:   fi.Default,
			Sensitive: fi.Sensitive,
		})
	}
	return s
}

// environMap returns the process environment as a map.
func environMap() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			env[k] = v
		}
	}
	return env
}

// Load populates cfg from the current process environment (os.Environ),
// applying `default` tag values where the env var is unset, and type-coercing
// string values into the target field types (string, int*, bool, float*,
// time.Duration).
//
// If any problem is found — a `required` field with no value and no default,
// a likely typo of one, or a value that is not of the field's type — Load
// returns a non-nil *Result (which also satisfies error) listing every problem
// at once. Fields that DID resolve are still set on cfg even when Load returns
// an error, so partial configs are usable for logging/diagnostics.
func Load(cfg interface{}) (*Result, error) {
	fields, err := parseTags(cfg)
	if err != nil {
		return nil, err
	}

	resolved, result := schemaFromFields(fields).Resolve(environMap())

	v := reflect.ValueOf(cfg)
	for _, fi := range fields {
		raw, ok := resolved.Values[fi.EnvName]
		if !ok {
			continue
		}
		if err := setField(v, fi, raw); err != nil {
			return result, err
		}
	}

	if result.HasProblems() {
		return result, result
	}
	return result, nil
}

// CheckUnused parses a simple .env file (KEY=VALUE per line, '#' comments,
// blank lines ignored) and returns keys defined in the file that don't
// correspond to any `env` tag on cfg. This catches config drift after
// refactors — a var that used to matter and got left behind in .env.
func CheckUnused(cfg interface{}, envFilePath string) ([]string, error) {
	fields, err := parseTags(cfg)
	if err != nil {
		return nil, err
	}
	ef, err := ParseEnvFile(envFilePath)
	if err != nil {
		return nil, err
	}
	return schemaFromFields(fields).Unused(ef.Keys), nil
}
