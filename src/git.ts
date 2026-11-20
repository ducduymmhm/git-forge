// Git object model and wire formats: object hashing, trees/commits/tags, pkt-lines and packfiles.
// Everything here is pure and synchronous; the store and the smart-HTTP layer build on it.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  readonly type: ObjType;
  readonly data: Buffer;
}

export const ZERO_OID = '0000000000000000000000000000000000000000';
export const OID_RE = /^[0-9a-f]{40}$/;

const TYPE_CODE: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export class GitError extends Error {}

/** The object id git gives `data` stored as `type`. */
export function hashObject(type: ObjType, data: Buffer): string {
  return createHash('sha1').update(`${type} ${data.length}\0`).update(data).digest('hex');
}

// ---------------------------------------------------------------- trees

export interface TreeEntry {
  readonly mode: string;
  readonly name: string;
  readonly oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let pos = 0;
  while (pos < data.length) {
    const space = data.indexOf(0x20, pos);
    const nul = data.indexOf(0, space + 1);
    if (space < 0 || nul < 0 || nul + 21 > data.length) throw new GitError('malformed tree object');
    entries.push({
      mode: data.toString('latin1', pos, space),
      name: data.toString('utf8', space + 1, nul),
      oid: data.toString('hex', nul + 1, nul + 21),
    });
    pos = nul + 21;
  }
  return entries;
}

/** Git sorts tree entries by name, comparing a directory as if its name ended in '/'. */
function treeSortKey(entry: TreeEntry): string {
  return entry.mode === '40000' || entry.mode === '040000' ? `${entry.name}/` : entry.name;
}

export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const ka = treeSortKey(a);
    const kb = treeSortKey(b);
    return ka < kb ? -1 : ka > kb ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    const mode = entry.mode === '040000' ? '40000' : entry.mode;
    parts.push(Buffer.from(`${mode} ${entry.name}\0`, 'utf8'), Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export function isTreeMode(mode: string): boolean {
  return mode === '40000' || mode === '040000';
}

// ---------------------------------------------------------------- commits and tags

export interface Signature {
  readonly name: string;
  readonly email: string;
  /** Seconds since the epoch. */
  readonly time: number;
  /** "+0000" style offset. */
  readonly tz: string;
}

export interface Commit {
  readonly tree: string;
  readonly parents: string[];
  readonly author: Signature;
  readonly committer: Signature;
  readonly message: string;
}

export function parseSignature(text: string): Signature {
  const match = /^(.*?) <([^>]*)> (-?\d+) ([+-]\d{4})$/.exec(text);
  if (!match) return { name: text, email: '', time: 0, tz: '+0000' };
  return { name: match[1], email: match[2], time: Number(match[3]), tz: match[4] };
}

export function formatSignature(sig: Signature): string {
  return `${sig.name} <${sig.email}> ${sig.time} ${sig.tz}`;
}

/** Header lines up to the blank line, then the message. Continuation lines (space-led) join the previous header. */
function parseHeaders(data: Buffer): { headers: Array<[string, string]>; message: string } {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  const headers: Array<[string, string]> = [];
  for (const line of head.split('\n')) {
    if (line.startsWith(' ') && headers.length > 0) {
      const last = headers[headers.length - 1];
      last[1] += `\n${line.slice(1)}`;
      continue;
    }
    const space = line.indexOf(' ');
    if (space < 0) continue;
    headers.push([line.slice(0, space), line.slice(space + 1)]);
  }
  return { headers, message };
}

export function parseCommit(data: Buffer): Commit {
  const { headers, message } = parseHeaders(data);
  let tree = '';
  const parents: string[] = [];
  let author: Signature = { name: '', email: '', time: 0, tz: '+0000' };
  let committer = author;
  for (const [key, value] of headers) {
    if (key === 'tree') tree = value;
    else if (key === 'parent') parents.push(value);
    else if (key === 'author') author = parseSignature(value);
    else if (key === 'committer') committer = parseSignature(value);
  }
  return { tree, parents, author, committer, message };
}

export function buildCommit(commit: Commit): Buffer {
  const lines = [`tree ${commit.tree}`];
  for (const parent of commit.parents) lines.push(`parent ${parent}`);
  lines.push(`author ${formatSignature(commit.author)}`, `committer ${formatSignature(commit.committer)}`);
  const message = commit.message.endsWith('\n') ? commit.message : `${commit.message}\n`;
  return Buffer.from(`${lines.join('\n')}\n\n${message}`, 'utf8');
}

export interface Tag {
  readonly object: string;
  readonly type: string;
  readonly tag: string;
  readonly message: string;
}

export function parseTag(data: Buffer): Tag {
  const { headers, message } = parseHeaders(data);
  let object = '';
  let type = '';
  let tag = '';
  for (const [key, value] of headers) {
    if (key === 'object') object = value;
    else if (key === 'type') type = value;
    else if (key === 'tag') tag = value;
  }
  return { object, type, tag, message };
}

// ---------------------------------------------------------------- pkt-lines

export const FLUSH = Buffer.from('0000', 'latin1');
export const DELIM = Buffer.from('0001', 'latin1');
export const RESPONSE_END = Buffer.from('0002', 'latin1');
/** git's LARGE_PACKET_MAX: the largest pkt-line including its 4-byte length. */
export const PKT_MAX = 65520;

export function pkt(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const head = Buffer.from((body.length + 4).toString(16).padStart(4, '0'), 'latin1');
  return Buffer.concat([head, body]);
}

export type Packet = { kind: 'data'; payload: Buffer } | { kind: 'flush' } | { kind: 'delim' } | { kind: 'end' };

/** Decode pkt-lines from `buf` starting at `start` until `stopAtFlush` sees a flush, or the end. */
export function readPackets(buf: Buffer, start = 0, stopAtFlush = false): { packets: Packet[]; end: number } {
  const packets: Packet[] = [];
  let pos = start;
  while (pos < buf.length) {
    if (pos + 4 > buf.length) throw new GitError('truncated pkt-line length');
    const lenText = buf.toString('latin1', pos, pos + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(lenText)) throw new GitError(`bad pkt-line length ${JSON.stringify(lenText)}`);
    const len = parseInt(lenText, 16);
    if (len === 0) {
      packets.push({ kind: 'flush' });
      pos += 4;
      if (stopAtFlush) break;
      continue;
    }
    if (len === 1) {
      packets.push({ kind: 'delim' });
      pos += 4;
      continue;
    }
    if (len === 2) {
      packets.push({ kind: 'end' });
      pos += 4;
      continue;
    }
    if (len < 4 || pos + len > buf.length) throw new GitError('truncated pkt-line');
    packets.push({ kind: 'data', payload: buf.subarray(pos + 4, pos + len) });
    pos += len;
  }
  return { packets, end: pos };
}

/** A data packet's text without its trailing newline. */
export function packetText(packet: Packet): string {
  if (packet.kind !== 'data') return '';
  const text = packet.payload.toString('utf8');
  return text.endsWith('\n') ? text.slice(0, -1) : text;
}

/** Frame `data` on side-band `band` in packets no larger than PKT_MAX. */
export function sideband(band: number, data: Buffer): Buffer[] {
  const out: Buffer[] = [];
  const chunk = PKT_MAX - 5;
  for (let pos = 0; pos < data.length; pos += chunk) {
    const piece = data.subarray(pos, Math.min(data.length, pos + chunk));
    const head = Buffer.alloc(5);
    head.write((piece.length + 5).toString(16).padStart(4, '0'), 0, 'latin1');
    head[4] = band;
    out.push(head, piece);
  }
  return out;
}

// ---------------------------------------------------------------- packfiles

export interface PackedObject {
  readonly oid: string;
  readonly type: ObjType;
  readonly data: Buffer;
}

/** Apply a git delta to `base`. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const varint = (): number => {
    let result = 0;
    let shift = 0;
    let byte: number;
    do {
      if (pos >= delta.length) throw new GitError('truncated delta header');
      byte = delta[pos++];
      result += (byte & 0x7f) * 2 ** shift;
      shift += 7;
    } while (byte & 0x80);
    return result;
  };
  const baseSize = varint();
  if (baseSize !== base.length) throw new GitError(`delta base size ${baseSize} does not match ${base.length}`);
  const resultSize = varint();
  const out = Buffer.allocUnsafe(resultSize);
  let outPos = 0;
  while (pos < delta.length) {
    const op = delta[pos++];
    if (op & 0x80) {
      let offset = 0;
      let size = 0;
      if (op & 0x01) offset |= delta[pos++];
      if (op & 0x02) offset |= delta[pos++] << 8;
      if (op & 0x04) offset |= delta[pos++] << 16;
      if (op & 0x08) offset = (offset | (delta[pos++] << 24)) >>> 0;
      if (op & 0x10) size |= delta[pos++];
      if (op & 0x20) size |= delta[pos++] << 8;
      if (op & 0x40) size |= delta[pos++] << 16;
      if (size === 0) size = 0x10000;
      if (offset + size > base.length || outPos + size > resultSize) throw new GitError('delta copy out of range');
      base.copy(out, outPos, offset, offset + size);
      outPos += size;
    } else if (op > 0) {
      if (pos + op > delta.length || outPos + op > resultSize) throw new GitError('delta insert out of range');
      delta.copy(out, outPos, pos, pos + op);
      pos += op;
      outPos += op;
    } else {
      throw new GitError('delta opcode 0 is reserved');
    }
  }
  if (outPos !== resultSize) throw new GitError('delta result size mismatch');
  return out;
}

interface RawEntry {
  offset: number;
  code: number;
  data: Buffer;
  baseOffset?: number;
  baseOid?: string;
}

/**
 * Parse a version 2/3 packfile. `lookup` resolves the base of a REF_DELTA the pack itself does not
 * carry (a thin pack built against objects the server already has).
 */
export function readPack(buf: Buffer, lookup: (oid: string) => GitObject | undefined): PackedObject[] {
  if (buf.length < 32 || buf.toString('latin1', 0, 4) !== 'PACK') throw new GitError('pack does not start with PACK');
  const version = buf.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new GitError(`unsupported pack version ${version}`);
  const count = buf.readUInt32BE(8);
  const trailer = buf.length - 20;
  const actual = createHash('sha1').update(buf.subarray(0, trailer)).digest();
  if (!actual.equals(buf.subarray(trailer))) throw new GitError('pack checksum mismatch');
  const entries: RawEntry[] = [];
  let pos = 12;
  for (let index = 0; index < count; index += 1) {
    const offset = pos;
    let byte = buf[pos++];
    const code = (byte >> 4) & 7;
    let size = byte & 15;
    let shift = 4;
    while (byte & 0x80) {
      byte = buf[pos++];
      size += (byte & 0x7f) * 2 ** shift;
      shift += 7;
    }
    const entry: RawEntry = { offset, code, data: Buffer.alloc(0) };
    if (code === 6) {
      byte = buf[pos++];
      let rel = byte & 0x7f;
      while (byte & 0x80) {
        byte = buf[pos++];
        rel = ((rel + 1) * 128) + (byte & 0x7f);
      }
      entry.baseOffset = offset - rel;
    } else if (code === 7) {
      entry.baseOid = buf.toString('hex', pos, pos + 20);
      pos += 20;
    } else if (!CODE_TYPE[code]) {
      throw new GitError(`unknown pack object type ${code}`);
    }
    if (pos > trailer) throw new GitError('pack truncated');
    const inflated = inflateSync(buf.subarray(pos, trailer), { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
    if (inflated.buffer.length !== size) throw new GitError(`pack object at ${offset} inflates to ${inflated.buffer.length}, header says ${size}`);
    entry.data = inflated.buffer;
    pos += inflated.engine.bytesWritten;
    entries.push(entry);
  }
  // Resolve whole objects first, then deltas in rounds until every base is known: an ofs-delta's base
  // by its offset, a ref-delta's by its id (in the pack, or already in the store for a thin pack).
  const resolved = new Map<number, PackedObject>();
  const byOid = new Map<string, PackedObject>();
  const settle = (entry: RawEntry, type: ObjType, data: Buffer): void => {
    const object: PackedObject = { oid: hashObject(type, data), type, data };
    resolved.set(entry.offset, object);
    byOid.set(object.oid, object);
  };
  let pending: RawEntry[] = [];
  for (const entry of entries) {
    if (entry.code === 6 || entry.code === 7) pending.push(entry);
    else settle(entry, CODE_TYPE[entry.code], entry.data);
  }
  while (pending.length > 0) {
    const next: RawEntry[] = [];
    for (const entry of pending) {
      let base: GitObject | undefined;
      if (entry.code === 6) base = resolved.get(entry.baseOffset as number);
      else base = byOid.get(entry.baseOid as string) ?? lookup(entry.baseOid as string);
      if (base === undefined) next.push(entry);
      else settle(entry, base.type, applyDelta(base.data, entry.data));
    }
    if (next.length === pending.length) {
      const first = next[0];
      throw new GitError(first.code === 6 ? `ofs-delta base at ${first.baseOffset} is not in the pack` : `ref-delta base ${first.baseOid} is missing`);
    }
    pending = next;
  }
  return entries.map((entry) => resolved.get(entry.offset) as PackedObject);
}

function packHeader(type: ObjType, size: number): Buffer {
  const bytes: number[] = [];
  let byte = (TYPE_CODE[type] << 4) | (size & 15);
  let rest = Math.floor(size / 16);
  while (rest > 0) {
    bytes.push(byte | 0x80);
    byte = rest & 0x7f;
    rest = Math.floor(rest / 128);
  }
  bytes.push(byte);
  return Buffer.from(bytes);
}

/** One object's pack entry (header plus deflated body): cacheable per object id. */
export function packEntry(type: ObjType, data: Buffer): Buffer {
  return Buffer.concat([packHeader(type, data.length), deflateSync(data, { level: 1 })]);
}

/** A complete version 2 pack from pre-built entries. */
export function assemblePack(entries: readonly Buffer[]): Buffer {
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'latin1');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(entries.length, 8);
  const hash = createHash('sha1').update(header);
  for (const entry of entries) hash.update(entry);
  return Buffer.concat([header, ...entries, hash.digest()]);
}
