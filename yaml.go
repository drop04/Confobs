package confobs

import (
	"fmt"
	"strconv"
	"strings"
)

// The schema format is a deliberately small subset of YAML so that confobs can
// stay dependency-free (one static binary, standard library only):
//
//	version: 1                      # optional
//	fields:
//	  - env: DATABASE_URL           # required: the variable name
//	    type: string                # string (default) | int | bool | float | duration
//	    required: true              # default false
//	    sensitive: true             # default false
//	    default: "8080"             # used when the variable is unset
//	    description: What it is for # optional, documentation only
//
// Supported: comments, block lists of maps, plain / 'single' / "double" quoted
// scalars. Not supported (and rejected with a clear message rather than
// misread): tabs, flow style ({...} and [...]), block scalars (| and >),
// anchors, and multi-document files.

var yamlFieldKeys = []string{"env", "type", "required", "sensitive", "default", "description"}

func yamlErr(name string, line int, format string, args ...interface{}) error {
	return fmt.Errorf("confobs: schema %s:%d: %s", name, line, fmt.Sprintf(format, args...))
}

func parseSchemaYAML(data []byte, name string) (*Schema, error) {
	s := &Schema{}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")

	inFields := false
	itemIndent := -1    // column of the "-" that starts each field
	contentIndent := -1 // column where a field's keys start
	var cur *SchemaField

	for i, rawLine := range strings.Split(text, "\n") {
		lineNo := i + 1
		line := stripYAMLComment(rawLine)
		if strings.TrimSpace(line) == "" {
			continue
		}

		indentStr := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if strings.Contains(indentStr, "\t") {
			return nil, yamlErr(name, lineNo, "tabs are not allowed for indentation; use spaces")
		}
		indent := len(indentStr)
		body := strings.TrimSpace(line)
		isItem := body == "-" || strings.HasPrefix(body, "- ")

		// Top-level keys (version, fields).
		if indent == 0 && !isItem {
			key, val, ok := splitYAMLKeyValue(body)
			if !ok {
				return nil, yamlErr(name, lineNo, `expected "key: value", got %q`, body)
			}
			inFields, cur, itemIndent, contentIndent = false, nil, -1, -1
			switch key {
			case "version":
				n, err := strconv.Atoi(unquoteYAML(val))
				if err != nil {
					return nil, yamlErr(name, lineNo, "version must be an integer, got %q", val)
				}
				s.Version = n
			case "fields":
				if val != "" {
					return nil, yamlErr(name, lineNo, `"fields" must be a block list: put each "- env: ..." item on its own line below it`)
				}
				inFields = true
			default:
				return nil, yamlErr(name, lineNo, "unknown top-level key %q%s", key, didYouMean(key, []string{"version", "fields"}))
			}
			continue
		}

		if !inFields {
			return nil, yamlErr(name, lineNo, `unexpected content; field definitions belong under a "fields:" line`)
		}

		if isItem {
			if itemIndent == -1 {
				itemIndent = indent
			} else if indent != itemIndent {
				return nil, yamlErr(name, lineNo, "list items must all be indented the same amount (expected %d spaces, found %d)", itemIndent, indent)
			}
			s.Fields = append(s.Fields, SchemaField{})
			cur = &s.Fields[len(s.Fields)-1]

			rest := strings.TrimSpace(strings.TrimPrefix(body, "-"))
			if rest == "" { // keys start on the following lines
				contentIndent = -1
				continue
			}
			contentIndent = indent + (len(body) - len(rest))
			body = rest
		} else {
			if cur == nil {
				return nil, yamlErr(name, lineNo, `expected a "- " list item to start a field`)
			}
			if contentIndent == -1 {
				contentIndent = indent
			} else if indent != contentIndent {
				return nil, yamlErr(name, lineNo, "misaligned key: expected %d spaces of indentation, found %d", contentIndent, indent)
			}
		}

		key, val, ok := splitYAMLKeyValue(body)
		if !ok {
			return nil, yamlErr(name, lineNo, `expected "key: value", got %q`, body)
		}
		if err := assignYAMLField(cur, key, val); err != nil {
			return nil, yamlErr(name, lineNo, "%v", err)
		}
	}
	return s, nil
}

func assignYAMLField(f *SchemaField, key, rawVal string) error {
	rawVal = strings.TrimSpace(rawVal)
	if rawVal != "" {
		switch rawVal[0] {
		case '{', '[':
			return fmt.Errorf("flow-style YAML (%c...) is not supported; write it in block style or quote the value", rawVal[0])
		case '|', '>':
			return fmt.Errorf("multi-line block scalars (%c) are not supported; keep the value on one line", rawVal[0])
		}
	}
	val := unquoteYAML(rawVal)

	switch key {
	case "env":
		f.Env = val
	case "type":
		f.Type = FieldType(val)
	case "required":
		b, err := parseYAMLBool(key, val)
		if err != nil {
			return err
		}
		f.Required = b
	case "sensitive":
		b, err := parseYAMLBool(key, val)
		if err != nil {
			return err
		}
		f.Sensitive = b
	case "default":
		f.Default = val
	case "description":
		f.Description = val
	default:
		return fmt.Errorf("unknown field key %q%s (valid keys: %s)", key, didYouMean(key, yamlFieldKeys), strings.Join(yamlFieldKeys, ", "))
	}
	return nil
}

func parseYAMLBool(key, v string) (bool, error) {
	switch strings.ToLower(v) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("%q must be true or false, got %q", key, v)
}

// splitYAMLKeyValue splits "key: value" at the first colon that is followed by
// a space or the end of the line, so values such as URLs may contain colons.
func splitYAMLKeyValue(body string) (key, val string, ok bool) {
	for i := 0; i < len(body); i++ {
		if body[i] == ':' && (i+1 == len(body) || body[i+1] == ' ') {
			key = strings.TrimSpace(body[:i])
			if key == "" {
				return "", "", false
			}
			return key, strings.TrimSpace(body[i+1:]), true
		}
	}
	return "", "", false
}

// stripYAMLComment removes a trailing "# comment", ignoring "#" inside quotes
// or glued to a word (as in a URL fragment).
func stripYAMLComment(line string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		atTokenStart := i == 0 || line[i-1] == ' '
		switch {
		case c == '\\' && inDouble:
			i++
		case c == '"' && !inSingle:
			if inDouble {
				inDouble = false
			} else if atTokenStart {
				inDouble = true
			}
		case c == '\'' && !inDouble:
			if inSingle {
				inSingle = false
			} else if atTokenStart { // so an apostrophe in "don't" is not a quote
				inSingle = true
			}
		case c == '#' && !inSingle && !inDouble && atTokenStart:
			return line[:i]
		}
	}
	return line
}

func unquoteYAML(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		if u, err := strconv.Unquote(v); err == nil {
			return u
		}
		return v[1 : len(v)-1]
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	return v
}

// didYouMean returns " (did you mean "x"?)" when word is within two edits of
// one of options — the schema file gets the same typo help as everything else.
func didYouMean(word string, options []string) string {
	best, bestDist := "", 3
	for _, o := range options {
		if d := levenshtein(strings.ToLower(word), o); d < bestDist {
			best, bestDist = o, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" (did you mean %q?)", best)
}
