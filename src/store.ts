// The forge's state: users, repositories (refs over a shared content-addressed object store), labels,
// issues, pull requests and comments, plus the git operations the APIs need on top of the objects
// (commits from file changes, ancestry, merge bases, tree diffs and squash merges).

import {
  buildCommit,
  buildTree,
  hashObject,
  isTreeMode,
  packEntry,
  parseCommit,
  parseTag,
  parseTree,
  ZERO_OID,
} from './git.ts';
import type { Commit, GitObject, ObjType, Signature } from './git.ts';

export interface User {
  readonly id: number;
  readonly nodeId: string;
  readonly login: string;
  readonly name: string;
  readonly email: string;
}

export interface Label {
  readonly id: number;
  readonly nodeId: string;
  name: string;
  color: string;
  description: string | null;
}

export interface Comment {
  readonly id: number;
  readonly nodeId: string;
  readonly user: User;
  body: string;
  readonly createdAt: string;
  updatedAt: string;
  /** The issue or pull request number the comment belongs to. */
  readonly number: number;
}

export interface Issue {
  readonly id: number;
  readonly nodeId: string;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  stateReason: string | null;
  labels: Label[];
  readonly user: User;
  readonly createdAt: string;
  updatedAt: string;
  closedAt: string | null;
  comments: Comment[];
  /** A serialized REST view, dropped whenever the issue changes. */
  json: string | null;
}

export interface Pull {
  readonly id: number;
  readonly nodeId: string;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  readonly user: User;
  readonly head: string;
  base: string;
  draft: boolean;
  readonly createdAt: string;
  updatedAt: string;
  closedAt: string | null;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  /** The head and base tips frozen when the pull request closed. */
  frozenHead: string | null;
  frozenBase: string | null;
  labels: Label[];
  comments: Comment[];
}

export interface Repo {
  readonly id: number;
  readonly nodeId: string;
  readonly owner: User;
  name: string;
  description: string | null;
  private: boolean;
  defaultBranch: string;
  hasIssues: boolean;
  readonly createdAt: string;
  updatedAt: string;
  pushedAt: string | null;
  readonly refs: Map<string, string>;
  readonly labels: Map<string, Label>;
  readonly issues: Map<number, Issue>;
  readonly pulls: Map<number, Pull>;
  nextNumber: number;
  /** Bumped on every ref change: fetch caches key on it. */
  version: number;
}

export class ForgeError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/** GitHub's timestamp format: ISO 8601 to the second, UTC. */
export function isoNow(): string {
  return new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');
}

export function isoFromEpoch(seconds: number): string {
  return new Date(seconds * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z');
}

/** An opaque, stable global id in GitHub's prefix_base64 style. */
export function globalId(prefix: string, kind: string, id: number): string {
  return `${prefix}_${Buffer.from(`${kind}:${id}`, 'utf8').toString('base64url')}`;
}

/** The kind and number behind a global id, or null when it is not one of ours. */
export function parseGlobalId(value: string): { kind: string; id: number } | null {
  const under = value.indexOf('_');
  if (under <= 0) return null;
  let text: string;
  try {
    text = Buffer.from(value.slice(under + 1), 'base64url').toString('utf8');
  } catch {
    return null;
  }
  const match = /^([a-z]+):(\d+)$/.exec(text);
  return match ? { kind: match[1], id: Number(match[2]) } : null;
}

export interface FlatEntry {
  readonly mode: string;
  readonly oid: string;
}

export interface FileChange {
  readonly path: string;
  /** New bytes, or null to delete the path. */
  readonly content: Buffer | null;
}

export interface DiffEntry {
  readonly path: string;
  readonly status: 'added' | 'modified' | 'removed';
  readonly oldOid: string | null;
  readonly newOid: string | null;
}

export class Forge {
  readonly viewer: User;
  readonly users = new Map<string, User>();
  /** Every object any repository holds, by id (content addressing makes sharing safe). */
  readonly objects = new Map<string, GitObject>();
  /** Deflated pack entries, built once per object on first fetch. */
  private readonly packEntries = new Map<string, Buffer>();
  /** Repositories keyed by lowercase "owner/name". */
  private readonly repos = new Map<string, Repo>();
  private nextId = 1;
  private readonly reposById = new Map<number, Repo>();
  readonly issuesById = new Map<number, { repo: Repo; issue: Issue }>();
  readonly pullsById = new Map<number, { repo: Repo; pull: Pull }>();
  readonly commentsById = new Map<number, { repo: Repo; comment: Comment }>();

  constructor(login: string) {
    const id = 1000;
    this.viewer = { id, nodeId: globalId('U', 'user', id), login, name: login, email: `${login}@users.noreply.forge` };
    this.users.set(login.toLowerCase(), this.viewer);
  }

  private allocId(): number {
    const id = this.nextId;
    this.nextId += 1;
    return id;
  }

  // ------------------------------------------------------------ objects

  putObject(type: ObjType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!this.objects.has(oid)) this.objects.set(oid, { type, data });
    return oid;
  }

  putObjectWithId(oid: string, type: ObjType, data: Buffer): void {
    if (!this.objects.has(oid)) this.objects.set(oid, { type, data });
  }

  getObject(oid: string): GitObject | undefined {
    return this.objects.get(oid);
  }

  packEntryOf(oid: string): Buffer {
    let entry = this.packEntries.get(oid);
    if (entry === undefined) {
      const object = this.objects.get(oid);
      if (object === undefined) throw new ForgeError(500, `object ${oid} is missing`);
      entry = packEntry(object.type, object.data);
      this.packEntries.set(oid, entry);
    }
    return entry;
  }

  commitOf(oid: string): Commit | null {
    const object = this.objects.get(oid);
    if (object === undefined || object.type !== 'commit') return null;
    return parseCommit(object.data);
  }

  /** Follow annotated tags down to the object they name. */
  peel(oid: string): string {
    let current = oid;
    for (let guard = 0; guard < 100; guard += 1) {
      const object = this.objects.get(current);
      if (object === undefined || object.type !== 'tag') return current;
      current = parseTag(object.data).object;
    }
    return current;
  }

  // ------------------------------------------------------------ repositories

  repoKey(owner: string, name: string): string {
    return `${owner.toLowerCase()}/${name.toLowerCase()}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.repoKey(owner, name));
  }

  repoById(id: number): Repo | undefined {
    return this.reposById.get(id);
  }

  reposOf(owner: User): Repo[] {
    const list: Repo[] = [];
    for (const repo of this.repos.values()) if (repo.owner === owner) list.push(repo);
    return list;
  }

  createRepo(owner: User, name: string, options: { description?: string | null; private?: boolean; defaultBranch?: string; hasIssues?: boolean }): Repo {
    if (this.repos.has(this.repoKey(owner.login, name))) throw new ForgeError(422, 'name already exists on this account');
    const id = this.allocId();
    const now = isoNow();
    const repo: Repo = {
      id,
      nodeId: globalId('R', 'repo', id),
      owner,
      name,
      description: options.description ?? null,
      private: options.private ?? false,
      defaultBranch: options.defaultBranch ?? 'main',
      hasIssues: options.hasIssues ?? true,
      createdAt: now,
      updatedAt: now,
      pushedAt: null,
      refs: new Map(),
      labels: new Map(),
      issues: new Map(),
      pulls: new Map(),
      nextNumber: 1,
      version: 0,
    };
    this.repos.set(this.repoKey(owner.login, name), repo);
    this.reposById.set(id, repo);
    return repo;
  }

  deleteRepo(repo: Repo): void {
    this.repos.delete(this.repoKey(repo.owner.login, repo.name));
    this.reposById.delete(repo.id);
    for (const issue of repo.issues.values()) this.issuesById.delete(issue.id);
    for (const pull of repo.pulls.values()) this.pullsById.delete(pull.id);
  }

  renameRepo(repo: Repo, name: string): void {
    const key = this.repoKey(repo.owner.login, name);
    if (key === this.repoKey(repo.owner.login, repo.name)) {
      repo.name = name;
      return;
    }
    if (this.repos.has(key)) throw new ForgeError(422, 'name already exists on this account');
    this.repos.delete(this.repoKey(repo.owner.login, repo.name));
    repo.name = name;
    this.repos.set(key, repo);
  }

  setRef(repo: Repo, ref: string, oid: string): void {
    if (oid === ZERO_OID) repo.refs.delete(ref);
    else repo.refs.set(ref, oid);
    repo.version += 1;
    const now = isoNow();
    repo.pushedAt = now;
    repo.updatedAt = now;
  }

  /** HEAD's target ref name. */
  headRef(repo: Repo): string {
    return `refs/heads/${repo.defaultBranch}`;
  }

  /** A branch name, a full ref, a tag or a commit id (40 hex, or an unambiguous prefix) to a commit id. */
  resolve(repo: Repo, rev: string): string | null {
    if (rev === '' || rev === 'HEAD') return repo.refs.get(this.headRef(repo)) ?? null;
    const candidates = rev.startsWith('refs/') ? [rev] : [`refs/heads/${rev}`, `refs/tags/${rev}`];
    for (const name of candidates) {
      const oid = repo.refs.get(name);
      if (oid !== undefined) return this.peel(oid);
    }
    const lower = rev.toLowerCase();
    if (/^[0-9a-f]{40}$/.test(lower) && this.objects.has(lower)) return this.peel(lower);
    if (/^[0-9a-f]{7,39}$/.test(lower)) {
      // Search the repository's history for a unique prefix.
      const tips = [...repo.refs.values()].map((oid) => this.peel(oid));
      let found: string | null = null;
      for (const oid of this.walkCommits(tips, 100_000)) {
        if (oid.startsWith(lower)) {
          if (found !== null && found !== oid) return null;
          found = oid;
        }
      }
      return found;
    }
    return null;
  }

  branchExists(repo: Repo, branch: string): boolean {
    return repo.refs.has(`refs/heads/${branch}`);
  }

  branchTip(repo: Repo, branch: string): string | null {
    return repo.refs.get(`refs/heads/${branch}`) ?? null;
  }

  // ------------------------------------------------------------ history

  /** Commit ids reachable from `tips`, newest committer time first (ties by discovery). */
  walkCommits(tips: readonly string[], limit: number): string[] {
    const seen = new Set<string>();
    const queue: Array<{ oid: string; time: number }> = [];
    const push = (oid: string): void => {
      if (seen.has(oid)) return;
      const commit = this.commitOf(oid);
      if (commit === null) return;
      seen.add(oid);
      queue.push({ oid, time: commit.committer.time });
    };
    for (const tip of tips) push(tip);
    const out: string[] = [];
    while (queue.length > 0 && out.length < limit) {
      let best = 0;
      for (let index = 1; index < queue.length; index += 1) if (queue[index].time > queue[best].time) best = index;
      const [next] = queue.splice(best, 1);
      out.push(next.oid);
      const commit = this.commitOf(next.oid);
      if (commit !== null) for (const parent of commit.parents) push(parent);
    }
    return out;
  }

  /** Whether `ancestor` is `descendant` or one of its ancestors. */
  isAncestor(ancestor: string, descendant: string): boolean {
    if (ancestor === descendant) return true;
    const seen = new Set<string>([descendant]);
    const stack = [descendant];
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      const commit = this.commitOf(oid);
      if (commit === null) continue;
      for (const parent of commit.parents) {
        if (parent === ancestor) return true;
        if (!seen.has(parent)) {
          seen.add(parent);
          stack.push(parent);
        }
      }
    }
    return false;
  }

  /** A best common ancestor of two commits, or null when their histories never meet. */
  mergeBase(a: string, b: string): string | null {
    const fromA = new Set<string>();
    const stack = [a];
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (fromA.has(oid)) continue;
      fromA.add(oid);
      const commit = this.commitOf(oid);
      if (commit !== null) stack.push(...commit.parents);
    }
    // Breadth-first from b by committer time: the first shared commit is the nearest.
    for (const oid of this.walkCommits([b], 1_000_000)) if (fromA.has(oid)) return oid;
    return null;
  }

  // ------------------------------------------------------------ trees

  /** Every blob path under a tree (directories flattened away). */
  flatten(treeOid: string | null): Map<string, FlatEntry> {
    const out = new Map<string, FlatEntry>();
    if (treeOid === null) return out;
    const walk = (oid: string, prefix: string): void => {
      const object = this.objects.get(oid);
      if (object === undefined || object.type !== 'tree') return;
      for (const entry of parseTree(object.data)) {
        const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
        if (isTreeMode(entry.mode)) walk(entry.oid, path);
        else out.set(path, { mode: entry.mode, oid: entry.oid });
      }
    };
    walk(treeOid, '');
    return out;
  }

  /** Write the trees for a flat path map and answer the root tree id. */
  writeTree(files: ReadonlyMap<string, FlatEntry>): string {
    interface Dir {
      files: Map<string, FlatEntry>;
      dirs: Map<string, Dir>;
    }
    const make = (): Dir => ({ files: new Map(), dirs: new Map() });
    const root = make();
    for (const [path, entry] of files) {
      const parts = path.split('/');
      let dir = root;
      for (let index = 0; index < parts.length - 1; index += 1) {
        let child = dir.dirs.get(parts[index]);
        if (child === undefined) {
          child = make();
          dir.dirs.set(parts[index], child);
        }
        dir = child;
      }
      dir.files.set(parts[parts.length - 1], entry);
    }
    const write = (dir: Dir): string => {
      const entries = [];
      for (const [name, entry] of dir.files) entries.push({ mode: entry.mode, name, oid: entry.oid });
      for (const [name, child] of dir.dirs) entries.push({ mode: '40000', name, oid: write(child) });
      return this.putObject('tree', buildTree(entries));
    };
    return write(root);
  }

  /** The entry at `path` in a commit's tree: a blob, a tree, or null. */
  lookupPath(commitOid: string, path: string): { type: 'blob' | 'tree'; mode: string; oid: string } | null {
    const commit = this.commitOf(commitOid);
    if (commit === null) return null;
    const parts = path.split('/').filter((part) => part !== '');
    let oid = commit.tree;
    let mode = '40000';
    for (const part of parts) {
      const object = this.objects.get(oid);
      if (object === undefined || object.type !== 'tree') return null;
      const entry = parseTree(object.data).find((candidate) => candidate.name === part);
      if (entry === undefined) return null;
      oid = entry.oid;
      mode = entry.mode;
    }
    return { type: isTreeMode(mode) ? 'tree' : 'blob', mode, oid };
  }

  signature(user: User): Signature {
    return { name: user.name, email: user.email, time: Math.floor(Date.now() / 1000), tz: '+0000' };
  }

  /** Commit `changes` on top of `parent` (null for a root commit) and answer the new commit id. */
  commitChanges(parent: string | null, changes: readonly FileChange[], message: string, author: Signature, committer: Signature): string {
    const base = parent === null ? null : this.commitOf(parent);
    const files = this.flatten(base === null ? null : base.tree);
    for (const change of changes) {
      if (change.content === null) files.delete(change.path);
      else files.set(change.path, { mode: '100644', oid: this.putObject('blob', change.content) });
    }
    const tree = this.writeTree(files);
    const data = buildCommit({ tree, parents: parent === null ? [] : [parent], author, committer, message });
    return this.putObject('commit', data);
  }

  /** Paths that differ between two trees. */
  diff(oldTree: string | null, newTree: string | null): DiffEntry[] {
    const before = this.flatten(oldTree);
    const after = this.flatten(newTree);
    const out: DiffEntry[] = [];
    for (const [path, entry] of after) {
      const old = before.get(path);
      if (old === undefined) out.push({ path, status: 'added', oldOid: null, newOid: entry.oid });
      else if (old.oid !== entry.oid || old.mode !== entry.mode) out.push({ path, status: 'modified', oldOid: old.oid, newOid: entry.oid });
    }
    for (const [path, entry] of before) if (!after.has(path)) out.push({ path, status: 'removed', oldOid: entry.oid, newOid: null });
    out.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
    return out;
  }

  /** The three-way merge of `ours` and `theirs` over `base`, or null on a conflict. */
  mergeTrees(base: string | null, ours: string, theirs: string): string | null {
    const b = this.flatten(base);
    const o = this.flatten(ours);
    const t = this.flatten(theirs);
    const paths = new Set<string>([...b.keys(), ...o.keys(), ...t.keys()]);
    const out = new Map<string, FlatEntry>();
    const same = (x: FlatEntry | undefined, y: FlatEntry | undefined): boolean =>
      x === undefined ? y === undefined : y !== undefined && x.oid === y.oid && x.mode === y.mode;
    for (const path of paths) {
      const be = b.get(path);
      const oe = o.get(path);
      const te = t.get(path);
      let pick: FlatEntry | undefined;
      if (same(oe, te)) pick = oe;
      else if (same(be, oe)) pick = te;
      else if (same(be, te)) pick = oe;
      else return null;
      if (pick !== undefined) out.set(path, pick);
    }
    return this.writeTree(out);
  }

  // ------------------------------------------------------------ issues, pulls, comments

  allocNumber(repo: Repo): number {
    const number = repo.nextNumber;
    repo.nextNumber += 1;
    return number;
  }

  createLabel(repo: Repo, name: string, color: string, description: string | null): Label {
    if (repo.labels.has(name.toLowerCase())) throw new ForgeError(422, 'Label already exists');
    const id = this.allocId();
    const label: Label = { id, nodeId: globalId('LA', 'label', id), name, color, description };
    repo.labels.set(name.toLowerCase(), label);
    return label;
  }

  createIssue(repo: Repo, input: { title: string; body: string; labels: Label[] }): Issue {
    const id = this.allocId();
    const now = isoNow();
    const issue: Issue = {
      id,
      nodeId: globalId('I', 'issue', id),
      number: this.allocNumber(repo),
      title: input.title,
      body: input.body,
      state: 'open',
      stateReason: null,
      labels: input.labels,
      user: this.viewer,
      createdAt: now,
      updatedAt: now,
      closedAt: null,
      comments: [],
      json: null,
    };
    repo.issues.set(issue.number, issue);
    this.issuesById.set(id, { repo, issue });
    repo.updatedAt = now;
    return issue;
  }

  setIssueState(issue: Issue, state: 'open' | 'closed', reason: string | null = null): void {
    if (issue.state !== state) {
      issue.state = state;
      issue.closedAt = state === 'closed' ? isoNow() : null;
      issue.stateReason = state === 'closed' ? (reason ?? 'completed') : reason === 'reopened' ? 'reopened' : null;
    }
    this.touchIssue(issue);
  }

  touchIssue(issue: Issue): void {
    issue.updatedAt = isoNow();
    issue.json = null;
  }

  addComment(repo: Repo, target: Issue | Pull, body: string): Comment {
    const id = this.allocId();
    const now = isoNow();
    const comment: Comment = { id, nodeId: globalId('IC', 'comment', id), user: this.viewer, body, createdAt: now, updatedAt: now, number: target.number };
    target.comments.push(comment);
    target.updatedAt = now;
    if ('json' in target) target.json = null;
    this.commentsById.set(id, { repo, comment });
    return comment;
  }

  createPull(repo: Repo, input: { title: string; body: string; head: string; base: string; draft: boolean }): Pull {
    const id = this.allocId();
    const now = isoNow();
    const pull: Pull = {
      id,
      nodeId: globalId('PR', 'pull', id),
      number: this.allocNumber(repo),
      title: input.title,
      body: input.body,
      state: 'open',
      user: this.viewer,
      head: input.head,
      base: input.base,
      draft: input.draft,
      createdAt: now,
      updatedAt: now,
      closedAt: null,
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      frozenHead: null,
      frozenBase: null,
      labels: [],
      comments: [],
    };
    repo.pulls.set(pull.number, pull);
    this.pullsById.set(id, { repo, pull });
    repo.updatedAt = now;
    return pull;
  }

  pullHeadSha(repo: Repo, pull: Pull): string | null {
    return pull.frozenHead ?? this.branchTip(repo, pull.head);
  }

  pullBaseSha(repo: Repo, pull: Pull): string | null {
    return pull.frozenBase ?? this.branchTip(repo, pull.base);
  }

  /** The changed paths of a pull request: its head against the merge base with its base branch. */
  pullDiff(repo: Repo, pull: Pull): DiffEntry[] {
    const head = this.pullHeadSha(repo, pull);
    const base = this.pullBaseSha(repo, pull);
    if (head === null) return [];
    const mergeBase = pull.merged && pull.frozenBase !== null ? pull.frozenBase : base === null ? null : this.mergeBase(base, head);
    const headCommit = this.commitOf(head);
    const baseCommit = mergeBase === null ? null : this.commitOf(mergeBase);
    return this.diff(baseCommit === null ? null : baseCommit.tree, headCommit === null ? null : headCommit.tree);
  }

  /** Whether the pull request merges cleanly into its base right now. */
  mergeable(repo: Repo, pull: Pull): boolean | null {
    if (pull.state !== 'open') return null;
    return this.mergePlan(repo, pull) !== null;
  }

  private mergePlan(repo: Repo, pull: Pull): { base: string; tree: string } | null {
    const head = this.branchTip(repo, pull.head);
    const base = this.branchTip(repo, pull.base);
    if (head === null || base === null) return null;
    const headCommit = this.commitOf(head);
    const baseCommit = this.commitOf(base);
    if (headCommit === null || baseCommit === null) return null;
    const mergeBase = this.mergeBase(base, head);
    const mergeBaseCommit = mergeBase === null ? null : this.commitOf(mergeBase);
    const tree = this.mergeTrees(mergeBaseCommit === null ? null : mergeBaseCommit.tree, baseCommit.tree, headCommit.tree);
    return tree === null ? null : { base, tree };
  }

  /** Squash-merge (or merge-commit) a pull request into its base branch. Throws 405 when it cannot. */
  mergePull(repo: Repo, pull: Pull, options: { method: string; title?: string; message?: string; sha?: string }): string {
    if (pull.state !== 'open') throw new ForgeError(405, 'Pull Request is not mergeable');
    const head = this.branchTip(repo, pull.head);
    if (head === null) throw new ForgeError(405, 'Head branch was deleted');
    if (options.sha !== undefined && options.sha !== '' && options.sha !== head) {
      throw new ForgeError(409, 'Head branch was modified. Review and try the merge again.');
    }
    const plan = this.mergePlan(repo, pull);
    if (plan === null) throw new ForgeError(405, 'Pull Request is not mergeable: merge conflict');
    const signature = this.signature(this.viewer);
    const title = options.title !== undefined && options.title !== '' ? options.title : `${pull.title} (#${pull.number})`;
    const message = options.message !== undefined && options.message !== '' ? `${title}\n\n${options.message}` : title;
    const parents = options.method === 'merge' ? [plan.base, head] : [plan.base];
    const commit = this.putObject('commit', buildCommit({ tree: plan.tree, parents, author: signature, committer: signature, message }));
    this.setRef(repo, `refs/heads/${pull.base}`, commit);
    const now = isoNow();
    pull.frozenHead = head;
    pull.frozenBase = plan.base;
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = now;
    pull.closedAt = now;
    pull.updatedAt = now;
    pull.mergeCommitSha = commit;
    return commit;
  }

  closePull(repo: Repo, pull: Pull): void {
    if (pull.state === 'closed') return;
    pull.frozenHead = this.branchTip(repo, pull.head);
    pull.frozenBase = this.branchTip(repo, pull.base);
    pull.state = 'closed';
    pull.closedAt = isoNow();
    pull.updatedAt = pull.closedAt;
  }

  reopenPull(pull: Pull): void {
    if (pull.state === 'open' || pull.merged) return;
    pull.state = 'open';
    pull.closedAt = null;
    pull.frozenHead = null;
    pull.frozenBase = null;
    pull.updatedAt = isoNow();
  }
}

/** Line-level change counts between two texts (Myers' greedy diff on lines). */
export function lineStats(before: Buffer | null, after: Buffer | null): { additions: number; deletions: number } {
  const split = (buf: Buffer | null): string[] => {
    if (buf === null || buf.length === 0) return [];
    const text = buf.toString('utf8');
    const lines = text.split('\n');
    if (lines[lines.length - 1] === '') lines.pop();
    return lines;
  };
  const a = split(before);
  const b = split(after);
  const n = a.length;
  const m = b.length;
  if (n === 0 || m === 0) return { additions: m, deletions: n };
  const max = n + m;
  const limit = Math.min(max, 4000);
  const offset = max + 1;
  const v = new Int32Array(2 * max + 3);
  for (let d = 0; d <= limit; d += 1) {
    for (let k = -d; k <= d; k += 2) {
      let x = k === -d || (k !== d && v[offset + k - 1] < v[offset + k + 1]) ? v[offset + k + 1] : v[offset + k - 1] + 1;
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) {
        x += 1;
        y += 1;
      }
      v[offset + k] = x;
      if (x >= n && y >= m) {
        // d edits; the common length is (n + m - d) / 2.
        const common = (n + m - d) / 2;
        return { additions: m - common, deletions: n - common };
      }
    }
  }
  return { additions: m, deletions: n };
}
