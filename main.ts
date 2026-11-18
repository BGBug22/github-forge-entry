// Entry point (Node 22): spawn the static Go forge binary, register its
// port with the make.fun environment, then long-poll /done and exit 0.
// The Go binary holds all state in memory; this launcher only wires it up.
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import { fileURLToPath } from "node:url";
import path from "node:path";

const dir = path.dirname(fileURLToPath(import.meta.url));
const envUrl = process.env.MAKEFUN_ENV_URL ?? "";

// The repo stores the binary as a plain file (mode 100644 through the
// API); make it executable at runtime so the spawn below never fails
// with EACCES on a fresh checkout.
try {
  fs.chmodSync(path.join(dir, "forge"), 0o755);
} catch {
  // the binary is already executable, or the error surfaces at spawn
}

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.once("error", reject);
    s.listen(0, "127.0.0.1", () => {
      const port = s.address().port;
      s.close(() => resolve(port));
    });
  });
}

async function waitHealth(port, timeoutMs = 20000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    try {
      const r = await fetch(`http://127.0.0.1:${port}/healthz`);
      if (r.ok) return true;
    } catch {
      // not up yet
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  return false;
}

const port = await freePort();
const child = spawn(path.join(dir, "forge"), [], {
  cwd: dir,
  env: { ...process.env, FORGE_PORT: String(port) },
  stdio: ["ignore", "pipe", "pipe"],
});
child.stderr.on("data", (d) => process.stderr.write(d));
let childExited = false;
child.on("exit", () => {
  childExited = true;
});

if (!(await waitHealth(port))) {
  console.error("forge did not become healthy on port", port);
  child.kill("SIGTERM");
  process.exit(1);
}

if (!envUrl) {
  // Local dev run: no environment to register with.
  console.log(`forge listening on http://127.0.0.1:${port}`);
  await new Promise(() => {});
}

// Register the port (first register wins; a 409 means it is already in).
try {
  const res = await fetch(`${envUrl}/register`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ port }),
  });
  if (!res.ok && res.status !== 409) {
    console.error("register failed:", res.status);
  }
} catch (e) {
  console.error("register error:", e);
}

// Long-poll /done until the suite finishes.
const deadline = Date.now() + 880_000;
while (Date.now() < deadline) {
  try {
    const res = await fetch(`${envUrl}/done`);
    if (res.ok) {
      const body = await res.json();
      if (body && body.done === true) break;
    }
  } catch {
    // environment not reachable yet; keep polling
  }
  if (childExited) break;
  await new Promise((r) => setTimeout(r, 1000));
}

child.kill("SIGTERM");
process.exit(0);
