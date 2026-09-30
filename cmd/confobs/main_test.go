package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testSchema = `
fields:
  - env: CT_DATABASE_URL
    required: true
    sensitive: true
  - env: CT_PORT
    type: int
    default: "8080"
  - env: CT_LOG_LEVEL
    default: info
`

// chdir changes the working directory for the duration of the test and
// restores it afterward. Equivalent to testing.T.Chdir (Go 1.24+), written
// out by hand so the test suite still runs on Go 1.22/1.23.
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Fatal(err)
		}
	})
}

// cli runs the whole tool in-process and returns its exit code and output.
func cli(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = execute(args, &out, &errb)
	return code, out.String(), errb.String()
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// unset removes a variable for the duration of the test (t.Setenv can only set).
func unset(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		old, had := os.LookupEnv(k)
		os.Unsetenv(k)
		if had {
			t.Cleanup(func() { os.Setenv(k, old) })
		}
	}
}

func setup(t *testing.T) (schema string, dir string) {
	t.Helper()
	unset(t, "CT_DATABASE_URL", "CT_PORT", "CT_LOG_LEVEL", "CT_DATBASE_URL")
	dir = t.TempDir()
	return write(t, dir, "schema.yaml", testSchema), dir
}

func TestCheck_OKShowsSourcesAndNeverTheSecret(t *testing.T) {
	schema, _ := setup(t)
	t.Setenv("CT_DATABASE_URL", "postgres://user:hunter2@db/x")

	code, out, _ := cli(t, "check", "--schema", schema)
	if code != exitOK {
		t.Fatalf("exit = %d, output:\n%s", code, out)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("secret printed:\n%s", out)
	}
	for _, want := range []string{"OK", "3 fields", "***REDACTED***", "default"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCheck_TypoMissingAndInvalidAllReportedAtOnce(t *testing.T) {
	schema, dir := setup(t)
	env := write(t, dir, ".env", "CT_DATBASE_URL=x\nCT_PORT=eighty\nSTALE=1\n")

	code, out, _ := cli(t, "check", "--schema", schema, "--env-file", env)
	if code != exitInvalid {
		t.Fatalf("exit = %d, want %d", code, exitInvalid)
	}
	for _, want := range []string{
		"2 problem(s)",
		"Did you mean CT_DATABASE_URL?",
		`expected int, got "eighty"`,
		"STALE", // unused warning
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "warning  CT_DATBASE_URL") {
		t.Errorf("a typo must not also be reported as an undeclared key:\n%s", out)
	}
}

func TestCheck_JSON(t *testing.T) {
	schema, dir := setup(t)
	t.Setenv("CT_DATABASE_URL", "postgres://user:hunter2@db/x")
	env := write(t, dir, ".env", "STALE=1\n")

	code, out, _ := cli(t, "check", "--schema", schema, "--env-file", env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("secret in JSON:\n%s", out)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	for _, key := range []string{"missing", "typos", "invalid"} {
		if string(got[key]) != "[]" {
			t.Errorf("%s = %s, want [] (never null)", key, got[key])
		}
	}
	var vals map[string]string
	json.Unmarshal(got["values"], &vals)
	if vals["CT_DATABASE_URL"] != "***REDACTED***" || vals["CT_PORT"] != "8080" {
		t.Errorf("values = %v", vals)
	}
}

func TestCheck_EnvFilePrecedence(t *testing.T) {
	schema, dir := setup(t)
	t.Setenv("CT_DATABASE_URL", "x")
	t.Setenv("CT_PORT", "1111")
	env := write(t, dir, ".env", "CT_PORT=2222\n")

	port := func(extra ...string) string {
		args := append([]string{"check", "--schema", schema, "--env-file", env, "--json"}, extra...)
		_, out, _ := cli(t, args...)
		var r struct{ Values map[string]string }
		json.Unmarshal([]byte(out), &r)
		return r.Values["CT_PORT"]
	}
	if got := port(); got != "1111" {
		t.Errorf("default precedence: real environment should win, got %s", got)
	}
	if got := port("--override"); got != "2222" {
		t.Errorf("--override: env file should win, got %s", got)
	}
}

func TestCheck_ErrorsAreUsageErrors(t *testing.T) {
	setup(t)
	if code, _, errOut := cli(t, "check", "--schema", "/no/such/schema.yaml"); code != exitUsage || !strings.Contains(errOut, "reading schema") {
		t.Errorf("missing schema: code %d, stderr %q", code, errOut)
	}
	if code, _, _ := cli(t, "check", "--bogus-flag"); code != exitUsage {
		t.Errorf("bad flag: code %d", code)
	}
	dir := t.TempDir()
	bad := write(t, dir, "bad.yaml", "feilds:\n")
	if code, _, errOut := cli(t, "check", "--schema", bad); code != exitUsage || !strings.Contains(errOut, `did you mean "fields"`) {
		t.Errorf("bad schema: code %d, stderr %q", code, errOut)
	}
	chdir(t, dir) // nothing named confobs.yaml here
	if code, _, errOut := cli(t, "check"); code != exitUsage || !strings.Contains(errOut, "no schema given") {
		t.Errorf("no schema anywhere: code %d, stderr %q", code, errOut)
	}
}

func TestCheck_FindsConventionalSchemaName(t *testing.T) {
	_, dir := setup(t)
	write(t, dir, "confobs.yaml", testSchema)
	chdir(t, dir)
	t.Setenv("CT_DATABASE_URL", "x")
	if code, out, _ := cli(t, "check"); code != exitOK {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestDiff_BaselineThenDriftThenNoUpdate(t *testing.T) {
	schema, dir := setup(t)
	snap := filepath.Join(dir, "snap.json")
	t.Setenv("CT_DATABASE_URL", "postgres://user:hunter2@db/x")

	code, out, _ := cli(t, "diff", "--schema", schema, "--snapshot", snap)
	if code != exitOK || !strings.Contains(out, "baseline written") {
		t.Fatalf("first diff: code %d\n%s", code, out)
	}
	raw, _ := os.ReadFile(snap)
	if strings.Contains(string(raw), "hunter2") {
		t.Errorf("snapshot holds the plaintext secret:\n%s", raw)
	}

	if code, out, _ = cli(t, "diff", "--schema", schema, "--snapshot", snap); code != exitOK || !strings.Contains(out, "no drift") {
		t.Errorf("second diff: code %d\n%s", code, out)
	}

	t.Setenv("CT_LOG_LEVEL", "debug")
	t.Setenv("CT_DATABASE_URL", "postgres://user:rotated@db/x")

	code, out, _ = cli(t, "diff", "--schema", schema, "--snapshot", snap, "--no-update")
	if code != exitOK {
		t.Errorf("drift alone must not fail without --exit-code, got %d", code)
	}
	for _, want := range []string{"2 change(s)", "CT_LOG_LEVEL", "info", "debug", "CT_DATABASE_URL", "***REDACTED***"} {
		if !strings.Contains(out, want) {
			t.Errorf("drift output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hunter2") || strings.Contains(out, "rotated") {
		t.Errorf("secret leaked in drift output:\n%s", out)
	}

	// --no-update kept the baseline, so the same drift is still there, and with
	// --exit-code it is a failing status.
	code, _, _ = cli(t, "diff", "--schema", schema, "--snapshot", snap, "--exit-code")
	if code != exitDrift {
		t.Errorf("--exit-code: got %d, want %d", code, exitDrift)
	}
	// ...and that run (which updated the baseline) means the next one is quiet.
	if code, out, _ = cli(t, "diff", "--schema", schema, "--snapshot", snap, "--exit-code"); code != exitOK {
		t.Errorf("after update: code %d\n%s", code, out)
	}
}

func TestDiff_InvalidConfigNeverBecomesTheBaseline(t *testing.T) {
	schema, dir := setup(t)
	snap := filepath.Join(dir, "snap.json")
	t.Setenv("CT_DATABASE_URL", "good")
	cli(t, "diff", "--schema", schema, "--snapshot", snap)
	before, _ := os.ReadFile(snap)

	unset(t, "CT_DATABASE_URL")
	code, out, _ := cli(t, "diff", "--schema", schema, "--snapshot", snap)
	if code != exitInvalid || !strings.Contains(out, "FAILED") {
		t.Errorf("invalid config: code %d\n%s", code, out)
	}
	after, _ := os.ReadFile(snap)
	if !bytes.Equal(before, after) {
		t.Errorf("a broken configuration overwrote the last known-good baseline")
	}
}

func TestDiff_JSON(t *testing.T) {
	schema, dir := setup(t)
	snap := filepath.Join(dir, "snap.json")
	t.Setenv("CT_DATABASE_URL", "x")
	cli(t, "diff", "--schema", schema, "--snapshot", snap)
	t.Setenv("CT_PORT", "9090")

	code, out, _ := cli(t, "diff", "--schema", schema, "--snapshot", snap, "--json")
	if code != exitOK {
		t.Fatalf("code %d", code)
	}
	var r struct {
		OK              bool `json:"ok"`
		Changed         bool `json:"changed"`
		SnapshotUpdated bool `json:"snapshot_updated"`
		Changes         []struct{ Field, Old, New string }
		Values          map[string]string
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !r.OK || !r.Changed || !r.SnapshotUpdated || len(r.Changes) != 1 {
		t.Fatalf("report = %+v", r)
	}
	if c := r.Changes[0]; c.Field != "CT_PORT" || c.Old != "8080" || c.New != "9090" {
		t.Errorf("change = %+v", c)
	}
	if r.Values["CT_PORT"] != "9090" {
		t.Errorf("values should carry the new effective config: %v", r.Values)
	}
}

func skipOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
}

func TestRun_StartsChildWithDefaultsInjected(t *testing.T) {
	skipOnWindows(t)
	schema, _ := setup(t)
	t.Setenv("CT_DATABASE_URL", "x")

	code, out, errOut := cli(t, "run", "--schema", schema, "--", "sh", "-c", `echo "port=$CT_PORT level=$CT_LOG_LEVEL"`)
	if code != exitOK {
		t.Fatalf("exit %d\nstderr: %s", code, errOut)
	}
	if strings.TrimSpace(out) != "port=8080 level=info" {
		t.Errorf("child saw %q; schema defaults must be injected", out)
	}
	if !strings.Contains(errOut, "config OK") {
		t.Errorf("expected the banner on stderr, got %q", errOut)
	}
	// stdout belongs to the child alone.
	if strings.Contains(out, "confobs") {
		t.Errorf("confobs wrote to the child's stdout: %q", out)
	}
}

func TestRun_QuietSuppressesBanner(t *testing.T) {
	skipOnWindows(t)
	schema, _ := setup(t)
	t.Setenv("CT_DATABASE_URL", "x")
	_, _, errOut := cli(t, "run", "--quiet", "--schema", schema, "--", "true")
	if errOut != "" {
		t.Errorf("--quiet still printed: %q", errOut)
	}
}

func TestRun_RefusesToStartOnBadConfig(t *testing.T) {
	skipOnWindows(t)
	schema, dir := setup(t)
	marker := filepath.Join(dir, "started")
	env := write(t, dir, ".env", "CT_DATBASE_URL=x\n")

	code, _, errOut := cli(t, "run", "--schema", schema, "--env-file", env, "--", "sh", "-c", "touch "+marker)
	if code != exitInvalid {
		t.Errorf("exit = %d, want %d", code, exitInvalid)
	}
	if !strings.Contains(errOut, "Did you mean CT_DATABASE_URL?") || !strings.Contains(errOut, "not starting") {
		t.Errorf("stderr:\n%s", errOut)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the program was started despite invalid configuration")
	}
}

func TestRun_PassesEnvFileValuesToChild(t *testing.T) {
	skipOnWindows(t)
	schema, dir := setup(t)
	unset(t, "CT_EXTRA")
	env := write(t, dir, ".env", "CT_DATABASE_URL=fromfile\nCT_EXTRA=also-from-file\n")

	code, out, errOut := cli(t, "run", "--schema", schema, "--env-file", env, "--", "sh", "-c", `echo "$CT_DATABASE_URL $CT_EXTRA"`)
	if code != exitOK || strings.TrimSpace(out) != "fromfile also-from-file" {
		t.Errorf("code %d out %q err %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "CT_EXTRA") {
		t.Errorf("undeclared env-file key should be warned about:\n%s", errOut)
	}
}

func TestRun_PropagatesExitCodeAndReportsDrift(t *testing.T) {
	skipOnWindows(t)
	schema, dir := setup(t)
	snap := filepath.Join(dir, "snap.json")
	t.Setenv("CT_DATABASE_URL", "x")

	if code, _, _ := cli(t, "run", "--schema", schema, "--snapshot", snap, "--", "sh", "-c", "exit 7"); code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	t.Setenv("CT_PORT", "9999")
	_, _, errOut := cli(t, "run", "--schema", schema, "--snapshot", snap, "--", "true")
	if !strings.Contains(errOut, "drift since the last run") || !strings.Contains(errOut, "CT_PORT") {
		t.Errorf("drift not reported on start:\n%s", errOut)
	}
}

func TestRun_KilledChildReports128PlusSignal(t *testing.T) {
	skipOnWindows(t)
	schema, _ := setup(t)
	t.Setenv("CT_DATABASE_URL", "x")
	if code, _, _ := cli(t, "run", "--quiet", "--schema", schema, "--", "sh", "-c", "kill -TERM $$"); code != 128+15 {
		t.Errorf("exit code = %d, want 143", code)
	}
}

func TestRun_UsageAndStartErrors(t *testing.T) {
	schema, _ := setup(t)
	t.Setenv("CT_DATABASE_URL", "x")
	if code, _, errOut := cli(t, "run", "--schema", schema); code != exitUsage || !strings.Contains(errOut, "needs a command") {
		t.Errorf("no command: code %d %q", code, errOut)
	}
	if code, _, errOut := cli(t, "run", "--quiet", "--schema", schema, "--", "/no/such/binary"); code != 127 || !strings.Contains(errOut, "cannot start") {
		t.Errorf("missing binary: code %d %q", code, errOut)
	}
}

func TestDispatch(t *testing.T) {
	if code, out, _ := cli(t, "version"); code != exitOK || !strings.Contains(out, "confobs") {
		t.Errorf("version: %d %q", code, out)
	}
	if code, out, _ := cli(t, "help"); code != exitOK || !strings.Contains(out, "Usage:") {
		t.Errorf("help: %d", code)
	}
	if code, _, errOut := cli(t, "frobnicate"); code != exitUsage || !strings.Contains(errOut, "unknown command") {
		t.Errorf("unknown: %d %q", code, errOut)
	}
	if code, _, _ := cli(t); code != exitUsage {
		t.Errorf("no args: %d", code)
	}
}
