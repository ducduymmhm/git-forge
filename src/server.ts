// The HTTP front: one node:http server routing /healthz, git smart HTTP, REST v3, GraphQL and MCP,
// with bearer-token auth, GitHub's response headers and JSON errors everywhere.

import { createServer } from 'node:http';
import type { IncomingMessage, Server, ServerResponse } from 'node:http';
import { GitError } from './git.ts';
import { ForgeError } from './store.ts';
import type { Forge } from './store.ts';
import { advertise, receivePack, uploadPack } from './smart-http.ts';
import { handleRest, notFound } from './rest.ts';
import type { ApiResponse } from './rest.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';

export interface ServerOptions {
  /** The one token the forge accepts; null accepts any non-empty bearer (local runs). */
  readonly token: string | null;
}

const JSON_TYPE = 'application/json; charset=utf-8';
const API_HEADERS: Record<string, string> = {
  'X-OAuth-Scopes': 'repo, read:org, workflow, gist, user',
  'X-Accepted-OAuth-Scopes': 'repo',
  'X-GitHub-Media-Type': 'github.v3; format=json',
  'X-GitHub-Api-Version-Selected': '2022-11-28',
};

let requestCounter = 0;
const requestPrefix = `${process.pid.toString(16).toUpperCase()}:${Date.now().toString(16).toUpperCase()}`;

function requestId(): string {
  requestCounter += 1;
  return `${requestPrefix}:${requestCounter.toString(16).toUpperCase()}`;
}

/** The token a request presents: Bearer, GitHub's "token", or the password of Basic auth. */
function presentedToken(req: IncomingMessage): string | null {
  const header = req.headers.authorization;
  if (header === undefined || header === '') return null;
  const space = header.indexOf(' ');
  if (space < 0) return null;
  const scheme = header.slice(0, space).toLowerCase();
  const value = header.slice(space + 1).trim();
  if (scheme === 'bearer' || scheme === 'token') return value === '' ? null : value;
  if (scheme === 'basic') {
    const decoded = Buffer.from(value, 'base64').toString('utf8');
    const colon = decoded.indexOf(':');
    const secret = colon < 0 ? decoded : decoded.slice(colon + 1);
    return secret === '' ? (colon < 0 ? null : decoded.slice(0, colon) || null) : secret;
  }
  return null;
}

function send(res: ServerResponse, status: number, contentType: string | null, body: Buffer | string | null, headers: Record<string, string> = {}): void {
  const out: Record<string, string | number> = { 'X-GitHub-Request-Id': requestId(), ...headers };
  if (contentType !== null) out['Content-Type'] = contentType;
  if (body === null) {
    if (status !== 204 && status !== 304) out['Content-Length'] = 0;
    res.writeHead(status, out);
    res.end();
    return;
  }
  out['Content-Length'] = typeof body === 'string' ? Buffer.byteLength(body) : body.length;
  res.writeHead(status, out);
  res.end(body);
}

function sendJson(res: ServerResponse, status: number, value: unknown, headers: Record<string, string> = {}): void {
  send(res, status, JSON_TYPE, typeof value === 'string' ? value : JSON.stringify(value), headers);
}

function sendApi(res: ServerResponse, response: ApiResponse): void {
  const headers = { ...API_HEADERS, ...(response.headers ?? {}) };
  if (response.body === undefined) {
    send(res, response.status, response.status === 204 ? null : JSON_TYPE, null, headers);
    return;
  }
  sendJson(res, response.status, response.body, headers);
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('end', () => resolve(chunks.length === 1 ? chunks[0] : Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

/** Parse a JSON request body; undefined for an empty one, a SyntaxError for a malformed one. */
function parseJson(body: Buffer): unknown {
  if (body.length === 0) return undefined;
  const text = body.toString('utf8');
  if (text.trim() === '') return undefined;
  return JSON.parse(text);
}

const GIT_PATH = /^\/([^/]+)\/([^/]+?)(?:\.git)?\/(info\/refs|git-upload-pack|git-receive-pack)$/;

export function createForgeServer(forge: Forge, options: ServerOptions): Server {
  const authorized = (req: IncomingMessage): 'ok' | 'missing' | 'bad' => {
    const token = presentedToken(req);
    if (token === null) return 'missing';
    if (options.token !== null && token !== options.token) return 'bad';
    return 'ok';
  };

  const handle = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const rawUrl = req.url ?? '/';
    const queryAt = rawUrl.indexOf('?');
    const pathname = queryAt < 0 ? rawUrl : rawUrl.slice(0, queryAt);
    const query = new URLSearchParams(queryAt < 0 ? '' : rawUrl.slice(queryAt + 1));
    const method = (req.method ?? 'GET').toUpperCase();
    const origin = `http://${req.headers.host ?? 'localhost'}`;

    if (pathname === '/healthz' || pathname === '/health' || pathname === '/readyz') {
      sendJson(res, 200, '{"ok":true}');
      return;
    }

    const body = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);

    // REST v3.
    if (pathname === '/api/v3' || pathname.startsWith('/api/v3/')) {
      const auth = authorized(req);
      if (auth !== 'ok') {
        sendApi(res, { status: 401, body: { message: auth === 'missing' ? 'Requires authentication' : 'Bad credentials', documentation_url: 'https://docs.github.com/rest', status: '401' } });
        return;
      }
      let parsed: unknown;
      try {
        parsed = parseJson(body);
      } catch {
        sendApi(res, { status: 400, body: { message: 'Problems parsing JSON', documentation_url: 'https://docs.github.com/rest', status: '400' } });
        return;
      }
      if (method !== 'GET' && method !== 'DELETE' && parsed !== undefined && (typeof parsed !== 'object' || parsed === null)) {
        sendApi(res, { status: 400, body: { message: 'Body should be a JSON object', documentation_url: 'https://docs.github.com/rest', status: '400' } });
        return;
      }
      const rest = pathname.slice('/api/v3'.length);
      let segments: string[];
      try {
        segments = rest.split('/').filter((segment) => segment !== '').map((segment) => decodeURIComponent(segment));
      } catch {
        sendApi(res, notFound());
        return;
      }
      sendApi(res, handleRest(forge, { method, segments, query, body: parsed, origin }));
      return;
    }

    // GraphQL v4.
    if (pathname === '/api/graphql' || pathname === '/graphql') {
      const auth = authorized(req);
      if (auth !== 'ok') {
        sendJson(res, 401, { message: auth === 'missing' ? 'This endpoint requires you to be authenticated.' : 'Bad credentials', documentation_url: 'https://docs.github.com/graphql/guides/forming-calls-with-graphql#authenticating-with-graphql' }, { 'WWW-Authenticate': 'Bearer realm="forge"' });
        return;
      }
      if (method !== 'POST' && method !== 'GET') {
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }
      let payload: unknown;
      try {
        payload = method === 'GET' ? { query: query.get('query') ?? '', variables: query.has('variables') ? JSON.parse(query.get('variables') as string) : {}, operationName: query.get('operationName') } : parseJson(body);
      } catch {
        sendJson(res, 400, { message: 'Problems parsing JSON', documentation_url: 'https://docs.github.com/graphql' });
        return;
      }
      const result = handleGraphQL(forge, payload, origin);
      sendJson(res, result.status, result.body, { 'X-OAuth-Scopes': API_HEADERS['X-OAuth-Scopes'] });
      return;
    }

    // MCP (Streamable HTTP).
    if (pathname === '/mcp' || pathname === '/mcp/') {
      const result = handleMcp(forge, {
        method,
        body,
        authorized: authorized(req),
        headers: req.headers,
        origin,
      });
      send(res, result.status, result.body === null ? null : JSON_TYPE, result.body, result.headers);
      return;
    }

    // Git smart HTTP.
    const git = GIT_PATH.exec(pathname);
    if (git !== null) {
      const [, ownerRaw, repoRaw, action] = git;
      let owner: string;
      let name: string;
      try {
        owner = decodeURIComponent(ownerRaw);
        name = decodeURIComponent(repoRaw);
      } catch {
        sendJson(res, 404, { message: 'Repository not found' });
        return;
      }
      const auth = authorized(req);
      if (auth !== 'ok') {
        sendJson(res, 401, { message: auth === 'missing' ? 'Requires authentication' : 'Bad credentials' }, { 'WWW-Authenticate': 'Basic realm="forge"' });
        return;
      }
      const repo = forge.getRepo(owner, name);
      if (repo === undefined) {
        sendJson(res, 404, { message: 'Repository not found' });
        return;
      }
      const v2 = /version=2/.test(String(req.headers['git-protocol'] ?? ''));
      const noCache = { 'Cache-Control': 'no-cache, max-age=0, must-revalidate', Expires: 'Fri, 01 Jan 1980 00:00:00 GMT', Pragma: 'no-cache' };
      try {
        if (action === 'info/refs' && method === 'GET') {
          const service = query.get('service') ?? '';
          if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
            sendJson(res, 403, { message: 'dumb HTTP is not served; use the smart protocol' });
            return;
          }
          const out = advertise(forge, repo, service, v2);
          send(res, out.status, out.contentType, out.body, noCache);
          return;
        }
        if (action === 'git-upload-pack' && method === 'POST') {
          const out = uploadPack(forge, repo, await inflateIfNeeded(req, body), v2);
          send(res, out.status, out.contentType, out.body, noCache);
          return;
        }
        if (action === 'git-receive-pack' && method === 'POST') {
          const out = receivePack(forge, repo, await inflateIfNeeded(req, body));
          send(res, out.status, out.contentType, out.body, noCache);
          return;
        }
      } catch (err) {
        if (err instanceof GitError || err instanceof ForgeError) {
          sendJson(res, 400, { message: err.message });
          return;
        }
        throw err;
      }
      sendJson(res, 405, { message: 'Method Not Allowed' });
      return;
    }

    sendJson(res, 404, { message: 'Not Found', documentation_url: 'https://docs.github.com/rest' });
  };

  const server = createServer({ keepAlive: true, noDelay: true }, (req, res) => {
    handle(req, res).catch((err: unknown) => {
      process.stderr.write(`[forge] ${req.method} ${req.url}: ${err instanceof Error ? (err.stack ?? err.message) : String(err)}\n`);
      if (!res.headersSent) sendJson(res, 500, { message: 'Internal Server Error' });
      else res.destroy();
    });
  });
  server.keepAliveTimeout = 120_000;
  server.headersTimeout = 130_000;
  server.requestTimeout = 0;
  return server;
}

/** git sends gzip-encoded request bodies when they are large. */
async function inflateIfNeeded(req: IncomingMessage, body: Buffer): Promise<Buffer> {
  const encoding = String(req.headers['content-encoding'] ?? '').toLowerCase();
  if (encoding === 'gzip' || encoding === 'x-gzip') {
    const { gunzipSync } = await import('node:zlib');
    return gunzipSync(body);
  }
  return body;
}
