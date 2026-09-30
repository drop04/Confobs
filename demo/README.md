# confobs polyglot demo

One schema (`schema.yaml`), enforced identically for two services written in
two different languages, neither of which contains a single line of confobs
code. This exists to prove one claim: **the validation and drift-detection
engine is language-agnostic**, because it lives entirely in the `confobs`
binary, not in the application.

```
demo/
  schema.yaml               the one config contract both services follow
  python-inventory/app.py   an inventory service (Python stdlib only)
  node-notifications/server.js   a notifications service (Node stdlib only)
```

Both apps read configuration the ordinary, unvalidated way
(`os.environ` / `process.env`) — on purpose. They trust their environment
completely. That trust is only safe because `confobs run` already validated
it *before* either process's first line of code ever executed.

## 1. Build the CLI

From the repository root:

```
go build -o bin/confobs ./cmd/confobs
```

The commands below assume you're running them from the repository root and
invoking the binary as `bin/confobs`; adjust the path if you've moved it
onto your `PATH`.

## 2. Set up an env file for each service

```
cat > demo/python-inventory/.env <<'EOF'
DATABASE_URL=postgres://demo:demo@localhost:5432/inventory
API_KEY=sk-inventory-demo
LOG_LEVEL=debug
EOF

cat > demo/node-notifications/.env <<'EOF'
DATABASE_URL=postgres://demo:demo@localhost:5432/notifications
API_KEY=sk-notifications-demo
LOG_LEVEL=debug
EOF
```

## 3. Start both services through confobs

```
cd demo/python-inventory
PORT=8091 ../../bin/confobs run --schema ../schema.yaml --env-file .env -- python3 app.py
```

```
cd demo/node-notifications
PORT=8092 ../../bin/confobs run --schema ../schema.yaml --env-file .env -- node server.js
```

Each prints a line like:

```
confobs: config OK (7 fields, ../schema.yaml) - starting: python3 app.py
inventory (python): config OK, starting on :8091 (log_level=debug, max_conns=20, feature_beta=False)
```

Then use them normally:

```
curl -s localhost:8091/health
curl -s -X POST "localhost:8091/items?name=widget&qty=5"
curl -s localhost:8091/items

curl -s localhost:8092/health
curl -s -X POST "localhost:8092/notifications?to=alice@example.com&message=hi"
curl -s localhost:8092/notifications
```

## 4. Prove the typo case (the actual point)

Misspell the one field that matters most, in either service's `.env`:

```
sed -i 's/DATABASE_URL/DATABSE_URL/' demo/python-inventory/.env
```

Try to start it the same way as step 3 (from inside `demo/python-inventory`,
`PORT=8091 ../../bin/confobs run --schema ../schema.yaml --env-file .env --
python3 app.py`). It never starts:

```
confobs: not starting "python3" - 1 config problem(s) against ../schema.yaml

  error    DATABASE_URL           required, but not set - DATABSE_URL is set, though. Did you mean DATABASE_URL?
```

No Python code ran. No partially-configured process came up. Do the same
thing to the Node service's `.env` and the message is identical, character
for character, because it's the same engine.

## 5. Prove drift detection

Run from the repository root:

```
bin/confobs diff --schema demo/schema.yaml --env-file demo/python-inventory/.env \
  --snapshot /tmp/inventory-snapshot.json
# first run: "no earlier snapshot - baseline written"

sed -i 's/LOG_LEVEL=debug/LOG_LEVEL=info/' demo/python-inventory/.env

bin/confobs diff --schema demo/schema.yaml --env-file demo/python-inventory/.env \
  --snapshot /tmp/inventory-snapshot.json
# second run: "config drift - 1 change(s)" ... LOG_LEVEL  debug  ->  info
```

`confobs run --snapshot FILE` does the same check automatically on every
start, so drift is visible on every deploy without a separate step.

## What this demonstrates

- The exact same schema file, the exact same binary, the exact same typo
  message — for a Python service and a Node service.
- Neither service imports, links, or even knows confobs exists.
- The Go library (`confobs.Load`, struct tags) in the rest of this repo
  and this CLI are two front-ends over the one engine in `schema.go` — a Go
  program, a Python program and a Node program governed by an equivalent
  schema get identical validation and identical drift detection.
