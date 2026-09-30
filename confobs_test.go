package confobs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testConfig struct {
	DatabaseURL string        `env:"DATABASE_URL,required" sensitive:"true"`
	Port        int           `env:"PORT" default:"8080"`
	Debug       bool          `env:"DEBUG" default:"false"`
	Timeout     time.Duration `env:"TIMEOUT" default:"5s"`
	Ignored     string        // no env tag: confobs should leave this alone
}

func clearEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		os.Unsetenv(k)
	}
}

func TestLoad_Basics(t *testing.T) {
	clearEnv(t, "DATABASE_URL", "PORT", "DEBUG", "TIMEOUT")
	os.Setenv("DATABASE_URL", "postgres://localhost/app")
	os.Setenv("PORT", "9090")
	defer clearEnv(t, "DATABASE_URL", "PORT")

	var cfg testConfig
	result, err := Load(&cfg)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if result.HasProblems() {
		t.Fatalf("unexpected problems: %+v", result)
	}
	if cfg.DatabaseURL != "postgres://localhost/app" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	// Defaults should apply for unset fields.
	if cfg.Debug != false {
		t.Errorf("Debug = %v, want false (default)", cfg.Debug)
	}
	if cfg.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s (default)", cfg.Timeout)
	}
}

func TestLoad_MissingRequired(t *testing.T) {
	clearEnv(t, "DATABASE_URL", "PORT", "DEBUG", "TIMEOUT")

	var cfg testConfig
	result, err := Load(&cfg)
	if err == nil {
		t.Fatal("expected error for missing required field, got nil")
	}
	if len(result.Missing) != 1 || result.Missing[0] != "DATABASE_URL" {
		t.Errorf("Missing = %v, want [DATABASE_URL]", result.Missing)
	}
}

func TestLoad_TypoSuggestion(t *testing.T) {
	clearEnv(t, "DATABASE_URL", "DATBASE_URL", "PORT")
	// Deliberately misspelled.
	os.Setenv("DATBASE_URL", "postgres://localhost/app")
	defer clearEnv(t, "DATBASE_URL")

	var cfg testConfig
	result, err := Load(&cfg)
	if err == nil {
		t.Fatal("expected error (unresolved required field), got nil")
	}
	if len(result.Typos) != 1 {
		t.Fatalf("Typos = %+v, want exactly 1 suggestion", result.Typos)
	}
	got := result.Typos[0]
	if got.Expected != "DATABASE_URL" || got.Found != "DATBASE_URL" {
		t.Errorf("typo suggestion = %+v", got)
	}
	if len(result.Missing) != 0 {
		t.Errorf("Missing = %v, want empty (typo should preempt hard-missing)", result.Missing)
	}
}

func TestLoad_InvalidType(t *testing.T) {
	clearEnv(t, "DATABASE_URL", "PORT")
	os.Setenv("DATABASE_URL", "x")
	os.Setenv("PORT", "not-a-number")
	defer clearEnv(t, "DATABASE_URL", "PORT")

	var cfg testConfig
	_, err := Load(&cfg)
	if err == nil {
		t.Fatal("expected type coercion error for PORT, got nil")
	}
}

func TestCheckUnused(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	content := "DATABASE_URL=x\nPORT=8080\nOLD_FEATURE_FLAG=true\n# a comment\n\n"
	if err := os.WriteFile(envFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	var cfg testConfig
	unused, err := CheckUnused(&cfg, envFile)
	if err != nil {
		t.Fatalf("CheckUnused error: %v", err)
	}
	if len(unused) != 1 || unused[0] != "OLD_FEATURE_FLAG" {
		t.Errorf("unused = %v, want [OLD_FEATURE_FLAG]", unused)
	}
}

func TestReload_FirstLoadThenDrift(t *testing.T) {
	dir := t.TempDir()
	snapPath := filepath.Join(dir, "snapshot.json")

	clearEnv(t, "DATABASE_URL", "PORT")
	os.Setenv("DATABASE_URL", "postgres://localhost/app")
	os.Setenv("PORT", "8080")
	defer clearEnv(t, "DATABASE_URL", "PORT")

	var cfg testConfig
	drift, loadResult, err := Reload(&cfg, snapPath)
	if err != nil {
		t.Fatalf("first Reload error: %v", err)
	}
	if loadResult.HasProblems() {
		t.Fatalf("unexpected load problems: %+v", loadResult)
	}
	if !drift.FirstLoad {
		t.Error("expected FirstLoad=true on first Reload")
	}
	if drift.Changed() {
		t.Errorf("expected no changes on first load, got %+v", drift.Changes)
	}

	// Change PORT and reload again — should now report a drift.
	os.Setenv("PORT", "9090")
	var cfg2 testConfig
	drift2, _, err := Reload(&cfg2, snapPath)
	if err != nil {
		t.Fatalf("second Reload error: %v", err)
	}
	if drift2.FirstLoad {
		t.Error("expected FirstLoad=false on second Reload")
	}
	if !drift2.Changed() {
		t.Fatal("expected a change after PORT was updated")
	}
	var found bool
	for _, c := range drift2.Changes {
		if c.Field == "Port" {
			found = true
			if c.Old != "8080" || c.New != "9090" {
				t.Errorf("Port change = %+v, want Old=8080 New=9090", c)
			}
		}
	}
	if !found {
		t.Errorf("no change reported for Port field: %+v", drift2.Changes)
	}
}

func TestReload_SensitiveFieldRedacted(t *testing.T) {
	dir := t.TempDir()
	snapPath := filepath.Join(dir, "snapshot.json")

	clearEnv(t, "DATABASE_URL", "PORT")
	os.Setenv("DATABASE_URL", "postgres://localhost/app-v1")
	os.Setenv("PORT", "8080")
	defer clearEnv(t, "DATABASE_URL", "PORT")

	var cfg testConfig
	if _, _, err := Reload(&cfg, snapPath); err != nil {
		t.Fatalf("first Reload error: %v", err)
	}

	// Confirm the on-disk snapshot never stores the real secret.
	raw, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(raw), "app-v1") {
		t.Error("snapshot file contains unredacted sensitive value")
	}

	os.Setenv("DATABASE_URL", "postgres://localhost/app-v2")
	var cfg2 testConfig
	drift, _, err := Reload(&cfg2, snapPath)
	if err != nil {
		t.Fatalf("second Reload error: %v", err)
	}
	if !drift.Changed() {
		t.Fatal("expected DatabaseURL change to be detected even though redacted")
	}
	for _, c := range drift.Changes {
		if c.Field == "DatabaseURL" {
			if c.New != "***REDACTED***" {
				t.Errorf("sensitive change value not redacted: %+v", c)
			}
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
