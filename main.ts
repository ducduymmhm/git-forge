// git-forge: a standalone GitHub-compatible forge in one Node process.
//
// The trial lifecycle: bind an ephemeral port, POST it to $MAKEFUN_ENV_URL/register, serve the suite
// (git smart HTTP, REST v3, GraphQL v4 and MCP), and poll $MAKEFUN_ENV_URL/done until the suite is
// finished, then exit 0. Run outside a trial (no MAKEFUN_ENV_URL) it simply serves on PORT (or an
// ephemeral port) until stopped.
//
// V8 tuning the harness cannot pass on its command line: one thread (no GC or compiler helper
// threads, so no cross-thread futex wake-ups per request) and a large young generation (few
// scavenges). The launcher re-executes this file once with those flags and mirrors the child's exit;
// if the child cannot be started, it serves in-process instead.

import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { Forge } from './src/store.ts';
import { createForgeServer } from './src/server.ts';

/** The viewer every trial's token names (the suite's seeded scenario viewer), unless overridden. */
const DEFAULT_LOGIN = 'adaiversen';
const TUNING = ['--single-threaded', '--max-semi-space-size=64'];

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function log(line: string): void {
  process.stdout.write(`[forge] ${line}\n`);
}

async function serve(): Promise<void> {
  const envUrl = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
  const token = process.env.MAKEFUN_SHOP_TOKEN;
  const login = process.env.MAKEFUN_FORGE_LOGIN || process.env.FORGE_LOGIN || DEFAULT_LOGIN;
  const forge = new Forge(login);
  const server = createForgeServer(forge, { token: token !== undefined && token !== '' ? token : null });

  const listen = async (port: number): Promise<number> => {
    await new Promise<void>((resolve, reject) => {
      server.once('error', reject);
      server.listen(port, () => resolve());
    });
    const address = server.address();
    if (address === null || typeof address === 'string') throw new Error('the server did not bind a TCP port');
    return address.port;
  };

  if (envUrl === '') {
    const port = await listen(Number(process.env.PORT ?? 0));
    log(`serving on http://127.0.0.1:${port} as ${login}${token ? '' : ' (any bearer token accepted)'}`);
    return;
  }

  const port = await listen(0);
  log(`listening on port ${port} as ${login}`);
  // Register: the env takes it (200), or another register already won (409).
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${envUrl}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(10_000),
      });
      await res.arrayBuffer().catch(() => undefined);
      if (res.status === 200 || res.status === 409) break;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered')) throw err;
    }
    await sleep(Math.min(1_000, 50 * 2 ** Math.min(attempt, 5)));
  }
  log('registered; serving until the env says done');
  // Poll GET /done until the env answers {"done": true}.
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${envUrl}/done`, { signal: AbortSignal.timeout(30_000) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) break;
        attempt = 0;
      } else {
        await res.arrayBuffer().catch(() => undefined);
      }
    } catch {
      // the env is busy or restarting: keep polling
    }
    await sleep(Math.min(2_000, 250 * 2 ** Math.min(attempt, 3)));
  }
  log('done');
  server.close();
  process.exit(0);
}

function runInProcess(): void {
  for (const signal of ['SIGINT', 'SIGTERM'] as const) process.on(signal, () => process.exit(0));
  process.on('uncaughtException', (err) => {
    process.stderr.write(`[forge] uncaught: ${err.stack ?? err.message}\n`);
  });
  process.on('unhandledRejection', (err) => {
    process.stderr.write(`[forge] unhandled rejection: ${err instanceof Error ? (err.stack ?? err.message) : String(err)}\n`);
  });
  serve().catch((err: unknown) => {
    process.stderr.write(`[forge] ${err instanceof Error ? err.message : String(err)}\n`);
    process.exit(1);
  });
}

if (process.env.FORGE_TUNED === '1' || process.env.FORGE_NO_TUNING === '1') {
  runInProcess();
} else {
  const child = spawn(process.execPath, [...TUNING, fileURLToPath(import.meta.url)], {
    stdio: 'inherit',
    env: { ...process.env, FORGE_TUNED: '1' },
  });
  let fellBack = false;
  const startedAt = Date.now();
  const fallBack = (): void => {
    if (fellBack) return;
    fellBack = true;
    runInProcess();
  };
  child.on('error', fallBack);
  child.on('exit', (code, signal) => {
    if (fellBack) return;
    // A child that dies at once (say, a flag this Node does not know) never served: serve here instead.
    if (code !== 0 && signal === null && Date.now() - startedAt < 3_000) {
      fallBack();
      return;
    }
    process.exit(code ?? (signal === null ? 0 : 1));
  });
  for (const signal of ['SIGINT', 'SIGTERM'] as const) {
    process.on(signal, () => {
      if (fellBack) process.exit(0);
      child.kill(signal);
    });
  }
}
