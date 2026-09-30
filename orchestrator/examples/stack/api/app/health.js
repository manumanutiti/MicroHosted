// The image's health check: exit 0 when the API answers /health (which
// answers ok only once the database does).
fetch("http://127.0.0.1:8080/health", { signal: AbortSignal.timeout(2000) })
  .then((r) => process.exit(r.ok ? 0 : 1), () => process.exit(1));
