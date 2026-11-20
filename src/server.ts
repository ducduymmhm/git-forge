// The HTTP front: routes /healthz, git smart HTTP, REST v3, GraphQL and MCP, with bearer-token auth,
// GitHub's response headers and JSON errors everywhere. It runs on the raw HTTP/1.1 server in
// http1.ts: each request arrives whole and each response leaves in one write.

import type { Server } from 'node:net';
import { gunzipSync } from 'node:zlib';
import { GitError } from './git.ts';
import { ForgeError } from './store.ts';
import type { Forge } from './store.ts';
import { advertise, receivePack, uploadPack } from './smart-http.ts';
import { handleRest, notFound } from './rest.ts';
import type { ApiResponse } from './rest.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { createRawServer } from './http1.ts';
import type { RawRequest, RawResponse } from './http1.ts';

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
const NO_CACHE = { 'Cache-Control': 'no-cache, max-age=0, must-revalidate', Expires: 'Fri, 01 Jan 1980 00:00:00 GMT', Pragma: 'no-cache' };
const DOCS = 'https://docs.github.com/rest';

let requestCounter = 0;
const requestPrefix = `${process.pid.toString(16).toUpperCase()}:${Date.now().toString(16).toUpperCase()}`;

function requestId(): string {
  requestCounter += 1;
  return `${requestPrefix}:${requestCounter.toString(16).toUpperCase()}`;
}

/** The token a request presents: Bearer, GitHub's "token", or the password of Basic auth. */
function presentedToken(header: string | undefined): string | null {
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

function reply(status: number, contentType: string | null, body: Buffer | string | null, headers: Record<string, string> = {}): RawResponse {
  const out: Record<string, string | number> = { 'X-GitHub-Request-Id': requestId() };
  for (const name in headers) out[name] = headers[name];
  if (contentType !== null) out['Content-Type'] = contentType;
  return { status, headers: out, body };
}

function json(status: number, value: unknown, headers: Record<string, string> = {}): RawResponse {
  return reply(status, JSON_TYPE, typeof value === 'string' ? value : JSON.stringify(value), headers);
}

function api(response: ApiResponse): RawResponse {
  const headers = response.headers === undefined ? API_HEADERS : { ...API_HEADERS, ...response.headers };
  if (response.body === undefined) return reply(response.status, response.status === 204 ? null : JSON_TYPE, null, headers);
  return json(response.status, response.body, headers);
}

/** Parse a JSON request body; undefined for an empty one, a SyntaxError for a malformed one. */
function parseJson(body: Buffer): unknown {
  if (body.length === 0) return undefined;
  const text = body.toString('utf8');
  if (text.trim() === '') return undefined;
  return JSON.parse(text);
}

/** git sends gzip-encoded request bodies when they are large. */
function inflateIfNeeded(req: RawRequest): Buffer {
  const encoding = (req.headers['content-encoding'] ?? '').toLowerCase();
  return encoding === 'gzip' || encoding === 'x-gzip' ? gunzipSync(req.body) : req.body;
}

const GIT_PATH = /^\/([^/]+)\/([^/]+?)(?:\.git)?\/(info\/refs|git-upload-pack|git-receive-pack)$/;

export function createForgeServer(forge: Forge, options: ServerOptions): Server {
  const authorized = (req: RawRequest): 'ok' | 'missing' | 'bad' => {
    const token = presentedToken(req.headers.authorization);
    if (token === null) return 'missing';
    if (options.token !== null && token !== options.token) return 'bad';
    return 'ok';
  };

  const restRoute = (req: RawRequest, method: string, pathname: string, query: URLSearchParams, origin: string): RawResponse => {
    const auth = authorized(req);
    if (auth !== 'ok') return api({ status: 401, body: { message: auth === 'missing' ? 'Requires authentication' : 'Bad credentials', documentation_url: DOCS, status: '401' } });
    let parsed: unknown;
    try {
      parsed = parseJson(req.body);
    } catch {
      return api({ status: 400, body: { message: 'Problems parsing JSON', documentation_url: DOCS, status: '400' } });
    }
    if (method !== 'GET' && method !== 'DELETE' && parsed !== undefined && (typeof parsed !== 'object' || parsed === null)) {
      return api({ status: 400, body: { message: 'Body should be a JSON object', documentation_url: DOCS, status: '400' } });
    }
    let segments: string[];
    try {
      segments = pathname.slice('/api/v3'.length).split('/').filter((segment) => segment !== '').map((segment) => decodeURIComponent(segment));
    } catch {
      return api(notFound());
    }
    return api(handleRest(forge, { method, segments, query, body: parsed, origin }));
  };

  const graphqlRoute = (req: RawRequest, method: string, query: URLSearchParams, origin: string): RawResponse => {
    const auth = authorized(req);
    if (auth !== 'ok') {
      return json(401, { message: auth === 'missing' ? 'This endpoint requires you to be authenticated.' : 'Bad credentials', documentation_url: 'https://docs.github.com/graphql/guides/forming-calls-with-graphql#authenticating-with-graphql' }, { 'WWW-Authenticate': 'Bearer realm="forge"' });
    }
    if (method !== 'POST' && method !== 'GET') return json(404, { message: 'Not Found' });
    let payload: unknown;
    try {
      payload = method === 'GET' ? { query: query.get('query') ?? '', variables: query.has('variables') ? JSON.parse(query.get('variables') as string) : {}, operationName: query.get('operationName') } : parseJson(req.body);
    } catch {
      return json(400, { message: 'Problems parsing JSON', documentation_url: 'https://docs.github.com/graphql' });
    }
    const result = handleGraphQL(forge, payload, origin);
    return json(result.status, result.body, { 'X-OAuth-Scopes': API_HEADERS['X-OAuth-Scopes'] });
  };

  const gitRoute = (req: RawRequest, method: string, match: RegExpExecArray, query: URLSearchParams): RawResponse => {
    const [, ownerRaw, repoRaw, action] = match;
    let owner: string;
    let name: string;
    try {
      owner = decodeURIComponent(ownerRaw);
      name = decodeURIComponent(repoRaw);
    } catch {
      return json(404, { message: 'Repository not found' });
    }
    const auth = authorized(req);
    if (auth !== 'ok') return json(401, { message: auth === 'missing' ? 'Requires authentication' : 'Bad credentials' }, { 'WWW-Authenticate': 'Basic realm="forge"' });
    const repo = forge.getRepo(owner, name);
    if (repo === undefined) return json(404, { message: 'Repository not found' });
    const v2 = (req.headers['git-protocol'] ?? '').includes('version=2');
    try {
      if (action === 'info/refs' && method === 'GET') {
        const service = query.get('service') ?? '';
        if (service !== 'git-upload-pack' && service !== 'git-receive-pack') return json(403, { message: 'dumb HTTP is not served; use the smart protocol' });
        const out = advertise(forge, repo, service, v2);
        return reply(out.status, out.contentType, out.body, NO_CACHE);
      }
      if (action === 'git-upload-pack' && method === 'POST') {
        const out = uploadPack(forge, repo, inflateIfNeeded(req), v2);
        return reply(out.status, out.contentType, out.body, NO_CACHE);
      }
      if (action === 'git-receive-pack' && method === 'POST') {
        const out = receivePack(forge, repo, inflateIfNeeded(req));
        return reply(out.status, out.contentType, out.body, NO_CACHE);
      }
    } catch (err) {
      if (err instanceof GitError || err instanceof ForgeError) return json(400, { message: err.message });
      throw err;
    }
    return json(405, { message: 'Method Not Allowed' });
  };

  const handle = (req: RawRequest): RawResponse => {
    const rawUrl = req.url;
    const queryAt = rawUrl.indexOf('?');
    const pathname = queryAt < 0 ? rawUrl : rawUrl.slice(0, queryAt);
    const query = new URLSearchParams(queryAt < 0 ? '' : rawUrl.slice(queryAt + 1));
    const method = req.method.toUpperCase();
    const origin = `http://${req.headers.host ?? 'localhost'}`;

    if (pathname === '/healthz' || pathname === '/health' || pathname === '/readyz') return json(200, '{"ok":true}');
    if (pathname === '/api/v3' || pathname.startsWith('/api/v3/')) return restRoute(req, method, pathname, query, origin);
    if (pathname === '/api/graphql' || pathname === '/graphql') return graphqlRoute(req, method, query, origin);
    if (pathname === '/mcp' || pathname === '/mcp/') {
      const result = handleMcp(forge, { method, body: req.body, authorized: authorized(req), headers: req.headers, origin });
      return reply(result.status, result.body === null ? null : JSON_TYPE, result.body, result.headers);
    }
    const git = GIT_PATH.exec(pathname);
    if (git !== null) return gitRoute(req, method, git, query);
    return json(404, { message: 'Not Found', documentation_url: DOCS });
  };

  return createRawServer(handle, (err: unknown) => {
    process.stderr.write(`[forge] ${err instanceof Error ? (err.stack ?? err.message) : String(err)}\n`);
  });
}
