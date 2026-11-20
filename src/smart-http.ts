// Git smart HTTP: the info/refs advertisements (upload-pack v0 and v2, receive-pack v0), upload-pack's
// ls-refs and fetch (with shallow, deepen and thin haves), and receive-pack (pack unpacking, per-ref
// fast-forward checks and a report-status on side-band-64k).

import {
  assemblePack,
  DELIM,
  FLUSH,
  GitError,
  OID_RE,
  packetText,
  parseTag,
  parseTree,
  pkt,
  readPack,
  readPackets,
  sideband,
  ZERO_OID,
  isTreeMode,
} from './git.ts';
import type { Packet } from './git.ts';
import type { Forge, Repo } from './store.ts';

export interface GitReply {
  readonly status: number;
  readonly contentType: string;
  readonly body: Buffer;
}

const AGENT = 'agent=git-forge/1.0';
const UPLOAD_V0_CAPS = ['thin-pack', 'side-band', 'side-band-64k', 'ofs-delta', 'shallow', 'deepen-since', 'deepen-not', 'deepen-relative', 'no-progress', 'include-tag', 'allow-tip-sha1-in-want', 'allow-reachable-sha1-in-want', 'object-format=sha1', AGENT];
const RECEIVE_CAPS = ['report-status', 'delete-refs', 'side-band-64k', 'quiet', 'atomic', 'ofs-delta', 'object-format=sha1', AGENT];

function reply(status: number, contentType: string, parts: Buffer[]): GitReply {
  return { status, contentType, body: parts.length === 1 ? parts[0] : Buffer.concat(parts) };
}

/** Refs in advertisement order: sorted by name. */
function sortedRefs(repo: Repo): Array<[string, string]> {
  return [...repo.refs.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
}

// ---------------------------------------------------------------- advertisements

export function advertise(forge: Forge, repo: Repo, service: string, v2: boolean): GitReply {
  const type = `application/x-${service}-advertisement`;
  if (service === 'git-upload-pack' && v2) {
    return reply(200, type, [
      pkt('version 2\n'),
      pkt(`${AGENT}\n`),
      pkt('ls-refs=unborn\n'),
      pkt('fetch=shallow\n'),
      pkt('server-option\n'),
      pkt('object-format=sha1\n'),
      FLUSH,
    ]);
  }
  const parts: Buffer[] = [pkt(`# service=${service}\n`), FLUSH];
  const refs = sortedRefs(repo);
  const lines: string[] = [];
  if (service === 'git-upload-pack') {
    const head = repo.refs.get(forge.headRef(repo));
    if (head !== undefined) lines.push(`${head} HEAD`);
    for (const [name, oid] of refs) {
      lines.push(`${oid} ${name}`);
      if (name.startsWith('refs/tags/')) {
        const peeled = forge.peel(oid);
        if (peeled !== oid) lines.push(`${peeled} ${name}^{}`);
      }
    }
  } else {
    for (const [name, oid] of refs) lines.push(`${oid} ${name}`);
  }
  const caps = service === 'git-upload-pack' ? [...UPLOAD_V0_CAPS, `symref=HEAD:${forge.headRef(repo)}`] : RECEIVE_CAPS;
  if (lines.length === 0) lines.push(`${ZERO_OID} capabilities^{}`);
  lines.forEach((line, index) => parts.push(pkt(index === 0 ? `${line}\0${caps.join(' ')}\n` : `${line}\n`)));
  parts.push(FLUSH);
  return reply(200, type, parts);
}

// ---------------------------------------------------------------- upload-pack

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  deepenSince: number | null;
  deepenNot: string[];
  done: boolean;
  includeTag: boolean;
  sideband: boolean;
}

/** The objects a fetch must send and the shallow boundary changes it makes. */
interface FetchPlan {
  objects: string[];
  shallow: string[];
  unshallow: string[];
  common: string[];
}

const fetchCache = new Map<string, Buffer>();
const FETCH_CACHE_MAX = 64;

function planFetch(forge: Forge, repo: Repo, args: FetchArgs): FetchPlan {
  const clientShallow = new Set(args.shallow);
  const common = args.haves.filter((oid) => forge.getObject(oid) !== undefined);

  // What the client already holds: everything reachable from its haves, down to its shallow boundary.
  const held = new Set<string>();
  const heldCommits = new Set<string>();
  if (common.length > 0) {
    const stack = [...common];
    while (stack.length > 0) {
      const oid = forge.peel(stack.pop() as string);
      if (heldCommits.has(oid)) continue;
      const commit = forge.commitOf(oid);
      if (commit === null) continue;
      heldCommits.add(oid);
      held.add(oid);
      if (!clientShallow.has(oid)) stack.push(...commit.parents);
    }
    for (const oid of heldCommits) {
      const commit = forge.commitOf(oid);
      if (commit !== null) markTree(forge, commit.tree, held);
    }
  }

  // The deepen-not boundary: commits reachable from those refs are not sent.
  const excluded = new Set<string>();
  for (const name of args.deepenNot) {
    const tip = forge.resolve(repo, name);
    if (tip === null) continue;
    for (const oid of forge.walkCommits([tip], 1_000_000)) excluded.add(oid);
  }

  const send: string[] = [];
  const sent = new Set<string>();
  const add = (oid: string): void => {
    if (sent.has(oid) || held.has(oid)) return;
    sent.add(oid);
    send.push(oid);
  };

  const commitDepth = new Map<string, number>();
  const queue: Array<{ oid: string; depth: number }> = [];
  for (const want of args.wants) {
    let oid = want;
    // Annotated tags travel with what they name.
    for (let guard = 0; guard < 100; guard += 1) {
      const object = forge.getObject(oid);
      if (object === undefined || object.type !== 'tag') break;
      add(oid);
      oid = parseTag(object.data).object;
    }
    const object = forge.getObject(oid);
    if (object === undefined) continue;
    if (object.type === 'commit') queue.push({ oid, depth: 1 });
    else if (object.type === 'tree') addTree(forge, oid, held, sent, send);
    else add(oid);
  }

  const newShallow: string[] = [];
  const traversed = new Set<string>();
  const sentCommits: string[] = [];
  while (queue.length > 0) {
    const { oid, depth } = queue.shift() as { oid: string; depth: number };
    const known = commitDepth.get(oid);
    if (known !== undefined && known <= depth) continue;
    commitDepth.set(oid, depth);
    if (excluded.has(oid)) continue;
    const commit = forge.commitOf(oid);
    if (commit === null) continue;
    if (args.depth === null && heldCommits.has(oid)) continue;
    if (args.deepenSince !== null && commit.committer.time < args.deepenSince && depth > 1) continue;
    traversed.add(oid);
    if (!heldCommits.has(oid) && !sent.has(oid)) {
      add(oid);
      sentCommits.push(oid);
    }
    const atLimit = args.depth !== null && depth >= args.depth;
    if (atLimit) {
      if (commit.parents.length > 0 && !clientShallow.has(oid)) newShallow.push(oid);
      continue;
    }
    if (args.depth === null && args.deepenSince === null && args.deepenNot.length === 0 && clientShallow.has(oid)) continue;
    for (const parent of commit.parents) queue.push({ oid: parent, depth: depth + 1 });
  }
  for (const oid of sentCommits) {
    const commit = forge.commitOf(oid);
    if (commit !== null) addTree(forge, commit.tree, held, sent, send);
  }

  // A client shallow commit whose parents now travel is no longer shallow.
  const unshallow: string[] = [];
  const deepening = args.depth !== null || args.deepenSince !== null || args.deepenNot.length > 0;
  if (deepening) {
    for (const oid of clientShallow) {
      if (!traversed.has(oid) || newShallow.includes(oid)) continue;
      const commit = forge.commitOf(oid);
      if (commit === null) continue;
      if (commit.parents.some((parent) => sent.has(parent))) unshallow.push(oid);
    }
  }

  if (args.includeTag) {
    for (const [name, oid] of repo.refs) {
      if (!name.startsWith('refs/tags/') || sent.has(oid)) continue;
      const object = forge.getObject(oid);
      if (object !== undefined && object.type === 'tag' && sent.has(parseTag(object.data).object)) add(oid);
    }
  }

  return { objects: send, shallow: newShallow, unshallow, common };
}

function markTree(forge: Forge, treeOid: string, into: Set<string>): void {
  if (into.has(treeOid)) return;
  into.add(treeOid);
  const object = forge.getObject(treeOid);
  if (object === undefined || object.type !== 'tree') return;
  for (const entry of parseTree(object.data)) {
    if (isTreeMode(entry.mode)) markTree(forge, entry.oid, into);
    else if (entry.mode !== '160000') into.add(entry.oid);
  }
}

function addTree(forge: Forge, treeOid: string, held: Set<string>, sent: Set<string>, send: string[]): void {
  if (held.has(treeOid) || sent.has(treeOid)) return;
  sent.add(treeOid);
  send.push(treeOid);
  const object = forge.getObject(treeOid);
  if (object === undefined || object.type !== 'tree') return;
  for (const entry of parseTree(object.data)) {
    if (isTreeMode(entry.mode)) addTree(forge, entry.oid, held, sent, send);
    else if (entry.mode !== '160000' && !held.has(entry.oid) && !sent.has(entry.oid)) {
      sent.add(entry.oid);
      send.push(entry.oid);
    }
  }
}

function buildPack(forge: Forge, oids: readonly string[]): Buffer {
  return assemblePack(oids.map((oid) => forge.packEntryOf(oid)));
}

function parseOid(text: string, what: string): string {
  const oid = text.trim().toLowerCase();
  if (!OID_RE.test(oid)) throw new GitError(`${what} is not an object id: ${JSON.stringify(text)}`);
  return oid;
}

function newArgs(): FetchArgs {
  return { wants: [], haves: [], shallow: [], depth: null, deepenSince: null, deepenNot: [], done: false, includeTag: false, sideband: true };
}

/** One fetch argument line (shared by v0 and v2). */
function fetchLine(args: FetchArgs, line: string): void {
  if (line.startsWith('want ')) args.wants.push(parseOid(line.slice(5, 45), 'want'));
  else if (line.startsWith('have ')) args.haves.push(parseOid(line.slice(5, 45), 'have'));
  else if (line.startsWith('shallow ')) args.shallow.push(parseOid(line.slice(8, 48), 'shallow'));
  else if (line.startsWith('deepen-since ')) args.deepenSince = Number(line.slice(13));
  else if (line.startsWith('deepen-not ')) args.deepenNot.push(line.slice(11));
  else if (line.startsWith('deepen ')) {
    const depth = Number(line.slice(7));
    if (!Number.isInteger(depth) || depth < 1) throw new GitError(`bad deepen ${JSON.stringify(line)}`);
    args.depth = depth;
  } else if (line === 'done') args.done = true;
  else if (line === 'include-tag') args.includeTag = true;
}

function fetchKey(repo: Repo, args: FetchArgs, v2: boolean): string {
  return [repo.id, repo.version, v2 ? 2 : 0, args.wants.join(','), args.haves.join(','), args.shallow.join(','), args.depth, args.deepenSince, args.deepenNot.join(','), args.done, args.includeTag, args.sideband].join('|');
}

function remember(key: string, body: Buffer): void {
  if (fetchCache.size >= FETCH_CACHE_MAX) {
    const oldest = fetchCache.keys().next().value;
    if (oldest !== undefined) fetchCache.delete(oldest);
  }
  fetchCache.set(key, body);
}

function fetchV2(forge: Forge, repo: Repo, args: FetchArgs): Buffer {
  const key = fetchKey(repo, args, true);
  const cached = fetchCache.get(key);
  if (cached !== undefined) return cached;
  const plan = planFetch(forge, repo, args);
  const parts: Buffer[] = [];
  if (!args.done) {
    parts.push(pkt('acknowledgments\n'));
    if (plan.common.length === 0) parts.push(pkt('NAK\n'));
    for (const oid of plan.common) parts.push(pkt(`ACK ${oid}\n`));
    parts.push(pkt('ready\n'), DELIM);
  }
  if (args.depth !== null || args.shallow.length > 0 || args.deepenSince !== null || args.deepenNot.length > 0) {
    parts.push(pkt('shallow-info\n'));
    for (const oid of plan.shallow) parts.push(pkt(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pkt(`unshallow ${oid}\n`));
    parts.push(DELIM);
  }
  parts.push(pkt('packfile\n'), ...sideband(1, buildPack(forge, plan.objects)), FLUSH);
  const body = Buffer.concat(parts);
  remember(key, body);
  return body;
}

function lsRefs(forge: Forge, repo: Repo, argLines: string[]): Buffer {
  const prefixes: string[] = [];
  let peel = false;
  let symrefs = false;
  let unborn = false;
  for (const line of argLines) {
    if (line === 'peel') peel = true;
    else if (line === 'symrefs') symrefs = true;
    else if (line === 'unborn') unborn = true;
    else if (line.startsWith('ref-prefix ')) prefixes.push(line.slice(11));
  }
  const matches = (name: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
  const parts: Buffer[] = [];
  if (matches('HEAD')) {
    const target = forge.headRef(repo);
    const head = repo.refs.get(target);
    if (head !== undefined) parts.push(pkt(`${head} HEAD${symrefs ? ` symref-target:${target}` : ''}\n`));
    else if (unborn) parts.push(pkt(`unborn HEAD${symrefs ? ` symref-target:${target}` : ''}\n`));
  }
  for (const [name, oid] of sortedRefs(repo)) {
    if (!matches(name)) continue;
    let line = `${oid} ${name}`;
    if (peel) {
      const peeled = forge.peel(oid);
      if (peeled !== oid) line += ` peeled:${peeled}`;
    }
    parts.push(pkt(`${line}\n`));
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

/** POST git-upload-pack: a v2 command, or a v0 want/have exchange. */
export function uploadPack(forge: Forge, repo: Repo, body: Buffer, v2: boolean): GitReply {
  const type = 'application/x-git-upload-pack-result';
  const { packets } = readPackets(body);
  if (v2 || (packets[0]?.kind === 'data' && packetText(packets[0]).startsWith('command='))) {
    let command = '';
    let index = 0;
    for (; index < packets.length; index += 1) {
      const packet = packets[index];
      if (packet.kind !== 'data') break;
      const text = packetText(packet);
      if (text.startsWith('command=')) command = text.slice(8);
    }
    const argLines: string[] = [];
    if (packets[index]?.kind === 'delim') {
      for (index += 1; index < packets.length; index += 1) {
        const packet = packets[index];
        if (packet.kind !== 'data') break;
        argLines.push(packetText(packet));
      }
    }
    if (command === 'ls-refs') return reply(200, type, [lsRefs(forge, repo, argLines)]);
    if (command === 'fetch') {
      const args = newArgs();
      for (const line of argLines) fetchLine(args, line);
      return reply(200, type, [fetchV2(forge, repo, args)]);
    }
    throw new GitError(`unknown command ${JSON.stringify(command)}`);
  }
  // Protocol v0: wants (the first carrying capabilities), shallow/deepen, flush, haves, done.
  const args = newArgs();
  let sawWant = false;
  for (const packet of packets as Packet[]) {
    if (packet.kind !== 'data') continue;
    let line = packetText(packet);
    if (line.startsWith('want ') && !sawWant) {
      sawWant = true;
      const caps = line.slice(46).split(' ');
      args.sideband = caps.includes('side-band-64k') || caps.includes('side-band');
      if (caps.includes('include-tag')) args.includeTag = true;
      line = line.slice(0, 45);
    }
    fetchLine(args, line);
  }
  if (args.wants.length === 0) return reply(200, type, [FLUSH]);
  const plan = planFetch(forge, repo, args);
  const parts: Buffer[] = [];
  if (args.depth !== null || args.deepenSince !== null || args.deepenNot.length > 0) {
    for (const oid of plan.shallow) parts.push(pkt(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pkt(`unshallow ${oid}\n`));
    parts.push(FLUSH);
  }
  parts.push(pkt('NAK\n'));
  const pack = buildPack(forge, plan.objects);
  if (args.sideband) parts.push(...sideband(1, pack), FLUSH);
  else parts.push(pack);
  return reply(200, type, parts);
}

// ---------------------------------------------------------------- receive-pack

interface Command {
  readonly oldOid: string;
  readonly newOid: string;
  readonly ref: string;
}

function validRefName(ref: string): boolean {
  if (!ref.startsWith('refs/') || ref.length > 255) return false;
  if (/[\x00-\x20~^:?*[\\\x7f]/.test(ref)) return false;
  if (ref.includes('..') || ref.includes('//') || ref.endsWith('/') || ref.endsWith('.lock') || ref.includes('@{')) return false;
  return true;
}

/** POST git-receive-pack: unpack, update each ref that fast-forwards, report per ref. */
export function receivePack(forge: Forge, repo: Repo, body: Buffer): GitReply {
  const type = 'application/x-git-receive-pack-result';
  const { packets, end } = readPackets(body, 0, true);
  const commands: Command[] = [];
  let caps: string[] = [];
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    let line = packet.payload.toString('utf8');
    const nul = line.indexOf('\0');
    if (nul >= 0) {
      caps = line.slice(nul + 1).trim().split(' ');
      line = line.slice(0, nul);
    }
    line = line.replace(/\n$/, '');
    if (line.startsWith('shallow ')) continue;
    const [oldOid, newOid, ref] = line.split(' ');
    if (!OID_RE.test(oldOid ?? '') || !OID_RE.test(newOid ?? '') || ref === undefined) throw new GitError(`bad command ${JSON.stringify(line)}`);
    commands.push({ oldOid, newOid, ref });
  }
  const useSideband = caps.includes('side-band-64k') || caps.includes('side-band');
  const wantsReport = caps.includes('report-status') || caps.includes('report-status-v2');
  const atomic = caps.includes('atomic');

  let unpack = 'ok';
  const pack = body.subarray(end);
  if (pack.length > 0) {
    try {
      for (const object of readPack(pack, (oid) => forge.getObject(oid))) forge.putObjectWithId(object.oid, object.type, object.data);
    } catch (err) {
      unpack = err instanceof Error ? err.message.replace(/\s+/g, ' ') : 'unpack failed';
    }
  }

  const hadBranches = [...repo.refs.keys()].some((name) => name.startsWith('refs/heads/'));
  const results: string[] = [];
  const updates: Command[] = [];
  for (const command of commands) {
    const reason = refusal(forge, repo, command, unpack);
    if (reason === null) {
      updates.push(command);
      results.push(`ok ${command.ref}`);
    } else {
      results.push(`ng ${command.ref} ${reason}`);
    }
  }
  if (atomic && updates.length !== commands.length) {
    for (let index = 0; index < results.length; index += 1) {
      if (results[index].startsWith('ok ')) results[index] = `ng ${commands[index].ref} atomic transaction failed`;
    }
  } else {
    for (const command of updates) forge.setRef(repo, command.ref, command.newOid);
  }
  // The first branch pushed to an empty repository becomes its default when the default is absent.
  if (!hadBranches && !repo.refs.has(forge.headRef(repo))) {
    const first = updates.find((command) => command.ref.startsWith('refs/heads/') && command.newOid !== ZERO_OID);
    if (first !== undefined) repo.defaultBranch = first.ref.slice('refs/heads/'.length);
  }

  if (!wantsReport) return reply(200, type, [FLUSH]);
  const report = Buffer.concat([pkt(`unpack ${unpack}\n`), ...results.map((line) => pkt(`${line}\n`)), FLUSH]);
  if (!useSideband) return reply(200, type, [report]);
  return reply(200, type, [...sideband(1, report), FLUSH]);
}

function refusal(forge: Forge, repo: Repo, command: Command, unpack: string): string | null {
  if (unpack !== 'ok') return 'unpacker error';
  if (!validRefName(command.ref)) return 'funny refname';
  const current = repo.refs.get(command.ref) ?? ZERO_OID;
  if (command.oldOid !== current) {
    if (current !== ZERO_OID && command.newOid !== ZERO_OID && command.oldOid !== ZERO_OID && !forge.isAncestor(forge.peel(current), forge.peel(command.newOid))) return 'non-fast-forward';
    return current === ZERO_OID ? 'stale info' : 'fetch first';
  }
  if (command.newOid === ZERO_OID) return null;
  const object = forge.getObject(command.newOid);
  if (object === undefined) return 'missing necessary objects';
  if (command.ref.startsWith('refs/heads/') && forge.peel(command.newOid) !== command.newOid) return 'not a commit';
  if (command.ref.startsWith('refs/heads/') && object.type !== 'commit') return 'not a commit';
  if (current !== ZERO_OID) {
    if (command.ref.startsWith('refs/tags/')) return 'already exists';
    if (!forge.isAncestor(forge.peel(current), forge.peel(command.newOid))) return 'non-fast-forward';
  }
  // Make sure the pushed history is complete enough to serve.
  const commit = forge.commitOf(forge.peel(command.newOid));
  if (commit !== null && forge.getObject(commit.tree) === undefined) return 'missing necessary objects';
  return null;
}
