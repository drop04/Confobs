# confobs

Config observability, for any language: typo-aware validation and drift
logging, built on one shared engine.

Most config-loading libraries validate that a required setting is *present*.
Fewer catch that you set `DATBASE_URL` instead of `DATABASE_URL` and are
silently running with an empty string. Almost none tell you *what changed*
the last time config was reloaded. `confobs` does both — usable natively
from Go, or from any language at all via a small command-line tool built on
exactly the same validation engine.

```go
type Config struct {
    DatabaseURL string        `env:"DATABASE_URL,required" sensitive:"true"`
    Port        int           `env:"PORT" default:"8080"`
    Timeout     time.Duration `env:"TIMEOUT" default:"5s"`
}
```

## Install

```
go get confobs
```

## Load: typo-aware validation

```go
var cfg Config
result, err := confobs.Load(&cfg)
if err != nil {
    for _, m := range result.Missing {
        log.Printf("missing required env var: %s", m)
    }
    for _, t := range result.Typos {
        log.Println(t) // env var "DATABASE_URL" is not set, but "DATBASE_URL" is — did you mean...
    }
    os.Exit(1)
}
```

`Load` fills `cfg` from `os.Environ()`, applying `default` tags where a var
is unset, and coercing strings into `string`, `int*`, `bool`, `float*`, and
`time.Duration` fields. If a `required` field has no value, `Load` first
checks whether some other currently-set env var is a near edit-distance
match (≤3) — catching the typo case — before reporting it as truly missing.

## CheckUnused: catch dead .env entries

```go
unused, _ := confobs.CheckUnused(&cfg, ".env")
// unused == ["OLD_FEATURE_FLAG"] — set in .env but not referenced by any tag
```

## Reload: drift logging

```go
drift, _, err := confobs.Reload(&cfg, "./snapshot.json")
switch {
case drift.FirstLoad:
    log.Println("baseline config snapshot written")
case drift.Changed():
    for _, c := range drift.Changes {
        log.Printf("config drift: %s changed from %q to %q", c.Field, c.Old, c.New)
    }
}
```

Every call to `Reload` re-reads the environment, diffs the result against
the last snapshot on disk, and writes an updated snapshot. Fields tagged
`sensitive:"true"` are never written to disk in plain text — they're stored
as a short SHA-256 fingerprint, so a changed secret is still detected as
drift without the value itself ever touching disk or your logs.

See `example/main.go` for a complete runnable walkthrough of the Go API.

## The confobs CLI — the same engine, for any language

The Go struct-tag API above and the `confobs` command-line tool are two
front-ends over **one validation engine** (`schema.go`): a Go program, a
Python program and a Node program governed by an equivalent schema get
identical answers to "is this configuration valid" and "what changed."

Instead of a Go struct, the CLI reads a small schema file:

```yaml
# schema.yaml
fields:
  - env: DATABASE_URL
    required: true
    sensitive: true
  - env: PORT
    type: int
    default: "8080"
```

```
confobs check --schema schema.yaml [--env-file .env]
confobs diff  --schema schema.yaml --snapshot snap.json
confobs run   --schema schema.yaml -- python3 app.py
```

- **`check`** validates the environment and prints every problem at once —
  missing required vars, likely typos, badly typed values — or `--json` for
  machine-readable output.
- **`diff`** compares the current configuration against a snapshot on disk
  and reports what changed; `--exit-code` makes it fail a CI step when drift
  is found.
- **`run`** is the zero-code integration: it validates the environment,
  injects schema defaults, and only *then* starts the given command with
  that resolved environment. If configuration is invalid, the program is
  never started at all. The program itself needs no confobs code —
  `python3 app.py` and `node server.js` work completely unmodified.

Build it with `go build -o bin/confobs ./cmd/confobs` (a single static
binary — no Python/Node/JVM runtime required to run the tool itself, only to
run the programs it's checking).

See `cmd/confobs/main.go` for full flag documentation (`confobs help`), and
`schema.go` / `yaml.go` for the schema format and its (dependency-free,
hand-rolled) YAML parser.

## Polyglot demo: one schema, two languages

`demo/` contains one shared `schema.yaml` and two small HTTP services that
implement it independently — an inventory service in **Python** and a
notifications service in **Node.js** — neither containing a single line of
confobs code:

```
cd demo/python-inventory
confobs run --schema ../schema.yaml --env-file .env -- python3 app.py

cd demo/node-notifications
confobs run --schema ../schema.yaml --env-file .env -- node server.js
```

Both are started, validated, and given their defaults by the exact same CLI
and schema. Misspell `DATABASE_URL` in either service's `.env` and `run`
refuses to start that service — Python or Node, same behavior, same message,
because both go through the same engine that validates the Go library and
the Go CLI itself. See `demo/README.md` for the full walkthrough, including
live drift detection with `confobs diff`.

## Design notes / scope

This is intentionally a small, focused tool, not a full config framework:

- No nested struct or slice diffing — flat fields only.
- One snapshot backend: a local JSON file. No pluggable KV store (yet).
- The schema YAML parser supports a deliberately small, documented subset of
  YAML (see the comment at the top of `yaml.go`) rather than full YAML.

## Showcase project

`cmd/server` is a small task-list HTTP API built specifically to demonstrate
confobs in a realistic setting — not just unit tests. It shows all three
behaviors live:

```
cp cmd/server/.env.example cmd/server/.env
go run ./cmd/server
```

- **Startup validation**: rename `DATABASE_URL` to `DATABSE_URL` in `.env`
  and re-run — the server refuses to start and tells you exactly which var
  it thinks you meant.
- **Unused-var detection**: `.env.example` ships with a deliberately stale
  `OLD_CACHE_TTL` entry; the server warns about it on startup.
- **Live config reload with drift logging**: while the server is running,
  edit `.env` (e.g. change `LOG_LEVEL=info` to `LOG_LEVEL=debug`) and send
  `kill -HUP <pid>`. The running process reloads and logs exactly what
  changed — no restart required:
  ```
  2026/01/01 12:00:00 reload: config changed:
  2026/01/01 12:00:00   LogLevel: "info" -> "debug"
  ```

See the comment block at the top of `cmd/server/main.go` for the full set of
things to try, including hitting `/health` and `/tasks`.

## Test

```
go test ./... -cover
```
