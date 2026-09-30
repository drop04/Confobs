#!/usr/bin/env node
// Notifications service — demo app #2 for confobs.
//
// Like the Python inventory service next to it, this file contains no
// confobs code. It reads configuration the ordinary way any Node service
// would (process.env), and trusts it completely. That trust is only safe
// because this process is started as:
//
//   confobs run --schema ../schema.yaml -- node server.js
//
// confobs validates the environment and fills in schema defaults *before*
// this file's first line ever executes. If the configuration were invalid,
// this process would never have been started at all.

const http = require("http");
const { URL } = require("url");

const START_TIME = Date.now();
const NOTIFICATIONS = []; // in-memory for the demo

function env(name, { required = false, cast = (v) => v, def = undefined } = {}) {
  const raw = process.env[name] ?? def;
  if (raw === undefined) {
    if (required) {
      // Last-resort guard for running `node server.js` directly, bypassing
      // confobs. In a normal deployment this branch never runs.
      console.error(
        `FATAL: ${name} is not set (did you forget to run this through ` +
          "\`confobs run\`?)"
      );
      process.exit(1);
    }
    return undefined;
  }
  return cast(raw);
}

const PORT = env("PORT", { cast: Number, def: "8080" });
const LOG_LEVEL = env("LOG_LEVEL", { def: "info" });
const MAX_CONNS = env("MAX_CONNS", { cast: Number, def: "20" });
const FEATURE_BETA = env("FEATURE_BETA", {
  cast: (v) => v.toLowerCase() === "true",
  def: "false",
});
const DATABASE_URL = env("DATABASE_URL", { required: true }); // never logged
const API_KEY = env("API_KEY", { required: true }); // never logged

function log(msg) {
  if (LOG_LEVEL === "debug") console.error(`[notifications] ${msg}`);
}

function sendJSON(res, status, payload) {
  const body = JSON.stringify(payload);
  res.writeHead(status, {
    "Content-Type": "application/json",
    "Content-Length": Buffer.byteLength(body),
  });
  res.end(body);
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, `http://${req.headers.host}`);
  log(`${req.method} ${url.pathname}`);

  if (req.method === "GET" && url.pathname === "/health") {
    sendJSON(res, 200, {
      status: "ok",
      service: "notifications",
      language: "node",
      uptime_seconds: Math.round((Date.now() - START_TIME) / 100) / 10,
      // DATABASE_URL and API_KEY are deliberately absent here, same as the
      // Python service — a health endpoint should never be able to leak a
      // secret, no matter what validated it upstream.
      config: {
        port: PORT,
        log_level: LOG_LEVEL,
        max_conns: MAX_CONNS,
        feature_beta: FEATURE_BETA,
      },
      notification_count: NOTIFICATIONS.length,
    });
    return;
  }

  if (req.method === "GET" && url.pathname === "/notifications") {
    sendJSON(res, 200, { notifications: NOTIFICATIONS });
    return;
  }

  if (req.method === "POST" && url.pathname === "/notifications") {
    const to = url.searchParams.get("to");
    const message = url.searchParams.get("message");
    if (!to || !message) {
      sendJSON(res, 400, { error: "missing ?to= and/or ?message=" });
      return;
    }
    const notification = { to, message, sent_at: new Date().toISOString() };
    NOTIFICATIONS.push(notification);
    sendJSON(res, 201, { sent: notification });
    return;
  }

  sendJSON(res, 404, { error: "not found" });
});

server.listen(PORT, () => {
  console.log(
    `notifications (node): config OK, starting on :${PORT} ` +
      `(log_level=${LOG_LEVEL}, max_conns=${MAX_CONNS}, ` +
      `feature_beta=${FEATURE_BETA})`
  );
});
