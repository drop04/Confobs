// Command confobs validates and observes the configuration of programs written
// in any language, using the same engine as the confobs Go library.
//
//	confobs check  --schema schema.yaml [--env-file .env]
//	confobs diff   --schema schema.yaml [--env-file .env] --snapshot snap.json
//	confobs run    --schema schema.yaml [--env-file .env] -- python3 app.py
//
// The program being checked never has to know confobs exists: `run` validates
// the environment, injects schema defaults, and only then starts it.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"confobs"
)

const version = "0.1.0"

// Exit codes. `run` additionally returns the child's own exit code.
const (
	exitOK      = 0
	exitInvalid = 1 // configuration failed validation
	exitUsage   = 2 // bad flags, unreadable schema/env file, I/O failure
	exitDrift   = 3 // `diff --exit-code` found changes
)

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

// execute is main with its inputs and outputs made explicit, so tests can drive
// the whole CLI in-process.
func execute(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "check":
		return cmdCheck(args[1:], stdout, stderr)
	case "diff":
		return cmdDiff(args[1:], stdout, stderr)
	case "run":
		return cmdRun(args[1:], stdout, stderr)
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, "confobs", version)
		return exitOK
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "confobs: unknown command %q\n\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `confobs - check and observe configuration for programs in any language

Usage:
  confobs check [flags]                     validate the environment against a schema
  confobs diff  [flags]                     report what changed since the last snapshot
  confobs run   [flags] -- <cmd> [args...]  validate, then start <cmd> with the checked environment
  confobs version | help

Flags (all commands):
  --schema FILE     schema file, YAML or JSON (default: confobs.yaml, confobs.yml or confobs.json)
  --env-file FILE   also read KEY=VALUE pairs from FILE
  --override        let --env-file values win over real environment variables
                    (by default the real environment wins, as in Docker and Compose)

check, diff:
  --json            machine-readable output (secrets are always redacted)

diff:
  --snapshot FILE   baseline to compare against (default: .confobs-snapshot.json)
  --no-update       compare only; do not rewrite the baseline
  --exit-code       exit with status 3 when drift is found (for CI)

run:
  --snapshot FILE   also report drift since the last run, then update the baseline
  --quiet           suppress confobs's own messages

The baseline is only rewritten when the configuration is valid, so it always
holds the last known-good configuration.

Exit codes: 0 ok, 1 invalid configuration, 2 usage or I/O error, 3 drift (diff
--exit-code). For run, a started program's own exit code is passed through.
`)
}

// commonFlags are shared by every subcommand.
type commonFlags struct {
	schema   string
	envFile  string
	override bool
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.schema, "schema", "", "schema file (YAML or JSON)")
	fs.StringVar(&c.envFile, "env-file", "", "read KEY=VALUE pairs from this file")
	fs.BoolVar(&c.override, "override", false, "env-file values win over real environment variables")
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("confobs "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }
	return fs
}

// parseFlags returns (exit code, true) when the caller should stop.
func parseFlags(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK, true
		}
		return exitUsage, true
	}
	return 0, false
}

// loaded is everything a subcommand needs after reading the schema and the
// environment.
type loaded struct {
	schemaPath string
	schema     *confobs.Schema
	processEnv map[string]string // the real environment, untouched
	env        map[string]string // what validation saw: real environment + env file, per precedence
	envFile    *confobs.EnvFile  // nil when --env-file was not given
	resolved   *confobs.Resolved
	result     *confobs.Result
}

func load(c commonFlags) (*loaded, error) {
	path, err := findSchema(c.schema)
	if err != nil {
		return nil, err
	}
	schema, err := confobs.LoadSchema(path)
	if err != nil {
		return nil, err
	}

	l := &loaded{schemaPath: path, schema: schema, processEnv: environMap()}
	l.env = make(map[string]string, len(l.processEnv))
	for k, v := range l.processEnv {
		l.env[k] = v
	}

	if c.envFile != "" {
		ef, err := confobs.ParseEnvFile(c.envFile)
		if err != nil {
			return nil, err
		}
		l.envFile = ef
		for k, v := range ef.Values {
			if _, exists := l.env[k]; !exists || c.override {
				l.env[k] = v
			}
		}
	}

	l.resolved, l.result = schema.Resolve(l.env)
	if l.envFile != nil {
		l.result.Unused = withoutTypos(schema.Unused(l.envFile.Keys), l.result.Typos)
	}
	return l, nil
}

// withoutTypos drops keys that are already explained as a likely typo of a
// declared variable, so one mistake is not reported twice.
func withoutTypos(unused []string, typos []confobs.TypoSuggestion) []string {
	explained := map[string]bool{}
	for _, t := range typos {
		explained[t.Found] = true
	}
	var out []string
	for _, k := range unused {
		if !explained[k] {
			out = append(out, k)
		}
	}
	return out
}

// findSchema returns the --schema value, or the first conventional file name
// that exists in the current directory.
func findSchema(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	for _, name := range []string{"confobs.yaml", "confobs.yml", "confobs.json"} {
		if _, err := os.Stat(name); err == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("no schema given: pass --schema FILE or create confobs.yaml in the current directory")
}

func environMap() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			env[k] = v
		}
	}
	return env
}

func fail(stderr io.Writer, err error) int {
	msg := strings.TrimPrefix(err.Error(), "confobs: ")
	fmt.Fprintf(stderr, "confobs: %s\n", msg)
	return exitUsage
}
