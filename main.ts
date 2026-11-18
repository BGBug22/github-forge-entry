// Entry point bounty "Build a GitHub-compatible git forge" (Go rebuild).
// Node 22 hanya jadi launcher: memilih port ephemeral, menjalankan binary
// statis ./forge, mendaftarkan port ke environment make.fun, lalu long-poll
// GET /done sampai suite selesai dan keluar 0.
//
// Semua logika forge (git smart HTTP, REST v3, GraphQL, MCP) hidup di dalam
// binary Go — file ini tidak menyimpan state apa pun.
import { spawn, type ChildProcess } from "node:child_process";
import { createServer } from "node:net";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const ENV_URL = process.env.MAKEFUN_ENV_URL ?? "";
const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? "";
const TRIAL_NOW = process.env.MAKEFUN_TRIAL_NOW ?? "";

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// Pilih port ephemeral yang bebas (listen 0 lalu tutup).
async function pickPort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.once("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const addr = srv.address();
      const port = typeof addr === "object" && addr !== null ? addr.port : 0;
      srv.close(() => resolve(port));
    });
  });
}

async function waitHealth(port: number, timeoutMs: number): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`http://127.0.0.1:${port}/healthz`);
      if (res.ok) return true;
    } catch {
      // belum naik — coba lagi
    }
    await sleep(100);
  }
  return false;
}

async function main(): Promise<number> {
  if (!ENV_URL) {
    console.error("[main] MAKEFUN_ENV_URL tidak diset");
    return 1;
  }
  const here = dirname(fileURLToPath(import.meta.url));
  const binary = join(here, "forge");
  const port = await pickPort();
  console.log(`[main] forge di port ${port}`);

  const child: ChildProcess = spawn(binary, [], {
    env: {
      ...process.env,
      FORGE_PORT: String(port),
      MAKEFUN_SHOP_TOKEN: TOKEN,
      MAKEFUN_TRIAL_NOW: TRIAL_NOW,
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  child.stdout?.on("data", (d: Buffer) => process.stdout.write(`[forge] ${d}`));
  child.stderr?.on("data", (d: Buffer) => process.stderr.write(`[forge] ${d}`));
  const childGone = new Promise<number>((resolve) => {
    child.once("exit", (code) => resolve(code ?? 1));
  });

  const up = await waitHealth(port, 15000);
  if (!up) {
    console.error("[main] forge tidak naik dalam 15 detik");
    child.kill("SIGTERM");
    return 1;
  }
  console.log(`[main] forge sehat di port ${port}`);

  // Daftarkan port ke environment (first register menang).
  try {
    const res = await fetch(`${ENV_URL}/register`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ port }),
    });
    console.log(`[main] register -> ${res.status}`);
  } catch (err) {
    console.error("[main] register gagal:", err);
  }

  // Long-poll /done sampai suite selesai.
  const deadline = Date.now() + 880_000;
  while (Date.now() < deadline) {
    const doneRace = await Promise.race([
      (async () => {
        try {
          const res = await fetch(`${ENV_URL}/done`);
          if (res.ok) {
            const data = (await res.json()) as { done?: boolean };
            return data.done === true;
          }
        } catch {
          // env belum siap — coba lagi
        }
        return false;
      })(),
      sleep(1000).then(() => false),
    ]);
    if (doneRace) {
      console.log("[main] done=true, mematikan forge");
      child.kill("SIGTERM");
      return 0;
    }
    const exited = await Promise.race([
      childGone.then((c) => ({ exited: true, code: c })),
      sleep(0).then(() => ({ exited: false, code: 0 })),
    ]);
    if (exited.exited) {
      console.error(`[main] forge mati duluan (code ${exited.code})`);
      return 1;
    }
    // Jeda antar poll agar tidak membanjiri /done.
    await sleep(500);
  }
  console.error("[main] timeout menunggu /done");
  child.kill("SIGTERM");
  return 1;
}

process.exitCode = await main();
