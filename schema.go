package confobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FieldType is the type a configuration value must be parseable as.
type FieldType string

// The supported field types. Values always travel as strings (that is what an
// environment variable is); the type only describes what the string must
// contain.
const (
	TypeString   FieldType = "string"
	TypeInt      FieldType = "int"
	TypeBool     FieldType = "bool"
	TypeFloat    FieldType = "float"
	TypeDuration FieldType = "duration" // Go syntax: 500ms, 30s, 5m, 1h30m
)

var validTypes = []FieldType{TypeString, TypeInt, TypeBool, TypeFloat, TypeDuration}

// SchemaField declares one configuration variable.
type SchemaField struct {
	Env         string    // environment variable name
	Type        FieldType // defaults to string
	Required    bool      // must be set (a Default counts as set, but is not allowed on required fields)
	Default     string    // used when the variable is not set; "" means no default
	Sensitive   bool      // never printed or stored in plain text
	Description string    // documentation only
}

// Schema is a language-independent description of the configuration a program
// expects. It is the same information the struct tags carry for Go programs,
// written down in a file so that programs in any language can be checked by the
// same engine.
type Schema struct {
	Version int
	Fields  []SchemaField
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LoadSchema reads and validates a schema file. Files ending in .json are
// parsed as JSON; everything else is parsed as YAML (a small, documented
// subset — see the README).
func LoadSchema(path string) (*Schema, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("confobs: reading schema: %w", err)
	}

	var s *Schema
	if strings.EqualFold(filepath.Ext(path), ".json") {
		s, err = parseSchemaJSON(data, path)
	} else {
		s, err = parseSchemaYAML(data, path)
	}
	if err != nil {
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("confobs: schema %s: %w", path, err)
	}
	return s, nil
}

// validate lints the schema itself. Catching a mistake in the schema at load
// time matters for the same reason the rest of the tool exists: a wrong schema
// would silently validate the wrong thing.
func (s *Schema) validate() error {
	if len(s.Fields) == 0 {
		return errors.New(`no fields declared (expected a "fields:" list)`)
	}
	seen := make(map[string]bool, len(s.Fields))
	for i := range s.Fields {
		f := &s.Fields[i]
		if f.Env == "" {
			return fmt.Errorf(`field #%d: missing "env" name`, i+1)
		}
		if !envNameRe.MatchString(f.Env) {
			return fmt.Errorf("field %q: not a valid environment variable name", f.Env)
		}
		if seen[f.Env] {
			return fmt.Errorf("field %q is declared more than once", f.Env)
		}
		seen[f.Env] = true

		if f.Type == "" {
			f.Type = TypeString
		}
		if !isValidType(f.Type) {
			return fmt.Errorf("field %q: unknown type %q (valid types: %s)", f.Env, f.Type, typeList())
		}
		if f.Required && f.Default != "" {
			return fmt.Errorf("field %q: a required field cannot also have a default (the default would always satisfy it)", f.Env)
		}
		if f.Default != "" {
			if err := checkType(f.Type, f.Default); err != nil {
				return fmt.Errorf("field %q: default %q is not a valid %s", f.Env, f.Default, f.Type)
			}
		}
	}
	return nil
}

func isValidType(t FieldType) bool {
	for _, v := range validTypes {
		if t == v {
			return true
		}
	}
	return false
}

func typeList() string {
	names := make([]string, len(validTypes))
	for i, t := range validTypes {
		names[i] = string(t)
	}
	return strings.Join(names, ", ")
}

// ---- JSON front-end -------------------------------------------------------

type jsonSchema struct {
	Version int         `json:"version"`
	Fields  []jsonField `json:"fields"`
}

type jsonField struct {
	Env         string       `json:"env"`
	Type        string       `json:"type"`
	Required    bool         `json:"required"`
	Default     scalarString `json:"default"`
	Sensitive   bool         `json:"sensitive"`
	Description string       `json:"description"`
}

// scalarString lets a JSON schema write `"default": 8080` or `"default": true`
// without quotes, the way people naturally will.
type scalarString string

func (s *scalarString) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return err
	}
	switch t := v.(type) {
	case nil:
		*s = ""
	case string:
		*s = scalarString(t)
	case json.Number:
		*s = scalarString(t.String())
	case bool:
		*s = scalarString(strconv.FormatBool(t))
	default:
		return errors.New("default must be a string, number or boolean")
	}
	return nil
}

func parseSchemaJSON(data []byte, name string) (*Schema, error) {
	var js jsonSchema
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // a misspelled key in the schema is itself a config bug
	if err := dec.Decode(&js); err != nil {
		return nil, fmt.Errorf("confobs: schema %s: %w", name, err)
	}
	s := &Schema{Version: js.Version}
	for _, f := range js.Fields {
		s.Fields = append(s.Fields, SchemaField{
			Env:         f.Env,
			Type:        FieldType(f.Type),
			Required:    f.Required,
			Default:     string(f.Default),
			Sensitive:   f.Sensitive,
			Description: f.Description,
		})
	}
	return s, nil
}

// ---- type checking --------------------------------------------------------

// checkType reports whether raw can be parsed as t.
func checkType(t FieldType, raw string) error {
	var err error
	switch t {
	case TypeInt:
		_, err = strconv.ParseInt(raw, 10, 64)
	case TypeBool:
		_, err = strconv.ParseBool(raw)
	case TypeFloat:
		_, err = strconv.ParseFloat(raw, 64)
	case TypeDuration:
		_, err = time.ParseDuration(raw)
	case TypeString, "":
	default:
		err = fmt.Errorf("unknown type %q", t)
	}
	return err
}

// typeHint is what an error message says a value was expected to look like.
func typeHint(t FieldType) string {
	switch t {
	case TypeBool:
		return "bool (true or false)"
	case TypeDuration:
		return "duration (for example 500ms, 30s, 5m)"
	default:
		return string(t)
	}
}

// normalizeValue rewrites a valid value into a canonical form, so that
// "08080" and "8080", or "TRUE" and "true", are not reported as drift.
func normalizeValue(t FieldType, raw string) string {
	switch t {
	case TypeInt:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return strconv.FormatInt(n, 10)
		}
	case TypeBool:
		if b, err := strconv.ParseBool(raw); err == nil {
			return strconv.FormatBool(b)
		}
	case TypeFloat:
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return strconv.FormatFloat(f, 'g', -1, 64)
		}
	case TypeDuration:
		if d, err := time.ParseDuration(raw); err == nil {
			return d.String()
		}
	}
	return raw
}

// ---- the shared validation engine ----------------------------------------

// Resolved holds the effective value of every field that resolved cleanly.
type Resolved struct {
	Values  map[string]string // env name -> effective value
	Sources map[string]string // env name -> "env" or "default"
}

// InvalidValue describes a value that is set but is not of the declared type.
type InvalidValue struct {
	Env    string `json:"env"`
	Reason string `json:"reason"`
}

// Resolve checks env (a map of variable name to value) against the schema.
//
// This is the one validation engine in confobs: the struct-tag API (Load) and
// the command-line tool both call it, so a Go program and a Python program
// governed by equivalent rules get identical answers.
//
// Resolve never stops at the first problem; the returned Result lists every
// missing variable, likely typo and badly typed value at once, because
// fixing configuration one error per restart is exactly the loop this tool
// exists to shorten.
func (s *Schema) Resolve(env map[string]string) (*Resolved, *Result) {
	res := &Resolved{Values: map[string]string{}, Sources: map[string]string{}}
	result := &Result{}
	declared := s.declared()

	for _, f := range s.Fields {
		raw, present := env[f.Env]
		source := "env"
		if !present && f.Default != "" {
			raw, present, source = f.Default, true, "default"
		}

		if !present {
			if f.Required {
				match, dist := closestKey(f.Env, env, declared)
				if match != "" && dist <= typoThreshold(f.Env) {
					result.Typos = append(result.Typos, TypoSuggestion{Expected: f.Env, Found: match, Distance: dist})
				} else {
					result.Missing = append(result.Missing, f.Env)
				}
			}
			continue
		}

		if err := checkType(f.Type, raw); err != nil {
			reason := "expected " + typeHint(f.Type)
			if !f.Sensitive { // never echo a secret back, even inside an error
				reason += fmt.Sprintf(", got %q", raw)
			}
			result.Invalid = append(result.Invalid, InvalidValue{Env: f.Env, Reason: reason})
			continue
		}
		res.Values[f.Env] = raw
		res.Sources[f.Env] = source
	}
	return res, result
}

func (s *Schema) declared() map[string]bool {
	d := make(map[string]bool, len(s.Fields))
	for _, f := range s.Fields {
		d[f.Env] = true
	}
	return d
}

// Unused returns the keys (typically read from an env file) that the schema
// does not declare, in the order given.
func (s *Schema) Unused(keys []string) []string {
	declared := s.declared()
	var unused []string
	for _, k := range keys {
		if !declared[k] {
			unused = append(unused, k)
		}
	}
	return unused
}

// Redact returns a copy of values with every sensitive field masked, safe to
// print or log.
func (s *Schema) Redact(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	for _, f := range s.Fields {
		if _, ok := out[f.Env]; ok && f.Sensitive {
			out[f.Env] = sensitiveRedactedLabel
		}
	}
	return out
}

// ---- typo matching --------------------------------------------------------

// maxTypoDistance caps the edit distance at which a set variable is suggested
// as the intended spelling of a missing one.
const maxTypoDistance = 3

// typoThreshold scales the allowed distance with the length of the name, so a
// short name like PORT is not "corrected" to an unrelated one like PATH,
// while a long name like DATABASE_URL still tolerates a couple of slips.
func typoThreshold(name string) int {
	t := len(name) / 4
	if t < 1 {
		t = 1
	}
	if t > maxTypoDistance {
		t = maxTypoDistance
	}
	return t
}

// closestKey returns the key in env nearest to target by edit distance,
// ignoring case (so database_url matches DATABASE_URL) and ignoring any name
// the schema already declares (if DB_PORT is a real field, it is not a typo of
// DB_HOST). Ties resolve alphabetically so output is deterministic.
func closestKey(target string, env map[string]string, declared map[string]bool) (string, int) {
	keys := make([]string, 0, len(env))
	for k := range env {
		if !declared[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	best, bestDist := "", -1
	want := strings.ToUpper(target)
	for _, k := range keys {
		if d := levenshtein(want, strings.ToUpper(k)); bestDist == -1 || d < bestDist {
			best, bestDist = k, d
		}
	}
	return best, bestDist
}
