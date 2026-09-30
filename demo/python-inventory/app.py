#!/usr/bin/env python3
"""Inventory service — demo app #1 for confobs.

This file contains no confobs code at all, on purpose. It reads its
configuration the plain, ordinary way any Python service would:

    os.environ["DATABASE_URL"]

The validation, typo detection, default-filling and drift logging all happen
*outside* this process, because it is started as:

    confobs run --schema ../schema.yaml -- python3 app.py

confobs checks the environment, fills in any schema defaults that are
missing, and only then execs this script with that resulting environment. If
the configuration is invalid, this file never even starts running.
"""
import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

START_TIME = time.time()
ITEMS = []  # in-memory for the demo


def env(name, required=False, cast=str, default=None):
    """A deliberately dumb, unvalidated env reader — the whole point is that
    this app trusts its environment completely and does zero checking of its
    own. That trust is only safe because confobs already checked it."""
    raw = os.environ.get(name, default)
    if raw is None:
        if required:
            # In a well-formed deployment this line never runs: confobs run
            # would have refused to start the process at all. It's here only
            # as a last-resort guard if someone runs `python3 app.py` directly,
            # bypassing confobs.
            print(f"FATAL: {name} is not set (did you forget to run this "
                  f"through `confobs run`?)", file=sys.stderr)
            sys.exit(1)
        return None
    return cast(raw)


PORT = env("PORT", cast=int, default="8080")
LOG_LEVEL = env("LOG_LEVEL", default="info")
MAX_CONNS = env("MAX_CONNS", cast=int, default="20")
FEATURE_BETA = env("FEATURE_BETA", cast=lambda v: v.lower() == "true", default="false")
DATABASE_URL = env("DATABASE_URL", required=True)   # never logged or returned
API_KEY = env("API_KEY", required=True)              # never logged or returned


def log(msg):
    if LOG_LEVEL == "debug":
        print(f"[inventory] {msg}", file=sys.stderr)


class Handler(BaseHTTPRequestHandler):
    server_version = "confobs-demo-inventory/1.0"

    def _json(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        log(f"GET {self.path}")
        if self.path == "/health":
            self._json(200, {
                "status": "ok",
                "service": "inventory",
                "language": "python",
                "uptime_seconds": round(time.time() - START_TIME, 1),
                # Note what is deliberately absent: DATABASE_URL and API_KEY
                # are never included here, even though this handler has them
                # in memory — a health endpoint should not be able to leak a
                # secret, regardless of what confobs does elsewhere.
                "config": {
                    "port": PORT,
                    "log_level": LOG_LEVEL,
                    "max_conns": MAX_CONNS,
                    "feature_beta": FEATURE_BETA,
                },
                "item_count": len(ITEMS),
            })
        elif self.path == "/items":
            self._json(200, {"items": ITEMS})
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self):
        log(f"POST {self.path}")
        if self.path.startswith("/items"):
            from urllib.parse import urlparse, parse_qs
            qs = parse_qs(urlparse(self.path).query)
            name = qs.get("name", [None])[0]
            qty = qs.get("qty", ["0"])[0]
            if not name:
                self._json(400, {"error": "missing ?name="})
                return
            item = {"name": name, "qty": int(qty)}
            ITEMS.append(item)
            self._json(201, {"added": item})
        else:
            self._json(404, {"error": "not found"})

    def log_message(self, fmt, *args):
        pass  # keep stdout clean; use log() above instead


def main():
    print(f"inventory (python): config OK, starting on :{PORT} "
          f"(log_level={LOG_LEVEL}, max_conns={MAX_CONNS}, "
          f"feature_beta={FEATURE_BETA})")
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    server.serve_forever()


if __name__ == "__main__":
    main()
