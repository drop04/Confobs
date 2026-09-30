// Command example demonstrates confobs end-to-end: loading a config struct
// from the environment with typo-aware validation, then reloading it to see
// drift reported against the previous snapshot.
//
// Run it a few times with different env vars to see the behavior change:
//
//	go run ./example
//	PORT=9090 go run ./example
//	DATBASE_URL=oops go run ./example   # note the typo
package main

import (
	"fmt"
	"os"
	"time"

	"confobs"
)

type Config struct {
	DatabaseURL string        `env:"DATABASE_URL,required" sensitive:"true"`
	Port        int           `env:"PORT" default:"8080"`
	Debug       bool          `env:"DEBUG" default:"false"`
	Timeout     time.Duration `env:"TIMEOUT" default:"5s"`
}

func main() {
	// Ensure there's something to load on a first run, so the example is
	// runnable out of the box without extra setup.
	if os.Getenv("DATABASE_URL") == "" && os.Getenv("DATBASE_URL") == "" {
		os.Setenv("DATABASE_URL", "postgres://localhost/app")
	}

	var cfg Config
	result, err := confobs.Load(&cfg)
	if err != nil {
		fmt.Println("config problems found:")
		for _, m := range result.Missing {
			fmt.Printf("  - missing required: %s\n", m)
		}
		for _, t := range result.Typos {
			fmt.Printf("  - %s\n", t)
		}
		os.Exit(1)
	}
	fmt.Printf("loaded config: Port=%d Debug=%v Timeout=%v\n", cfg.Port, cfg.Debug, cfg.Timeout)

	snapPath := "./snapshot.json"
	drift, _, err := confobs.Reload(&cfg, snapPath)
	if err != nil {
		fmt.Println("reload error:", err)
		os.Exit(1)
	}
	switch {
	case drift.FirstLoad:
		fmt.Println("first run — baseline snapshot written to", snapPath)
	case drift.Changed():
		fmt.Println("config drift detected:")
		for _, c := range drift.Changes {
			fmt.Printf("  - %s: %q -> %q\n", c.Field, c.Old, c.New)
		}
	default:
		fmt.Println("no drift since last run")
	}
}
