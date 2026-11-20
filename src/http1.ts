// A minimal HTTP/1.1 server over node:net: keep-alive, pipelining, Content-Length and chunked request
// bodies, and every response written with a single socket write. It replaces node:http on the hot
// path, where its per-request objects, timers and writev calls cost more than the work itself.

import { createServer } from 'node:net';
import type { Server, Socket } from 'node:net';

export interface RawRequest {
  readonly method: string;
  readonly url: string;
  /** Header names lowercased; repeated headers joined with ", ". */
  readonly headers: Record<string, string>;
  readonly body: Buffer;
}

export interface RawResponse {
  readonly status: number;
  readonly headers: Record<string, string | number>;
  readonly body: Buffer | string | null;
}

export type RawHandler = (req: RawRequest) => RawResponse | Promise<RawResponse>;

const REASONS: Record<number, string> = {
  200: 'OK', 201: 'Created', 202: 'Accepted', 204: 'No Content', 304: 'Not Modified', 400: 'Bad Request', 401: 'Unauthorized',
  403: 'Forbidden', 404: 'Not Found', 405: 'Method Not Allowed', 409: 'Conflict', 410: 'Gone', 413: 'Payload Too Large',
  422: 'Unprocessable Entity', 431: 'Request Header Fields Too Large', 500: 'Internal Server Error',
};

const MAX_HEAD = 64 * 1024;
const MAX_BODY = 512 * 1024 * 1024;

class ParseError extends Error {}

/** Serialize one response; a string body is UTF-8. */
function encode(res: RawResponse, keepAlive: boolean, head: boolean): Buffer {
  const body = res.body === null ? null : typeof res.body === 'string' ? Buffer.from(res.body, 'utf8') : res.body;
  let text = `HTTP/1.1 ${res.status} ${REASONS[res.status] ?? 'Status'}\r\n`;
  for (const name in res.headers) text += `${name}: ${res.headers[name]}\r\n`;
  if (res.status !== 204 && res.status !== 304) text += `Content-Length: ${body === null ? 0 : body.length}\r\n`;
  text += keepAlive ? 'Connection: keep-alive\r\n\r\n' : 'Connection: close\r\n\r\n';
  if (body === null || body.length === 0 || head) return Buffer.from(text, 'latin1');
  const headLength = Buffer.byteLength(text, 'latin1');
  const out = Buffer.allocUnsafe(headLength + body.length);
  out.latin1Write(text, 0);
  body.copy(out, headLength);
  return out;
}

interface Pending {
  method: string;
  url: string;
  headers: Record<string, string>;
  keepAlive: boolean;
  /** Content-Length, or -1 for a chunked body. */
  length: number;
}

function parseHead(buf: Buffer, end: number): Pending {
  const text = buf.latin1Slice(0, end);
  const lines = text.split('\r\n');
  const first = lines[0];
  const sp1 = first.indexOf(' ');
  const sp2 = first.lastIndexOf(' ');
  if (sp1 <= 0 || sp2 <= sp1) throw new ParseError('bad request line');
  const method = first.slice(0, sp1);
  const url = first.slice(sp1 + 1, sp2);
  const version = first.slice(sp2 + 1);
  const headers: Record<string, string> = {};
  for (let index = 1; index < lines.length; index += 1) {
    const line = lines[index];
    const colon = line.indexOf(':');
    if (colon <= 0) continue;
    const name = line.slice(0, colon).trim().toLowerCase();
    const value = line.slice(colon + 1).trim();
    headers[name] = headers[name] === undefined ? value : `${headers[name]}, ${value}`;
  }
  const connection = (headers.connection ?? '').toLowerCase();
  const keepAlive = version === 'HTTP/1.0' ? connection === 'keep-alive' : connection !== 'close';
  let length = 0;
  if ((headers['transfer-encoding'] ?? '').toLowerCase().includes('chunked')) length = -1;
  else if (headers['content-length'] !== undefined) {
    length = Number(headers['content-length']);
    if (!Number.isSafeInteger(length) || length < 0 || length > MAX_BODY) throw new ParseError('bad content-length');
  }
  return { method, url, headers, keepAlive, length };
}

/** Decode a complete chunked body starting at `start`, or null when more bytes are needed. */
function readChunked(buf: Buffer, start: number): { body: Buffer; end: number } | null {
  const parts: Buffer[] = [];
  let pos = start;
  for (;;) {
    const lineEnd = buf.indexOf('\r\n', pos, 'latin1');
    if (lineEnd < 0) return null;
    const size = parseInt(buf.latin1Slice(pos, lineEnd).split(';')[0].trim(), 16);
    if (!Number.isFinite(size) || size < 0) throw new ParseError('bad chunk size');
    pos = lineEnd + 2;
    if (size === 0) {
      // Trailers end with an empty line.
      for (;;) {
        const trailerEnd = buf.indexOf('\r\n', pos, 'latin1');
        if (trailerEnd < 0) return null;
        const empty = trailerEnd === pos;
        pos = trailerEnd + 2;
        if (empty) return { body: parts.length === 1 ? parts[0] : Buffer.concat(parts), end: pos };
      }
    }
    if (pos + size + 2 > buf.length) return null;
    parts.push(buf.subarray(pos, pos + size));
    pos += size + 2;
  }
}

function serve(socket: Socket, handler: RawHandler, onError: (err: unknown) => void): void {
  socket.setNoDelay(true);
  let buffer: Buffer = Buffer.alloc(0);
  let pending: Pending | null = null;
  let headEnd = 0;
  let busy = false;
  let closed = false;

  const fail = (status: number, message: string): void => {
    if (closed) return;
    closed = true;
    const body = JSON.stringify({ message });
    socket.end(encode({ status, headers: { 'Content-Type': 'application/json; charset=utf-8' }, body }, false, false));
  };

  const pump = (): void => {
    while (!busy && !closed) {
      if (pending === null) {
        const end = buffer.indexOf('\r\n\r\n', 0, 'latin1');
        if (end < 0) {
          if (buffer.length > MAX_HEAD) fail(431, 'Request Header Fields Too Large');
          return;
        }
        try {
          pending = parseHead(buffer, end);
        } catch {
          fail(400, 'Bad Request');
          return;
        }
        headEnd = end + 4;
      }
      let body: Buffer;
      let consumed: number;
      if (pending.length === -1) {
        let chunked: { body: Buffer; end: number } | null;
        try {
          chunked = readChunked(buffer, headEnd);
        } catch {
          fail(400, 'Bad Request');
          return;
        }
        if (chunked === null) return;
        body = chunked.body;
        consumed = chunked.end;
      } else {
        if (buffer.length < headEnd + pending.length) return;
        body = buffer.subarray(headEnd, headEnd + pending.length);
        consumed = headEnd + pending.length;
      }
      const request = pending;
      pending = null;
      buffer = consumed === buffer.length ? Buffer.alloc(0) : buffer.subarray(consumed);
      const req: RawRequest = { method: request.method, url: request.url, headers: request.headers, body };
      const isHead = request.method === 'HEAD';
      let result: RawResponse | Promise<RawResponse>;
      try {
        result = handler(req);
      } catch (err) {
        onError(err);
        result = { status: 500, headers: { 'Content-Type': 'application/json; charset=utf-8' }, body: '{"message":"Internal Server Error"}' };
      }
      if (result instanceof Promise) {
        busy = true;
        result.then(
          (res) => finish(res, request.keepAlive, isHead),
          (err: unknown) => {
            onError(err);
            finish({ status: 500, headers: { 'Content-Type': 'application/json; charset=utf-8' }, body: '{"message":"Internal Server Error"}' }, request.keepAlive, isHead);
          },
        );
        return;
      }
      if (!write(result, request.keepAlive, isHead)) return;
    }
  };

  const write = (res: RawResponse, keepAlive: boolean, head: boolean): boolean => {
    if (closed) return false;
    const out = encode(res, keepAlive, head);
    if (keepAlive) {
      socket.write(out);
      return true;
    }
    closed = true;
    socket.end(out);
    return false;
  };

  const finish = (res: RawResponse, keepAlive: boolean, head: boolean): void => {
    busy = false;
    if (write(res, keepAlive, head)) pump();
  };

  socket.on('data', (chunk: Buffer) => {
    buffer = buffer.length === 0 ? chunk : Buffer.concat([buffer, chunk]);
    pump();
  });
  socket.on('error', () => {
    closed = true;
    socket.destroy();
  });
  socket.on('end', () => {
    closed = true;
    socket.end();
  });
}

export function createRawServer(handler: RawHandler, onError: (err: unknown) => void): Server {
  return createServer({ noDelay: true, keepAlive: true }, (socket) => serve(socket, handler, onError));
}
