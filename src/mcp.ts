// The Model Context Protocol endpoint at /mcp: JSON-RPC 2.0 over Streamable HTTP. It speaks the
// 2026-07-28 revision (server/discover, per-request _meta, no session) and the handshake revisions
// (initialize + notifications/initialized, an optional Mcp-Session-Id). Its tools use the
// github-mcp-server names and act on the same store as REST and GraphQL.

import type { IncomingHttpHeaders } from 'node:http';
import { randomUUID } from 'node:crypto';
import { ForgeError } from './store.ts';
import type { Forge, Issue, Pull, Repo } from './store.ts';
import {
  commentJson,
  commitJson,
  createRepoFrom,
  filterIssues,
  issueJson,
  openPull,
  pullFilesJson,
  pullJson,
  repoJson,
  resolveLabels,
  sortIssues,
  writeFile,
} from './rest.ts';
import type { Commit } from './git.ts';

const LATEST = '2026-07-28';
const SUPPORTED = [LATEST, '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const SERVER_INFO = { name: 'git-forge', title: 'git-forge MCP server', version: '1.0.0' };
const INSTRUCTIONS = 'A GitHub-compatible forge. Tools take owner/repo, mirror github-mcp-server, and read and write the same repositories, issues and pull requests as the REST and GraphQL APIs.';

export interface McpRequest {
  readonly method: string;
  readonly body: Buffer;
  readonly authorized: 'ok' | 'missing' | 'bad';
  readonly headers: IncomingHttpHeaders;
  readonly origin: string;
}

export interface McpReply {
  readonly status: number;
  readonly body: string | null;
  readonly headers: Record<string, string>;
}

type Json = Record<string, unknown>;

interface ToolDef {
  readonly name: string;
  readonly title: string;
  readonly description: string;
  readonly properties: Record<string, Json>;
  readonly required: readonly string[];
  readonly readOnly: boolean;
  readonly run: (forge: Forge, args: Json, origin: string) => unknown;
}

class ToolInputError extends Error {}

const sessions = new Set<string>();

// ---------------------------------------------------------------- argument helpers

function isRecord(value: unknown): value is Json {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function text(args: Json, name: string, required = true): string {
  const value = args[name];
  if (typeof value === 'string' && value !== '') return value;
  if (typeof value === 'number') return String(value);
  if (required) throw new ToolInputError(`missing required parameter: ${name}`);
  return '';
}

function optionalText(args: Json, name: string): string | undefined {
  const value = args[name];
  return typeof value === 'string' ? value : typeof value === 'number' ? String(value) : undefined;
}

function integer(args: Json, name: string, required = true): number {
  const value = args[name];
  const number = typeof value === 'number' ? value : typeof value === 'string' && /^\d+$/.test(value.trim()) ? Number(value.trim()) : Number.NaN;
  if (Number.isSafeInteger(number)) return number;
  if (required) throw new ToolInputError(`missing required parameter: ${name} (an integer)`);
  return Number.NaN;
}

/** The repository named by owner + repo (or a full_name "owner/repo"). */
function repoOf(forge: Forge, args: Json): Repo {
  let owner = optionalText(args, 'owner');
  let name = optionalText(args, 'repo');
  const full = optionalText(args, 'full_name');
  if ((owner === undefined || name === undefined) && full !== undefined && full.includes('/')) {
    [owner, name] = full.split('/', 2);
  }
  if (owner === undefined || owner === '') throw new ToolInputError('missing required parameter: owner');
  if (name === undefined || name === '') throw new ToolInputError('missing required parameter: repo');
  const repo = forge.getRepo(owner, name);
  if (repo === undefined) throw new ForgeError(404, `repository ${owner}/${name} not found`);
  return repo;
}

function issueOf(repo: Repo, number: number): Issue {
  const issue = repo.issues.get(number);
  if (issue === undefined) throw new ForgeError(404, `issue #${number} not found in ${repo.owner.login}/${repo.name}`);
  return issue;
}

function pullOf(repo: Repo, number: number): Pull {
  const pull = repo.pulls.get(number);
  if (pull === undefined) throw new ForgeError(404, `pull request #${number} not found in ${repo.owner.login}/${repo.name}`);
  return pull;
}

function page(args: Json): { perPage: number; page: number } {
  const perPage = Math.min(100, Math.max(1, integer(args, 'perPage', false) || integer(args, 'per_page', false) || 30));
  const pageNumber = Math.max(1, integer(args, 'page', false) || 1);
  return { perPage, page: pageNumber };
}

function slicePage<T>(list: readonly T[], args: Json): T[] {
  const { perPage, page: pageNumber } = page(args);
  return list.slice((pageNumber - 1) * perPage, pageNumber * perPage);
}

const ownerRepo = {
  owner: { type: 'string', description: 'Repository owner (the user login)' },
  repo: { type: 'string', description: 'Repository name' },
};
const paging = {
  page: { type: 'number', description: 'Page number for pagination (min 1)', minimum: 1 },
  perPage: { type: 'number', description: 'Results per page for pagination (min 1, max 100)', minimum: 1, maximum: 100 },
};

// ---------------------------------------------------------------- the tools

const TOOLS: ToolDef[] = [
  {
    name: 'get_me',
    title: 'Get my user profile',
    description: 'Get details of the authenticated user: login, id and profile URLs.',
    properties: {},
    required: [],
    readOnly: true,
    run: (forge, _args, origin) => ({ login: forge.viewer.login, id: forge.viewer.id, node_id: forge.viewer.nodeId, name: forge.viewer.name, html_url: `${origin}/${forge.viewer.login}`, type: 'User' }),
  },
  {
    name: 'create_repo',
    title: 'Create repository',
    description: 'Create a new repository owned by the authenticated user.',
    properties: {
      name: { type: 'string', description: 'Repository name' },
      description: { type: 'string', description: 'Repository description' },
      private: { type: 'boolean', description: 'Whether the repository is private' },
      autoInit: { type: 'boolean', description: 'Initialize the repository with a README' },
    },
    required: ['name'],
    readOnly: false,
    run: (forge, args, origin) => {
      const repo = createRepoFrom(forge, { name: text(args, 'name'), description: optionalText(args, 'description') ?? null, private: args.private === true, auto_init: args.autoInit === true });
      return repoJson(forge, origin, repo);
    },
  },
  {
    name: 'get_repo',
    title: 'Get repository',
    description: 'Get a repository by owner and name: its id, full name, visibility, default branch and description.',
    properties: { ...ownerRepo },
    required: ['owner', 'repo'],
    readOnly: true,
    run: (forge, args, origin) => repoJson(forge, origin, repoOf(forge, args)),
  },
  {
    name: 'list_repositories',
    title: 'List repositories',
    description: 'List the repositories the authenticated user owns.',
    properties: { ...paging },
    required: [],
    readOnly: true,
    run: (forge, args, origin) => {
      const repos = forge.reposOf(forge.viewer).sort((a, b) => a.id - b.id);
      return { total_count: repos.length, items: slicePage(repos, args).map((repo) => repoJson(forge, origin, repo)) };
    },
  },
  {
    name: 'create_issue',
    title: 'Create issue',
    description: 'Create a new issue in a repository.',
    properties: {
      ...ownerRepo,
      title: { type: 'string', description: 'Issue title' },
      body: { type: 'string', description: 'Issue body content (Markdown)' },
      labels: { type: 'array', items: { type: 'string' }, description: 'Labels to apply; each must already exist' },
    },
    required: ['owner', 'repo', 'title'],
    readOnly: false,
    run: (forge, args, origin) => {
      const repo = repoOf(forge, args);
      const title = text(args, 'title');
      const resolved = resolveLabels(repo, args.labels);
      if (resolved.unknown.length > 0) throw new ForgeError(422, `label ${resolved.unknown[0]} does not exist`);
      const issue = forge.createIssue(repo, { title, body: optionalText(args, 'body') ?? '', labels: resolved.labels });
      return JSON.parse(issueJson(origin, repo, issue));
    },
  },
  {
    name: 'get_issue',
    title: 'Get issue',
    description: 'Get one issue of a repository by its number.',
    properties: { ...ownerRepo, issue_number: { type: 'number', description: 'The issue number' } },
    required: ['owner', 'repo', 'issue_number'],
    readOnly: true,
    run: (forge, args, origin) => {
      const repo = repoOf(forge, args);
      return JSON.parse(issueJson(origin, repo, issueOf(repo, integer(args, 'issue_number'))));
    },
  },
  {
    name: 'list_issues',
    title: 'List issues',
    description: 'List the issues of a repository, filtered by state and labels and ordered by creation.',
    properties: {
      ...ownerRepo,
      state: { type: 'string', enum: ['open', 'closed', 'all', 'OPEN', 'CLOSED'], description: 'Filter by state (default open)' },
      labels: { type: 'array', items: { type: 'string' }, description: 'Only issues carrying all these labels' },
      direction: { type: 'string', enum: ['asc', 'desc'], description: 'Sort direction (default desc)' },
      ...paging,
    },
    required: ['owner', 'repo'],
    readOnly: true,
    run: (forge, args, origin) => {
      const repo = repoOf(forge, args);
      const state = (optionalText(args, 'state') ?? 'open').toLowerCase();
      const labels = Array.isArray(args.labels) ? args.labels.filter((label): label is string => typeof label === 'string') : [];
      const list = sortIssues(filterIssues(repo, state === 'all' ? 'all' : state === 'closed' ? 'closed' : 'open', labels), 'created', optionalText(args, 'direction') ?? 'desc');
      const issues = slicePage(list, args).map((issue) => JSON.parse(issueJson(origin, repo, issue)) as Json);
      return { total_count: list.length, issues };
    },
  },
  {
    name: 'update_issue',
    title: 'Update issue',
    description: 'Update an issue: its title, body, state or labels.',
    properties: {
      ...ownerRepo,
      issue_number: { type: 'number', description: 'The issue number' },
      title: { type: 'string', description: 'New title' },
      body: { type: 'string', description: 'New body' },
      state: { type: 'string', enum: ['open', 'closed'], description: 'New state' },
      labels: { type: 'array', items: { type: 'string' }, description: 'Replace the labels' },
    },
    required: ['owner', 'repo', 'issue_number'],
    readOnly: false,
    run: (forge, args, origin) => {
      const repo = repoOf(forge, args);
      const issue = issueOf(repo, integer(args, 'issue_number'));
      const title = optionalText(args, 'title');
      if (title !== undefined && title !== '') issue.title = title;
      const body = optionalText(args, 'body');
      if (body !== undefined) issue.body = body;
      if (Array.isArray(args.labels)) {
        const resolved = resolveLabels(repo, args.labels);
        if (resolved.unknown.length > 0) throw new ForgeError(422, `label ${resolved.unknown[0]} does not exist`);
        issue.labels = resolved.labels;
      }
      const state = optionalText(args, 'state');
      if (state === 'open' || state === 'closed') forge.setIssueState(issue, state);
      forge.touchIssue(issue);
      return JSON.parse(issueJson(origin, repo, issue));
    },
  },
  {
    name: 'add_issue_comment',
    title: 'Add issue comment',
    description: 'Add a comment to an issue or pull request.',
    properties: { ...ownerRepo, issue_number: { type: 'number', description: 'The issue or pull request number' }, body: { type: 'string', description: 'Comment content' } },
    required: ['owner', 'repo', 'issue_number', 'body'],
    readOnly: false,
    run: (forge, args, origin) => {
      const repo = repoOf(forge, args);
      const number = integer(args, 'issue_number');
      const target = repo.issues.get(number) ?? repo.pulls.get(number);
      if (target === undefined) throw new ForgeError(404, `issue #${number} not found`);
      return commentJson(origin, repo, forge.addComment(repo, target, text(args, 'body')));
    },
  },
  {
    name: 'list_issue_comments',
    title: 'List issue comments',
    description: 'List the comments on an issue or pull request.',
    properties: { ...ownerRepo, issue_number: { type: 'number', description: 'The issue or pull request number' }, ...paging },
    required: ['owner', 'repo', 'issue_number'],
    readOnly: true,
    run: (forge, args, origin) => {
      const repo = repoOf(forge, args);
      const number = integer(args, 'issue_number');
      const target = repo.issues.get(number) ?? repo.pulls.get(number);
      if (target === undefined) throw new ForgeError(404, `issue #${number} not found`);
      return { total_count: target.comments.length, comments: slicePage(target.comments, args).map((comment) => commentJson(origin, repo, comment)) };
    },
  },
  {
    name: 'create_pull_request',
    title: 'Open new pull request',
    description: 'Create a new pull request from a head branch into a base branch.',
    properties: {
      ...ownerRepo,
      title: { type: 'string', description: 'PR title' },
      body: { type: 'string', description: 'PR description' },
      head: { type: 'string', description: 'Branch containing the changes' },
      base: { type: 'string', description: 'Branch to merge into' },
      draft: { type: 'boolean', description: 'Create as a draft PR' },
    },
    required: ['owner', 'repo', 'title', 'head', 'base'],
    readOnly: false,
    run: (forge, args, origin) => {
      const repo = repoOf(forge, args);
      const pull = openPull(forge, repo, { title: text(args, 'title'), body: optionalText(args, 'body') ?? '', head: text(args, 'head'), base: text(args, 'base'), draft: args.draft === true });
      return pullJson(forge, origin, repo, pull, true);
    },
  },
  {
    name: 'get_pull_request',
    title: 'Get pull request',
    description: 'Get one pull request by its number: state, head and base, mergeability.',
    properties: { ...ownerRepo, pull_number: { type: 'number', description: 'The pull request number' } },
    required: ['owner', 'repo', 'pull_number'],
    readOnly: true,
    run: (forge, args, origin) => {
      const repo = repoOf(forge, args);
      return pullJson(forge, origin, repo, pullOf(repo, integer(args, 'pull_number')), true);
    },
  },
  {
    name: 'list_pull_requests',
    title: 'List pull requests',
    description: 'List the pull requests of a repository by state.',
    properties: {
      ...ownerRepo,
      state: { type: 'string', enum: ['open', 'closed', 'all'], description: 'Filter by state (default open)' },
      head: { type: 'string', description: 'Filter by head branch' },
      base: { type: 'string', description: 'Filter by base branch' },
      ...paging,
    },
    required: ['owner', 'repo'],
    readOnly: true,
    run: (forge, args, origin) => {
      const repo = repoOf(forge, args);
      const state = optionalText(args, 'state') ?? 'open';
      let list = [...repo.pulls.values()].filter((pull) => state === 'all' || pull.state === state);
      const head = optionalText(args, 'head');
      const base = optionalText(args, 'base');
      if (head !== undefined) list = list.filter((pull) => pull.head === head.replace(/^.*:/, ''));
      if (base !== undefined) list = list.filter((pull) => pull.base === base);
      list.sort((a, b) => b.number - a.number);
      return { total_count: list.length, pull_requests: slicePage(list, args).map((pull) => pullJson(forge, origin, repo, pull, false)) };
    },
  },
  {
    name: 'get_pull_request_files',
    title: 'Get pull request files',
    description: 'List the files a pull request changes, with their status and line counts.',
    properties: { ...ownerRepo, pull_number: { type: 'number', description: 'The pull request number' } },
    required: ['owner', 'repo', 'pull_number'],
    readOnly: true,
    run: (forge, args, origin) => {
      const repo = repoOf(forge, args);
      return { files: pullFilesJson(forge, origin, repo, pullOf(repo, integer(args, 'pull_number'))) };
    },
  },
  {
    name: 'merge_pull_request',
    title: 'Merge pull request',
    description: 'Merge a pull request into its base branch (squash by default).',
    properties: {
      ...ownerRepo,
      pull_number: { type: 'number', description: 'The pull request number' },
      merge_method: { type: 'string', enum: ['merge', 'squash', 'rebase'], description: 'Merge method (default squash)' },
      commit_title: { type: 'string', description: 'Title for the merge commit' },
      commit_message: { type: 'string', description: 'Extra detail for the merge commit' },
    },
    required: ['owner', 'repo', 'pull_number'],
    readOnly: false,
    run: (forge, args) => {
      const repo = repoOf(forge, args);
      const pull = pullOf(repo, integer(args, 'pull_number'));
      const sha = forge.mergePull(repo, pull, { method: optionalText(args, 'merge_method') ?? 'squash', title: optionalText(args, 'commit_title'), message: optionalText(args, 'commit_message') });
      return { sha, merged: true, message: 'Pull Request successfully merged', number: pull.number };
    },
  },
  {
    name: 'list_branches',
    title: 'List branches',
    description: 'List the branches of a repository with their tip commits.',
    properties: { ...ownerRepo, ...paging },
    required: ['owner', 'repo'],
    readOnly: true,
    run: (forge, args) => {
      const repo = repoOf(forge, args);
      const branches = [...repo.refs.entries()].filter(([ref]) => ref.startsWith('refs/heads/')).map(([ref, sha]) => ({ name: ref.slice(11), commit: { sha }, protected: false }));
      branches.sort((a, b) => (a.name < b.name ? -1 : 1));
      return { branches: slicePage(branches, args) };
    },
  },
  {
    name: 'create_branch',
    title: 'Create branch',
    description: 'Create a new branch from another branch (the default branch when from_branch is omitted).',
    properties: { ...ownerRepo, branch: { type: 'string', description: 'Name for the new branch' }, from_branch: { type: 'string', description: 'Source branch (defaults to the default branch)' } },
    required: ['owner', 'repo', 'branch'],
    readOnly: false,
    run: (forge, args) => {
      const repo = repoOf(forge, args);
      const branch = text(args, 'branch');
      const from = optionalText(args, 'from_branch') ?? repo.defaultBranch;
      const sha = forge.branchTip(repo, from);
      if (sha === null) throw new ForgeError(404, `branch ${from} not found`);
      if (forge.branchExists(repo, branch)) throw new ForgeError(422, `branch ${branch} already exists`);
      forge.setRef(repo, `refs/heads/${branch}`, sha);
      return { ref: `refs/heads/${branch}`, object: { sha, type: 'commit' } };
    },
  },
  {
    name: 'list_commits',
    title: 'List commits',
    description: 'List the commits of a branch, tag or commit, newest first.',
    properties: { ...ownerRepo, sha: { type: 'string', description: 'Branch, tag or commit SHA to start from (default branch when omitted)' }, ...paging },
    required: ['owner', 'repo'],
    readOnly: true,
    run: (forge, args, origin) => {
      const repo = repoOf(forge, args);
      const start = optionalText(args, 'sha') ?? '';
      const tip = forge.resolve(repo, start);
      if (tip === null) throw new ForgeError(404, `no commit found for ${start === '' ? 'the default branch' : start}`);
      const { perPage, page: pageNumber } = page(args);
      const list = forge.walkCommits([tip], perPage * pageNumber);
      return { commits: list.slice((pageNumber - 1) * perPage).map((oid) => commitJson(forge, origin, repo, oid, forge.commitOf(oid) as Commit)) };
    },
  },
  {
    name: 'get_file_contents',
    title: 'Get file or directory contents',
    description: 'Get the contents of a file (decoded text) or a directory listing at a ref.',
    properties: { ...ownerRepo, path: { type: 'string', description: 'Path to the file or directory (empty for the root)' }, ref: { type: 'string', description: 'Branch, tag or commit (default branch when omitted)' } },
    required: ['owner', 'repo'],
    readOnly: true,
    run: (forge, args) => {
      const repo = repoOf(forge, args);
      const ref = optionalText(args, 'ref') ?? repo.defaultBranch;
      const sha = forge.resolve(repo, ref);
      if (sha === null) throw new ForgeError(404, `ref ${ref} not found`);
      const path = (optionalText(args, 'path') ?? '').replace(/^\/+|\/+$/g, '');
      const entry = path === '' ? { type: 'tree' as const, oid: (forge.commitOf(sha) as Commit).tree, mode: '40000' } : forge.lookupPath(sha, path);
      if (entry === null) throw new ForgeError(404, `${path} not found at ${ref}`);
      if (entry.type === 'tree') {
        const listing = forge.flatten(entry.oid);
        return { type: 'dir', path, entries: [...new Set([...listing.keys()].map((child) => child.split('/')[0]))].sort() };
      }
      const blob = forge.getObject(entry.oid);
      return { type: 'file', path, sha: entry.oid, size: blob?.data.length ?? 0, encoding: 'utf-8', content: blob?.data.toString('utf8') ?? '' };
    },
  },
  {
    name: 'create_or_update_file',
    title: 'Create or update file',
    description: 'Create or update one file on a branch with a commit. Pass the current blob sha to update an existing file.',
    properties: {
      ...ownerRepo,
      path: { type: 'string', description: 'Path of the file' },
      content: { type: 'string', description: 'Content of the file (plain text)' },
      message: { type: 'string', description: 'Commit message' },
      branch: { type: 'string', description: 'Branch to commit to (default branch when omitted)' },
      sha: { type: 'string', description: 'Blob SHA of the file being replaced (required for updates)' },
    },
    required: ['owner', 'repo', 'path', 'content', 'message'],
    readOnly: false,
    run: (forge, args) => {
      const repo = repoOf(forge, args);
      const result = writeFile(forge, repo, {
        path: text(args, 'path'),
        content: Buffer.from(typeof args.content === 'string' ? args.content : '', 'utf8'),
        message: text(args, 'message'),
        branch: optionalText(args, 'branch'),
        sha: optionalText(args, 'sha'),
      });
      return { content: { path: text(args, 'path'), sha: result.blob }, commit: { sha: result.commit } };
    },
  },
  {
    name: 'search_issues',
    title: 'Search issues',
    description: 'Search issues and pull requests of the user\'s repositories by words in their title or body.',
    properties: { query: { type: 'string', description: 'Search words; repo:owner/name and is:open / is:closed qualifiers are understood' }, ...paging },
    required: ['query'],
    readOnly: true,
    run: (forge, args, origin) => {
      const words: string[] = [];
      let repoFilter: string | null = null;
      let state: string | null = null;
      for (const term of text(args, 'query').split(/\s+/).filter((part) => part !== '')) {
        if (term.startsWith('repo:')) repoFilter = term.slice(5).toLowerCase();
        else if (term === 'is:open' || term === 'is:closed') state = term.slice(3);
        else if (!term.includes(':')) words.push(term.toLowerCase());
      }
      const hits: Json[] = [];
      for (const repo of forge.reposOf(forge.viewer)) {
        if (repoFilter !== null && `${repo.owner.login}/${repo.name}`.toLowerCase() !== repoFilter) continue;
        for (const issue of repo.issues.values()) {
          if (state !== null && issue.state !== state) continue;
          if (words.every((word) => issue.title.toLowerCase().includes(word) || issue.body.toLowerCase().includes(word))) hits.push(JSON.parse(issueJson(origin, repo, issue)) as Json);
        }
      }
      return { total_count: hits.length, items: slicePage(hits, args) };
    },
  },
];

/** The tools/list answer, computed once so every call is byte-identical. */
const TOOL_LIST = TOOLS.map((tool) => ({
  name: tool.name,
  title: tool.title,
  description: tool.description,
  inputSchema: { type: 'object', properties: tool.properties, required: [...tool.required], additionalProperties: false },
  annotations: { title: tool.title, readOnlyHint: tool.readOnly, destructiveHint: false, idempotentHint: tool.readOnly, openWorldHint: false },
}));

const TOOLS_BY_NAME = new Map<string, ToolDef>();
for (const tool of TOOLS) TOOLS_BY_NAME.set(tool.name, tool);

/** A tool by exact name, or by a suffix after a server prefix ("github__create_issue"), case-insensitively. */
function findTool(name: string): ToolDef | undefined {
  const exact = TOOLS_BY_NAME.get(name);
  if (exact !== undefined) return exact;
  const lower = name.toLowerCase();
  for (const tool of TOOLS) if (lower === tool.name || lower.endsWith(`__${tool.name}`) || lower.endsWith(`.${tool.name}`) || lower.endsWith(`/${tool.name}`)) return tool;
  return undefined;
}

// ---------------------------------------------------------------- JSON-RPC

interface RpcMessage {
  jsonrpc?: unknown;
  id?: unknown;
  method?: unknown;
  params?: unknown;
}

function rpcError(id: unknown, code: number, message: string, data?: unknown): Json {
  const error: Json = { code, message };
  if (data !== undefined) error.data = data;
  return { jsonrpc: '2.0', id: id === undefined ? null : id, error };
}

function rpcResult(id: unknown, result: unknown): Json {
  return { jsonrpc: '2.0', id, result };
}

function serverMeta(): Json {
  return { 'io.modelcontextprotocol/serverInfo': SERVER_INFO };
}

function toolResult(value: unknown): Json {
  const structured = isRecord(value) ? value : { result: value };
  return { content: [{ type: 'text', text: JSON.stringify(value) }], structuredContent: structured, isError: false };
}

function toolError(message: string): Json {
  return { content: [{ type: 'text', text: message }], isError: true };
}

/** Answer one JSON-RPC message; null for a notification (no id) that needs no answer. */
function dispatch(forge: Forge, message: unknown, origin: string, headers: IncomingHttpHeaders): Json | null {
  if (!isRecord(message)) return rpcError(null, -32600, 'Invalid Request: expected a JSON-RPC object');
  const { id, method, params } = message as RpcMessage;
  const isNotification = id === undefined;
  if (message.jsonrpc !== '2.0' || typeof method !== 'string') return isNotification ? null : rpcError(id, -32600, 'Invalid Request');
  if (id !== undefined && id !== null && typeof id !== 'string' && typeof id !== 'number') return rpcError(null, -32600, 'Invalid Request: bad id');
  const p = isRecord(params) ? params : {};
  if (method.startsWith('notifications/')) return null;
  switch (method) {
    case 'server/discover':
      return rpcResult(id, {
        supportedVersions: SUPPORTED,
        capabilities: { tools: { listChanged: false }, resources: {}, prompts: {}, logging: {} },
        serverInfo: SERVER_INFO,
        instructions: INSTRUCTIONS,
        _meta: serverMeta(),
      });
    case 'initialize': {
      const requested = typeof p.protocolVersion === 'string' ? p.protocolVersion : SUPPORTED[1];
      const version = SUPPORTED.includes(requested) ? requested : SUPPORTED[1];
      return rpcResult(id, { protocolVersion: version, capabilities: { tools: { listChanged: false }, resources: {}, prompts: {}, logging: {} }, serverInfo: SERVER_INFO, instructions: INSTRUCTIONS });
    }
    case 'ping':
      return rpcResult(id, {});
    case 'tools/list':
      return rpcResult(id, { tools: TOOL_LIST });
    case 'resources/list':
      return rpcResult(id, { resources: [] });
    case 'resources/templates/list':
      return rpcResult(id, { resourceTemplates: [] });
    case 'prompts/list':
      return rpcResult(id, { prompts: [] });
    case 'logging/setLevel':
      return rpcResult(id, {});
    case 'completion/complete':
      return rpcResult(id, { completion: { values: [], hasMore: false } });
    case 'tools/call': {
      const name = p.name;
      if (typeof name !== 'string' || name === '') return rpcError(id, -32602, 'Invalid params: tools/call needs a tool name');
      const tool = findTool(name);
      if (tool === undefined) return rpcError(id, -32602, `Unknown tool: ${name}`);
      const args = isRecord(p.arguments) ? p.arguments : {};
      const missing = tool.required.filter((field) => args[field] === undefined || args[field] === null || args[field] === '');
      if (missing.length > 0) return rpcResult(id, toolError(`missing required parameter${missing.length > 1 ? 's' : ''}: ${missing.join(', ')}`));
      try {
        return rpcResult(id, toolResult(tool.run(forge, args, origin)));
      } catch (err) {
        if (err instanceof ToolInputError || err instanceof ForgeError) return rpcResult(id, toolError(err.message));
        throw err;
      }
    }
    default:
      void headers;
      return rpcError(id, -32601, `Method not found: ${method}`);
  }
}

export function handleMcp(forge: Forge, req: McpRequest): McpReply {
  const json = { 'Content-Type': 'application/json' };
  if (req.authorized !== 'ok') {
    return {
      status: 401,
      body: JSON.stringify(rpcError(null, -32001, req.authorized === 'missing' ? 'Unauthorized: a bearer token is required' : 'Unauthorized: bad credentials')),
      headers: { 'WWW-Authenticate': 'Bearer realm="forge", error="invalid_token"' },
    };
  }
  if (req.method === 'DELETE') {
    const session = req.headers['mcp-session-id'];
    if (typeof session === 'string') sessions.delete(session);
    return { status: 204, body: null, headers: {} };
  }
  if (req.method === 'GET') {
    // No server-initiated stream: the transport allows refusing it.
    return { status: 405, body: JSON.stringify(rpcError(null, -32000, 'Method Not Allowed: this server sends no server-initiated messages')), headers: { Allow: 'POST, DELETE' } };
  }
  if (req.method !== 'POST') return { status: 405, body: JSON.stringify(rpcError(null, -32000, 'Method Not Allowed')), headers: { Allow: 'POST, DELETE' } };
  let message: unknown;
  try {
    message = JSON.parse(req.body.toString('utf8'));
  } catch {
    return { status: 400, body: JSON.stringify(rpcError(null, -32700, 'Parse error: the body is not valid JSON')), headers: json };
  }
  const extra: Record<string, string> = {};
  if (Array.isArray(message)) {
    if (message.length === 0) return { status: 400, body: JSON.stringify(rpcError(null, -32600, 'Invalid Request: empty batch')), headers: json };
    const answers = message.map((item) => dispatch(forge, item, req.origin, req.headers)).filter((answer): answer is Json => answer !== null);
    if (answers.length === 0) return { status: 202, body: null, headers: {} };
    return { status: 200, body: JSON.stringify(answers), headers: json };
  }
  if (isRecord(message) && message.method === 'initialize') {
    const session = randomUUID();
    sessions.add(session);
    extra['Mcp-Session-Id'] = session;
  }
  const answer = dispatch(forge, message, req.origin, req.headers);
  if (answer === null) return { status: 202, body: null, headers: extra };
  const status = isRecord(answer.error) && answer.error.code === -32600 ? 400 : 200;
  return { status, body: JSON.stringify(answer), headers: { ...json, ...extra } };
}
