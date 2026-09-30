// The stack's API: GET /api/visits counts one visit in PostgreSQL and says
// how many there have been; GET /health answers ok once the database does.
//
// Usage: node server.js PORT DB_HOST
"use strict";

const http = require("http");
const fs = require("fs");
const { Pool } = require("pg");

const [port, dbHost] = process.argv.slice(2);
const pool = new Pool({ host: dbHost, user: "counter", database: "counter", max: 4, connectionTimeoutMillis: 2000 });

const osName = (() => {
  try {
    return /^PRETTY_NAME="?([^"\n]*)/m.exec(fs.readFileSync("/etc/os-release", "utf8"))[1];
  } catch {
    return "unknown";
  }
})();

// The table, created on first use; a failure (the database not up yet) is
// tried again on the next request.
let table = null;
function ensureTable() {
  if (!table) {
    table = pool.query("CREATE TABLE IF NOT EXISTS visits (at timestamptz NOT NULL DEFAULT now())")
      .catch((err) => { table = null; throw err; });
  }
  return table;
}

function reply(res, code, body) {
  const data = JSON.stringify(body) + "\n";
  res.writeHead(code, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(data) });
  res.end(data);
}

http.createServer(async (req, res) => {
  try {
    await ensureTable();
    if (req.url === "/health") {
      await pool.query("SELECT 1");
      return reply(res, 200, { ok: true });
    }
    if (req.url === "/api/visits") {
      await pool.query("INSERT INTO visits DEFAULT VALUES");
      const { rows } = await pool.query("SELECT count(*)::int AS n, version() AS db FROM visits");
      return reply(res, 200, { visits: rows[0].n, db: rows[0].db.split(" on ")[0], node: process.version, os: osName });
    }
    reply(res, 404, { error: "not found" });
  } catch (err) {
    reply(res, 503, { error: "database unavailable", detail: err.code || err.message });
  }
}).listen(Number(port));
