// Package confobs (config observability) gives Go config structs two things
// most projects hand-roll badly or skip entirely:
//
//  1. Typo-aware validation — if a required env var is missing but something
//     close to it is set, you get "did you mean X" instead of a blank string
//     bug three services downstream.
//  2. Drift logging — every time config is (re)loaded, it's diffed against
//     the last known snapshot so config changes show up as structured,
//     reviewable events instead of silent behavior shifts.
//
// There are two front-ends over one validation engine:
//
//   - Struct tags (this file): Load and Reload work on a tagged Go struct.
//   - Schema files (schema.go): LoadSchema reads a YAML/JSON schema, so the
//     same rules can be enforced for programs written in any language. The
//     confobs command-line tool (cmd/confobs) is built on this path.
//
// Both features share one reflection layer over a single tagged struct:
//
//	type Config struct {
//	    DatabaseURL string        `env:"DATABASE_URL,required" sensitive:"true"`
//	    Port        int           `env:"PORT" default:"8080"`
//	    Timeout     time.Duration `env:"TIMEOUT" default:"5s"`
//	}
package confobs

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// fieldInfo describes one config field derived from its struct tags.
type fieldInfo struct {
	Name      string    // Go struct field name
	EnvName   string    // env:"NAME"
	Type      FieldType // derived from the Go type of the field
	Required  bool      // env:"NAME,required"
	Default   string    // default:"..."
	Sensitive bool      // sensitive:"true" — value is redacted in diffs/snapshots
	Index     int       // field index within the struct, for reflection access
}

// parseTags walks cfg (must be a pointer to a struct) and extracts fieldInfo
// for every field carrying an `env` tag. Fields without an `env` tag are
// ignored, so a config struct can freely mix confobs-managed fields with
// fields set some other way.
func parseTags(cfg interface{}) ([]fieldInfo, error) {
	v := reflect.ValueOf(cfg)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return nil, fmt.Errorf("confobs: cfg must be a non-nil pointer to a struct, got %T", cfg)
	}
	elem := v.Elem()
	if elem.Kind() != reflect.Struct {
		return nil, fmt.Errorf("confobs: cfg must point to a struct, got pointer to %s", elem.Kind())
	}

	t := elem.Type()
	var fields []fieldInfo
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		envTag, ok := f.Tag.Lookup("env")
		if !ok || envTag == "" {
			continue
		}
		if !f.IsExported() {
			return nil, fmt.Errorf("confobs: field %s has an env tag but is unexported, so it cannot be set", f.Name)
		}
		ft, ok := fieldTypeOf(f.Type)
		if !ok {
			return nil, fmt.Errorf("confobs: field %s: unsupported type %s (supported: string, int*, bool, float*, time.Duration)", f.Name, f.Type)
		}
		parts := strings.Split(envTag, ",")
		fi := fieldInfo{
			Name:    f.Name,
			EnvName: strings.TrimSpace(parts[0]),
			Type:    ft,
			Index:   i,
		}
		for _, opt := range parts[1:] {
			if strings.TrimSpace(opt) == "required" {
				fi.Required = true
			}
		}
		fi.Default = f.Tag.Get("default")
		fi.Sensitive = strings.EqualFold(f.Tag.Get("sensitive"), "true")
		fields = append(fields, fi)
	}
	return fields, nil
}

// fieldTypeOf maps a Go type onto the small set of types confobs understands.
func fieldTypeOf(t reflect.Type) (FieldType, bool) {
	if t == reflect.TypeOf(time.Duration(0)) {
		return TypeDuration, true
	}
	switch t.Kind() {
	case reflect.String:
		return TypeString, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return TypeInt, true
	case reflect.Bool:
		return TypeBool, true
	case reflect.Float32, reflect.Float64:
		return TypeFloat, true
	}
	return "", false
}

// setField converts raw (a string, as it would come from an env var) into
// the appropriate type and assigns it to the struct field described by fi.
func setField(cfgPtr reflect.Value, fi fieldInfo, raw string) error {
	fv := cfgPtr.Elem().Field(fi.Index)
	if !fv.CanSet() {
		return fmt.Errorf("confobs: field %s is not settable (is it unexported?)", fi.Name)
	}

	// time.Duration is an int64 under the hood, so it must be special-cased
	// ahead of the generic int handling below.
	if fv.Type() == reflect.TypeOf(time.Duration(0)) {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("confobs: field %s: invalid duration %q: %w", fi.Name, raw, err)
		}
		fv.SetInt(int64(d))
		return nil
	}

	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("confobs: field %s: invalid integer %q: %w", fi.Name, raw, err)
		}
		if fv.OverflowInt(n) {
			return fmt.Errorf("confobs: field %s: value %d does not fit in %s", fi.Name, n, fv.Type())
		}
		fv.SetInt(n)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("confobs: field %s: invalid bool %q: %w", fi.Name, raw, err)
		}
		fv.SetBool(b)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("confobs: field %s: invalid float %q: %w", fi.Name, raw, err)
		}
		if fv.OverflowFloat(f) {
			return fmt.Errorf("confobs: field %s: value %v does not fit in %s", fi.Name, f, fv.Type())
		}
		fv.SetFloat(f)
	default:
		return fmt.Errorf("confobs: field %s: unsupported type %s (supported: string, int*, bool, float*, time.Duration)", fi.Name, fv.Kind())
	}
	return nil
}

// rawFieldString reads the current value of the struct field back out as a
// string, with no redaction applied. It's used internally to compute a
// hash fingerprint for sensitive fields — never written to disk directly.
func rawFieldString(cfgPtr reflect.Value, fi fieldInfo) string {
	fv := cfgPtr.Elem().Field(fi.Index)
	if fv.Type() == reflect.TypeOf(time.Duration(0)) {
		return time.Duration(fv.Int()).String()
	}
	switch fv.Kind() {
	case reflect.String:
		return fv.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(fv.Int(), 10)
	case reflect.Bool:
		return strconv.FormatBool(fv.Bool())
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(fv.Float(), 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", fv.Interface())
	}
}

// levenshtein computes the classic edit distance between two strings.
// Used to power "did you mean" suggestions in envcheck.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}

	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
