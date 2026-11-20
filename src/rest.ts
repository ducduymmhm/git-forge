// The REST v3 surface under /api/v3: repositories, contents, branches, commits, the git data refs,
// labels, issues, comments and pull requests, in GitHub's JSON shapes.

import { parseCommit, parseTree, ZERO_OID, isTreeMode } from './git.ts';
import type { Commit } from './git.ts';
import { ForgeError, isoFromEpoch, lineStats } from './store.ts';
import type { Comment, Forge, Issue, Label, Pull, Repo, User } from './store.ts';

export interface ApiRequest {
  readonly method: string;
  /** Path segments after /api/v3, URL-decoded. */
  readonly segments: readonly string[];
  readonly query: URLSearchParams;
  /** The parsed JSON body (undefined when there was none). */
  readonly body: unknown;
  /** The forge's origin as the client reached it, e.g. http://10.0.0.5:4312 */
  readonly origin: string;
}

export interface ApiResponse {
  readonly status: number;
  /** A JSON value, a pre-serialized JSON string, or undefined for no body. */
  readonly body?: unknown;
  readonly headers?: Record<string, string>;
}

type Json = Record<string, unknown>;

const DOCS = 'https://docs.github.com/rest';

export function notFound(message = 'Not Found'): ApiResponse {
  return { status: 404, body: { message, documentation_url: DOCS, status: '404' } };
}

function error(status: number, message: string, errors?: unknown[]): ApiResponse {
  const body: Json = { message, documentation_url: DOCS, status: String(status) };
  if (errors !== undefined) body.errors = errors;
  return { status, body };
}

function invalid(resource: string, field: string, code = 'invalid', extra: Json = {}): ApiResponse {
  return error(422, 'Validation Failed', [{ resource, field, code, ...extra }]);
}

function ok(body: unknown, status = 200, headers?: Record<string, string>): ApiResponse {
  return { status, body, headers };
}

function isRecord(value: unknown): value is Json {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function bodyOf(req: ApiRequest): Json {
  return isRecord(req.body) ? req.body : {};
}

function str(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

// ---------------------------------------------------------------- JSON shapes

export function apiRoot(origin: string): string {
  return `${origin}/api/v3`;
}

export function userJson(origin: string, user: User): Json {
  const api = apiRoot(origin);
  return {
    login: user.login,
    id: user.id,
    node_id: user.nodeId,
    avatar_url: `${origin}/avatars/${user.login}`,
    gravatar_id: '',
    url: `${api}/users/${user.login}`,
    html_url: `${origin}/${user.login}`,
    repos_url: `${api}/users/${user.login}/repos`,
    type: 'User',
    user_view_type: 'public',
    site_admin: false,
  };
}

export function repoJson(forge: Forge, origin: string, repo: Repo): Json {
  const api = apiRoot(origin);
  const full = `${repo.owner.login}/${repo.name}`;
  const url = `${api}/repos/${full}`;
  let openIssues = 0;
  for (const issue of repo.issues.values()) if (issue.state === 'open') openIssues += 1;
  for (const pull of repo.pulls.values()) if (pull.state === 'open') openIssues += 1;
  return {
    id: repo.id,
    node_id: repo.nodeId,
    name: repo.name,
    full_name: full,
    private: repo.private,
    owner: userJson(origin, repo.owner),
    html_url: `${origin}/${full}`,
    description: repo.description,
    fork: false,
    url,
    branches_url: `${url}/branches{/branch}`,
    commits_url: `${url}/commits{/sha}`,
    contents_url: `${url}/contents/{+path}`,
    issues_url: `${url}/issues{/number}`,
    labels_url: `${url}/labels{/name}`,
    pulls_url: `${url}/pulls{/number}`,
    git_refs_url: `${url}/git/refs{/sha}`,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    pushed_at: repo.pushedAt ?? repo.createdAt,
    git_url: `${origin}/${full}.git`,
    clone_url: `${origin}/${full}.git`,
    svn_url: `${origin}/${full}`,
    homepage: null,
    size: 0,
    stargazers_count: 0,
    watchers_count: 0,
    language: null,
    has_issues: repo.hasIssues,
    has_projects: false,
    has_downloads: true,
    has_wiki: false,
    has_pages: false,
    has_discussions: false,
    forks_count: 0,
    mirror_url: null,
    archived: false,
    disabled: false,
    open_issues_count: openIssues,
    license: null,
    allow_forking: true,
    is_template: false,
    web_commit_signoff_required: false,
    topics: [],
    visibility: repo.private ? 'private' : 'public',
    forks: 0,
    open_issues: openIssues,
    watchers: 0,
    default_branch: repo.defaultBranch,
    permissions: { admin: true, maintain: true, push: true, triage: true, pull: true },
    allow_squash_merge: true,
    allow_merge_commit: true,
    allow_rebase_merge: true,
    allow_auto_merge: false,
    delete_branch_on_merge: false,
    empty: repo.refs.size === 0,
  };
}

export function labelJson(origin: string, repo: Repo, label: Label): Json {
  return {
    id: label.id,
    node_id: label.nodeId,
    url: `${apiRoot(origin)}/repos/${repo.owner.login}/${repo.name}/labels/${encodeURIComponent(label.name)}`,
    name: label.name,
    color: label.color,
    default: false,
    description: label.description,
  };
}

export function issueJson(origin: string, repo: Repo, issue: Issue): string {
  const cached = issue.json;
  if (cached !== null && cached.startsWith(`${origin}\n`)) return cached.slice(origin.length + 1);
  const url = `${apiRoot(origin)}/repos/${repo.owner.login}/${repo.name}/issues/${issue.number}`;
  const text = JSON.stringify({
    url,
    repository_url: `${apiRoot(origin)}/repos/${repo.owner.login}/${repo.name}`,
    labels_url: `${url}/labels{/name}`,
    comments_url: `${url}/comments`,
    events_url: `${url}/events`,
    html_url: `${origin}/${repo.owner.login}/${repo.name}/issues/${issue.number}`,
    id: issue.id,
    node_id: issue.nodeId,
    number: issue.number,
    title: issue.title,
    user: userJson(origin, issue.user),
    labels: issue.labels.map((label) => labelJson(origin, repo, label)),
    state: issue.state,
    locked: false,
    assignee: null,
    assignees: [],
    milestone: null,
    comments: issue.comments.length,
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    closed_at: issue.closedAt,
    author_association: 'OWNER',
    type: null,
    active_lock_reason: null,
    sub_issues_summary: { total: 0, completed: 0, percent_completed: 0 },
    body: issue.body,
    closed_by: issue.state === 'closed' ? userJson(origin, issue.user) : null,
    timeline_url: `${url}/timeline`,
    performed_via_github_app: null,
    state_reason: issue.stateReason,
  });
  issue.json = `${origin}\n${text}`;
  return text;
}

/** A pull request seen through the issues API (GitHub serves both from one number space). */
function pullAsIssueJson(origin: string, repo: Repo, pull: Pull): Json {
  const url = `${apiRoot(origin)}/repos/${repo.owner.login}/${repo.name}/issues/${pull.number}`;
  return {
    url,
    repository_url: `${apiRoot(origin)}/repos/${repo.owner.login}/${repo.name}`,
    comments_url: `${url}/comments`,
    html_url: `${origin}/${repo.owner.login}/${repo.name}/pull/${pull.number}`,
    id: pull.id,
    node_id: pull.nodeId,
    number: pull.number,
    title: pull.title,
    user: userJson(origin, pull.user),
    labels: pull.labels.map((label) => labelJson(origin, repo, label)),
    state: pull.state,
    locked: false,
    assignee: null,
    assignees: [],
    milestone: null,
    comments: pull.comments.length,
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    closed_at: pull.closedAt,
    author_association: 'OWNER',
    body: pull.body,
    pull_request: {
      url: `${apiRoot(origin)}/repos/${repo.owner.login}/${repo.name}/pulls/${pull.number}`,
      html_url: `${origin}/${repo.owner.login}/${repo.name}/pull/${pull.number}`,
      merged_at: pull.mergedAt,
    },
    state_reason: null,
  };
}

export function commentJson(origin: string, repo: Repo, comment: Comment): Json {
  const api = `${apiRoot(origin)}/repos/${repo.owner.login}/${repo.name}`;
  return {
    url: `${api}/issues/comments/${comment.id}`,
    html_url: `${origin}/${repo.owner.login}/${repo.name}/issues/${comment.number}#issuecomment-${comment.id}`,
    issue_url: `${api}/issues/${comment.number}`,
    id: comment.id,
    node_id: comment.nodeId,
    user: userJson(origin, comment.user),
    created_at: comment.createdAt,
    updated_at: comment.updatedAt,
    author_association: 'OWNER',
    body: comment.body,
    performed_via_github_app: null,
  };
}

function trimMessage(message: string): string {
  return message.endsWith('\n') ? message.slice(0, -1) : message;
}

function signatureJson(sig: Commit['author']): Json {
  return { name: sig.name, email: sig.email, date: isoFromEpoch(sig.time) };
}

/** The git data view of a commit (contents API, git/commits). */
export function gitCommitJson(origin: string, repo: Repo, sha: string, commit: Commit): Json {
  const api = `${apiRoot(origin)}/repos/${repo.owner.login}/${repo.name}`;
  return {
    sha,
    node_id: `C_${Buffer.from(`commit:${sha}`).toString('base64url')}`,
    url: `${api}/git/commits/${sha}`,
    html_url: `${origin}/${repo.owner.login}/${repo.name}/commit/${sha}`,
    author: signatureJson(commit.author),
    committer: signatureJson(commit.committer),
    tree: { sha: commit.tree, url: `${api}/git/trees/${commit.tree}` },
    message: trimMessage(commit.message),
    parents: commit.parents.map((parent) => ({ sha: parent, url: `${api}/git/commits/${parent}`, html_url: `${origin}/${repo.owner.login}/${repo.name}/commit/${parent}` })),
    verification: { verified: false, reason: 'unsigned', signature: null, payload: null, verified_at: null },
  };
}

/** The repos/commits view of a commit. */
export function commitJson(forge: Forge, origin: string, repo: Repo, sha: string, commit: Commit): Json {
  const api = `${apiRoot(origin)}/repos/${repo.owner.login}/${repo.name}`;
  const author = commit.author.email === forge.viewer.email ? userJson(origin, forge.viewer) : null;
  const committer = commit.committer.email === forge.viewer.email ? userJson(origin, forge.viewer) : null;
  return {
    sha,
    node_id: `C_${Buffer.from(`commit:${sha}`).toString('base64url')}`,
    commit: {
      author: signatureJson(commit.author),
      committer: signatureJson(commit.committer),
      message: trimMessage(commit.message),
      tree: { sha: commit.tree, url: `${api}/git/trees/${commit.tree}` },
      url: `${api}/git/commits/${sha}`,
      comment_count: 0,
      verification: { verified: false, reason: 'unsigned', signature: null, payload: null, verified_at: null },
    },
    url: `${api}/commits/${sha}`,
    html_url: `${origin}/${repo.owner.login}/${repo.name}/commit/${sha}`,
    comments_url: `${api}/commits/${sha}/comments`,
    author,
    committer,
    parents: commit.parents.map((parent) => ({ sha: parent, url: `${api}/commits/${parent}`, html_url: `${origin}/${repo.owner.login}/${repo.name}/commit/${parent}` })),
  };
}

function branchRef(forge: Forge, origin: string, repo: Repo, branch: string, sha: string): Json {
  return {
    label: `${repo.owner.login}:${branch}`,
    ref: branch,
    sha,
    user: userJson(origin, repo.owner),
    repo: repoJson(forge, origin, repo),
  };
}

export function pullJson(forge: Forge, origin: string, repo: Repo, pull: Pull, full: boolean): Json {
  const api = `${apiRoot(origin)}/repos/${repo.owner.login}/${repo.name}`;
  const url = `${api}/pulls/${pull.number}`;
  const html = `${origin}/${repo.owner.login}/${repo.name}/pull/${pull.number}`;
  const headSha = forge.pullHeadSha(repo, pull) ?? ZERO_OID;
  const baseSha = forge.pullBaseSha(repo, pull) ?? ZERO_OID;
  const out: Json = {
    url,
    id: pull.id,
    node_id: pull.nodeId,
    html_url: html,
    diff_url: `${html}.diff`,
    patch_url: `${html}.patch`,
    issue_url: `${api}/issues/${pull.number}`,
    commits_url: `${url}/commits`,
    review_comments_url: `${url}/comments`,
    comments_url: `${api}/issues/${pull.number}/comments`,
    statuses_url: `${api}/statuses/${headSha}`,
    number: pull.number,
    state: pull.state,
    locked: false,
    title: pull.title,
    user: userJson(origin, pull.user),
    body: pull.body,
    labels: pull.labels.map((label) => labelJson(origin, repo, label)),
    milestone: null,
    active_lock_reason: null,
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    closed_at: pull.closedAt,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    assignee: null,
    assignees: [],
    requested_reviewers: [],
    requested_teams: [],
    draft: pull.draft,
    head: branchRef(forge, origin, repo, pull.head, headSha),
    base: branchRef(forge, origin, repo, pull.base, baseSha),
    author_association: 'OWNER',
    auto_merge: null,
  };
  if (full) {
    const files = forge.pullDiff(repo, pull);
    let additions = 0;
    let deletions = 0;
    for (const file of files) {
      const stats = fileStats(forge, file.oldOid, file.newOid);
      additions += stats.additions;
      deletions += stats.deletions;
    }
    const mergeable = forge.mergeable(repo, pull);
    out.merged = pull.merged;
    out.mergeable = mergeable;
    out.rebaseable = mergeable;
    out.mergeable_state = pull.state === 'closed' ? 'unknown' : mergeable === true ? 'clean' : 'dirty';
    out.merged_by = pull.merged ? userJson(origin, forge.viewer) : null;
    out.comments = pull.comments.length;
    out.review_comments = 0;
    out.maintainer_can_modify = false;
    out.commits = pullCommits(forge, repo, pull).length;
    out.additions = additions;
    out.deletions = deletions;
    out.changed_files = files.length;
  } else {
    out.merged = pull.merged;
  }
  return out;
}

function fileStats(forge: Forge, oldOid: string | null, newOid: string | null): { additions: number; deletions: number } {
  const before = oldOid === null ? null : forge.getObject(oldOid)?.data ?? null;
  const after = newOid === null ? null : forge.getObject(newOid)?.data ?? null;
  return lineStats(before, after);
}

/** The commits a pull request carries: on its head and not on its base (oldest first). */
export function pullCommits(forge: Forge, repo: Repo, pull: Pull): string[] {
  const head = forge.pullHeadSha(repo, pull);
  if (head === null) return [];
  const base = forge.pullBaseSha(repo, pull);
  const onBase = new Set(base === null ? [] : forge.walkCommits([base], 1_000_000));
  const out: string[] = [];
  for (const oid of forge.walkCommits([head], 10_000)) if (!onBase.has(oid)) out.push(oid);
  return out.reverse();
}

export function pullFilesJson(forge: Forge, origin: string, repo: Repo, pull: Pull): Json[] {
  const api = `${apiRoot(origin)}/repos/${repo.owner.login}/${repo.name}`;
  const head = forge.pullHeadSha(repo, pull) ?? ZERO_OID;
  return forge.pullDiff(repo, pull).map((file) => {
    const stats = fileStats(forge, file.oldOid, file.newOid);
    return {
      sha: file.newOid ?? file.oldOid,
      filename: file.path,
      status: file.status,
      additions: stats.additions,
      deletions: stats.deletions,
      changes: stats.additions + stats.deletions,
      blob_url: `${origin}/${repo.owner.login}/${repo.name}/blob/${head}/${file.path}`,
      raw_url: `${origin}/${repo.owner.login}/${repo.name}/raw/${head}/${file.path}`,
      contents_url: `${api}/contents/${file.path}?ref=${head}`,
    };
  });
}

// ---------------------------------------------------------------- pagination

function pageParams(query: URLSearchParams): { perPage: number; page: number } {
  const perPage = Math.min(100, Math.max(1, Number.parseInt(query.get('per_page') ?? '30', 10) || 30));
  const page = Math.max(1, Number.parseInt(query.get('page') ?? '1', 10) || 1);
  return { perPage, page };
}

/** Slice a list to the requested page and build the Link header GitHub sends. */
function paginate<T>(req: ApiRequest, list: readonly T[]): { items: T[]; headers: Record<string, string> } {
  const { perPage, page } = pageParams(req.query);
  const last = Math.max(1, Math.ceil(list.length / perPage));
  const items = list.slice((page - 1) * perPage, page * perPage);
  const link = (target: number, rel: string): string => {
    const query = new URLSearchParams(req.query);
    query.set('per_page', String(perPage));
    query.set('page', String(target));
    const path = req.segments.map((segment) => encodeURIComponent(segment)).join('/');
    return `<${apiRoot(req.origin)}/${path}?${query.toString()}>; rel="${rel}"`;
  };
  const links: string[] = [];
  if (page > 1) links.push(link(page - 1, 'prev'));
  if (page < last) links.push(link(page + 1, 'next'), link(last, 'last'));
  if (page > 1) links.push(link(1, 'first'));
  return { items, headers: links.length > 0 ? { Link: links.join(', ') } : {} };
}

function listResponse<T>(req: ApiRequest, list: readonly T[], render: (item: T) => unknown): ApiResponse {
  const { items, headers } = paginate(req, list);
  const rendered = items.map(render);
  // Pre-serialized items (issues) are joined without re-parsing.
  if (rendered.every((item) => typeof item === 'string')) return { status: 200, body: `[${rendered.join(',')}]`, headers };
  return { status: 200, body: rendered.map((item) => (typeof item === 'string' ? JSON.parse(item) : item)), headers };
}

// ---------------------------------------------------------------- the router

export function handleRest(forge: Forge, req: ApiRequest): ApiResponse {
  const [head, ...rest] = req.segments;
  const method = req.method;
  if (head === undefined || head === '') return method === 'GET' ? ok(rootJson(req.origin)) : notFound();
  if (head === 'meta' && rest.length === 0 && method === 'GET') {
    return ok({ verifiable_password_authentication: false, installed_version: '3.17.0', hooks: [], git: [], packages: [], domains: {} });
  }
  if (head === 'rate_limit' && method === 'GET') {
    const core = { limit: 5000, used: 0, remaining: 5000, reset: Math.floor(Date.now() / 1000) + 3600 };
    return ok({ resources: { core, graphql: core, search: core }, rate: core });
  }
  if (head === 'zen' && method === 'GET') return ok({ message: 'Keep it logically awesome.' });
  if (head === 'user') return userRoutes(forge, req, rest);
  if (head === 'users' && rest.length >= 1) {
    const user = forge.users.get(rest[0].toLowerCase());
    if (user === undefined) return notFound();
    if (rest.length === 1 && method === 'GET') return ok(fullUserJson(forge, req.origin, user));
    if (rest.length === 2 && rest[1] === 'repos' && method === 'GET') return listRepos(forge, req, user);
    return notFound();
  }
  if (head === 'repos' && rest.length >= 2) {
    const repo = forge.getRepo(rest[0], rest[1]);
    if (repo === undefined) return notFound();
    return repoRoutes(forge, req, repo, rest.slice(2));
  }
  if (head === 'search' && rest.length === 1 && method === 'GET') return search(forge, req, rest[0]);
  return notFound();
}

function rootJson(origin: string): Json {
  const api = apiRoot(origin);
  return {
    current_user_url: `${api}/user`,
    current_user_repositories_url: `${api}/user/repos{?type,page,per_page,sort}`,
    repository_url: `${api}/repos/{owner}/{repo}`,
    user_url: `${api}/users/{user}`,
    issue_search_url: `${api}/search/issues?q={query}{&page,per_page,sort,order}`,
    repository_search_url: `${api}/search/repositories?q={query}{&type,page,per_page,sort,order}`,
    rate_limit_url: `${api}/rate_limit`,
  };
}

function fullUserJson(forge: Forge, origin: string, user: User): Json {
  return {
    ...userJson(origin, user),
    name: user.name,
    company: null,
    blog: '',
    location: null,
    email: user.email,
    hireable: null,
    bio: null,
    public_repos: forge.reposOf(user).filter((repo) => !repo.private).length,
    public_gists: 0,
    followers: 0,
    following: 0,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  };
}

function userRoutes(forge: Forge, req: ApiRequest, rest: readonly string[]): ApiResponse {
  if (rest.length === 0 && req.method === 'GET') return ok(fullUserJson(forge, req.origin, forge.viewer));
  if (rest.length === 1 && rest[0] === 'repos') {
    if (req.method === 'GET') return listRepos(forge, req, forge.viewer);
    if (req.method === 'POST') return createRepo(forge, req);
  }
  return notFound();
}

function listRepos(forge: Forge, req: ApiRequest, user: User): ApiResponse {
  let repos = forge.reposOf(user);
  const visibility = req.query.get('visibility') ?? req.query.get('type');
  if (visibility === 'public') repos = repos.filter((repo) => !repo.private);
  else if (visibility === 'private') repos = repos.filter((repo) => repo.private);
  const sort = req.query.get('sort') ?? 'full_name';
  const direction = req.query.get('direction') ?? (sort === 'full_name' ? 'asc' : 'desc');
  const key = (repo: Repo): string =>
    sort === 'created' ? `${repo.createdAt}${String(repo.id).padStart(12, '0')}` : sort === 'updated' || sort === 'pushed' ? `${repo.updatedAt}${String(repo.id).padStart(12, '0')}` : repo.name.toLowerCase();
  repos.sort((a, b) => (key(a) < key(b) ? -1 : key(a) > key(b) ? 1 : 0));
  if (direction === 'desc') repos.reverse();
  return listResponse(req, repos, (repo) => repoJson(forge, req.origin, repo));
}

const REPO_NAME_RE = /^[A-Za-z0-9._-]{1,100}$/;

/** Create a repository from a REST or MCP request body. */
export function createRepoFrom(forge: Forge, body: Json): Repo {
  const name = str(body.name);
  if (name === undefined || !REPO_NAME_RE.test(name) || name === '.' || name === '..' || name.toLowerCase().endsWith('.git')) {
    throw new ForgeError(422, 'Repository creation failed: name is invalid');
  }
  if (forge.getRepo(forge.viewer.login, name) !== undefined) throw new ForgeError(422, 'Repository creation failed: name already exists on this account');
  const isPrivate = body.private === true || body.visibility === 'private' || body.visibility === 'internal';
  const repo = forge.createRepo(forge.viewer, name, {
    description: str(body.description) ?? null,
    private: isPrivate,
    defaultBranch: str(body.default_branch) ?? 'main',
    hasIssues: body.has_issues !== false,
  });
  if (body.auto_init === true) {
    const signature = forge.signature(forge.viewer);
    const readme = `# ${name}\n${repo.description ? `\n${repo.description}\n` : ''}`;
    const commit = forge.commitChanges(null, [{ path: 'README.md', content: Buffer.from(readme, 'utf8') }], 'Initial commit', signature, signature);
    forge.setRef(repo, forge.headRef(repo), commit);
  }
  return repo;
}

function createRepo(forge: Forge, req: ApiRequest): ApiResponse {
  const body = bodyOf(req);
  const name = str(body.name);
  if (name !== undefined && forge.getRepo(forge.viewer.login, name) !== undefined) {
    return error(422, 'Repository creation failed.', [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name already exists on this account' }]);
  }
  try {
    return ok(repoJson(forge, req.origin, createRepoFrom(forge, body)), 201);
  } catch (err) {
    if (err instanceof ForgeError) return error(err.status, err.message, [{ resource: 'Repository', code: 'custom', field: 'name', message: err.message }]);
    throw err;
  }
}

function repoRoutes(forge: Forge, req: ApiRequest, repo: Repo, rest: readonly string[]): ApiResponse {
  const method = req.method;
  if (rest.length === 0) {
    if (method === 'GET') return ok(repoJson(forge, req.origin, repo));
    if (method === 'PATCH') return patchRepo(forge, req, repo);
    if (method === 'DELETE') {
      forge.deleteRepo(repo);
      return { status: 204 };
    }
    return notFound();
  }
  const [section, ...tail] = rest;
  switch (section) {
    case 'branches':
      return branches(forge, req, repo, tail);
    case 'commits':
      return commits(forge, req, repo, tail);
    case 'git':
      return gitData(forge, req, repo, tail);
    case 'contents':
      return contents(forge, req, repo, tail.join('/'));
    case 'readme':
      return readme(forge, req, repo);
    case 'tags':
      return tags(forge, req, repo);
    case 'labels':
      return labels(forge, req, repo, tail);
    case 'issues':
      return issues(forge, req, repo, tail);
    case 'pulls':
      return pulls(forge, req, repo, tail);
    case 'languages':
      return method === 'GET' ? ok({}) : notFound();
    case 'topics':
      return method === 'GET' ? ok({ names: [] }) : notFound();
    case 'collaborators':
    case 'contributors':
    case 'forks':
    case 'hooks':
    case 'releases':
    case 'milestones':
      return method === 'GET' && tail.length === 0 ? ok([]) : notFound();
    default:
      return notFound();
  }
}

function patchRepo(forge: Forge, req: ApiRequest, repo: Repo): ApiResponse {
  const body = bodyOf(req);
  if (body.name !== undefined) {
    const name = str(body.name);
    if (name === undefined || !REPO_NAME_RE.test(name)) return invalid('Repository', 'name');
    try {
      forge.renameRepo(repo, name);
    } catch (err) {
      if (err instanceof ForgeError) return error(422, err.message);
      throw err;
    }
  }
  if (body.description !== undefined) repo.description = body.description === null ? null : String(body.description);
  if (typeof body.private === 'boolean') repo.private = body.private;
  if (body.visibility === 'private' || body.visibility === 'public') repo.private = body.visibility === 'private';
  if (typeof body.has_issues === 'boolean') repo.hasIssues = body.has_issues;
  if (body.default_branch !== undefined) {
    const branch = str(body.default_branch);
    if (branch === undefined || (repo.refs.size > 0 && !forge.branchExists(repo, branch))) return invalid('Repository', 'default_branch');
    repo.defaultBranch = branch;
    repo.version += 1;
  }
  repo.updatedAt = new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');
  return ok(repoJson(forge, req.origin, repo));
}

// ---------------------------------------------------------------- branches, commits, tags

function branches(forge: Forge, req: ApiRequest, repo: Repo, tail: readonly string[]): ApiResponse {
  if (req.method !== 'GET') return notFound();
  const api = `${apiRoot(req.origin)}/repos/${repo.owner.login}/${repo.name}`;
  if (tail.length === 0) {
    const list: Array<{ name: string; sha: string }> = [];
    for (const [ref, sha] of repo.refs) if (ref.startsWith('refs/heads/')) list.push({ name: ref.slice(11), sha });
    list.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    return listResponse(req, list, (branch) => ({ name: branch.name, commit: { sha: branch.sha, url: `${api}/commits/${branch.sha}` }, protected: false }));
  }
  const name = tail.join('/');
  const sha = forge.branchTip(repo, name);
  if (sha === null) return notFound('Branch not found');
  const commit = forge.commitOf(sha);
  return ok({
    name,
    commit: commit === null ? { sha } : commitJson(forge, req.origin, repo, sha, commit),
    _links: { self: `${api}/branches/${name}`, html: `${req.origin}/${repo.owner.login}/${repo.name}/tree/${name}` },
    protected: false,
    protection: { enabled: false, required_status_checks: { enforcement_level: 'off', contexts: [], checks: [] } },
    protection_url: `${api}/branches/${name}/protection`,
  });
}

function commits(forge: Forge, req: ApiRequest, repo: Repo, tail: readonly string[]): ApiResponse {
  if (req.method !== 'GET') return notFound();
  if (tail.length > 0) {
    const ref = tail.join('/');
    const sha = forge.resolve(repo, ref);
    const commit = sha === null ? null : forge.commitOf(sha);
    if (sha === null || commit === null) return error(422, `No commit found for SHA: ${ref}`);
    const parent = commit.parents[0] ?? null;
    const parentCommit = parent === null ? null : forge.commitOf(parent);
    const files = forge.diff(parentCommit === null ? null : parentCommit.tree, commit.tree).map((file) => {
      const stats = fileStats(forge, file.oldOid, file.newOid);
      return { sha: file.newOid ?? file.oldOid, filename: file.path, status: file.status, additions: stats.additions, deletions: stats.deletions, changes: stats.additions + stats.deletions };
    });
    const total = files.reduce((sum, file) => ({ additions: sum.additions + file.additions, deletions: sum.deletions + file.deletions }), { additions: 0, deletions: 0 });
    return ok({ ...commitJson(forge, req.origin, repo, sha, commit), stats: { ...total, total: total.additions + total.deletions }, files });
  }
  if (repo.refs.size === 0) return error(409, 'Git Repository is empty.');
  const start = req.query.get('sha') ?? '';
  const tip = forge.resolve(repo, start);
  if (tip === null) return notFound(`No commit found for SHA: ${start}`);
  const { perPage, page } = pageParams(req.query);
  let list = forge.walkCommits([tip], page * perPage + 1 + (req.query.has('path') || req.query.has('since') || req.query.has('until') || req.query.has('author') ? 1_000_000 : 0));
  const path = req.query.get('path');
  if (path !== null && path !== '') {
    list = list.filter((oid) => {
      const commit = forge.commitOf(oid);
      if (commit === null) return false;
      const here = forge.lookupPath(oid, path)?.oid ?? null;
      if (commit.parents.length === 0) return here !== null;
      return commit.parents.every((parent) => (forge.lookupPath(parent, path)?.oid ?? null) !== here);
    });
  }
  const since = req.query.get('since');
  const until = req.query.get('until');
  if (since !== null || until !== null) {
    const from = since === null ? -Infinity : Date.parse(since) / 1000;
    const to = until === null ? Infinity : Date.parse(until) / 1000;
    list = list.filter((oid) => {
      const time = forge.commitOf(oid)?.committer.time ?? 0;
      return time >= from && time <= to;
    });
  }
  return listResponse(req, list, (oid) => commitJson(forge, req.origin, repo, oid, forge.commitOf(oid) as Commit));
}

function tags(forge: Forge, req: ApiRequest, repo: Repo): ApiResponse {
  if (req.method !== 'GET') return notFound();
  const api = `${apiRoot(req.origin)}/repos/${repo.owner.login}/${repo.name}`;
  const list: Array<{ name: string; sha: string }> = [];
  for (const [ref, oid] of repo.refs) if (ref.startsWith('refs/tags/')) list.push({ name: ref.slice(10), sha: forge.peel(oid) });
  list.sort((a, b) => (a.name < b.name ? 1 : a.name > b.name ? -1 : 0));
  return listResponse(req, list, (tag) => ({
    name: tag.name,
    commit: { sha: tag.sha, url: `${api}/commits/${tag.sha}` },
    zipball_url: `${api}/zipball/refs/tags/${tag.name}`,
    tarball_url: `${api}/tarball/refs/tags/${tag.name}`,
    node_id: `REF_${Buffer.from(`tag:${repo.id}:${tag.name}`).toString('base64url')}`,
  }));
}

// ---------------------------------------------------------------- git data

function refJson(forge: Forge, origin: string, repo: Repo, ref: string, sha: string): Json {
  const api = `${apiRoot(origin)}/repos/${repo.owner.login}/${repo.name}`;
  const type = forge.getObject(sha)?.type ?? 'commit';
  return {
    ref,
    node_id: `REF_${Buffer.from(`ref:${repo.id}:${ref}`).toString('base64url')}`,
    url: `${api}/git/${ref}`,
    object: { sha, type, url: `${api}/git/${type}s/${sha}` },
  };
}

function gitData(forge: Forge, req: ApiRequest, repo: Repo, tail: readonly string[]): ApiResponse {
  const [kind, ...rest] = tail;
  const method = req.method;
  if (kind === 'refs' || kind === 'ref' || kind === 'matching-refs') {
    if (kind === 'refs' && rest.length === 0 && method === 'POST') {
      const body = bodyOf(req);
      const ref = str(body.ref);
      const sha = str(body.sha)?.toLowerCase();
      if (ref === undefined || !ref.startsWith('refs/') || ref.split('/').length < 3) return error(422, 'Reference name is invalid', [{ resource: 'Reference', field: 'ref', code: 'invalid' }]);
      if (sha === undefined || forge.getObject(sha) === undefined) return error(422, 'Object does not exist', [{ resource: 'Reference', field: 'sha', code: 'invalid' }]);
      if (repo.refs.has(ref)) return error(422, 'Reference already exists', [{ resource: 'Reference', field: 'ref', code: 'already_exists' }]);
      forge.setRef(repo, ref, sha);
      return ok(refJson(forge, req.origin, repo, ref, sha), 201);
    }
    const name = `refs/${rest.join('/')}`;
    if (method === 'GET') {
      const exact = repo.refs.get(name);
      if (exact !== undefined && kind !== 'matching-refs') return ok(refJson(forge, req.origin, repo, name, exact));
      if (kind === 'ref') return notFound();
      const prefix = rest.length === 0 ? 'refs/' : name;
      const list = [...repo.refs.entries()]
        .filter(([ref]) => ref.startsWith(prefix) && (kind === 'matching-refs' || ref === prefix || ref.startsWith(prefix.endsWith('/') ? prefix : `${prefix}/`)))
        .sort(([a], [b]) => (a < b ? -1 : 1));
      if (list.length === 0) return kind === 'matching-refs' ? ok([]) : notFound();
      return ok(list.map(([ref, sha]) => refJson(forge, req.origin, repo, ref, sha)));
    }
    const current = repo.refs.get(name);
    if (method === 'PATCH') {
      if (current === undefined) return error(422, 'Reference does not exist');
      const body = bodyOf(req);
      const sha = str(body.sha)?.toLowerCase();
      if (sha === undefined || forge.getObject(sha) === undefined) return error(422, 'Object does not exist');
      if (body.force !== true && !forge.isAncestor(forge.peel(current), forge.peel(sha))) return error(422, 'Update is not a fast forward');
      forge.setRef(repo, name, sha);
      return ok(refJson(forge, req.origin, repo, name, sha));
    }
    if (method === 'DELETE') {
      if (current === undefined) return error(422, 'Reference does not exist');
      forge.setRef(repo, name, ZERO_OID);
      return { status: 204 };
    }
    return notFound();
  }
  if (method !== 'GET' || rest.length !== 1) return notFound();
  const sha = rest[0].toLowerCase();
  const object = forge.getObject(sha);
  const api = `${apiRoot(req.origin)}/repos/${repo.owner.login}/${repo.name}`;
  if (kind === 'commits') {
    if (object === undefined || object.type !== 'commit') return notFound();
    return ok(gitCommitJson(req.origin, repo, sha, parseCommit(object.data)));
  }
  if (kind === 'blobs') {
    if (object === undefined || object.type !== 'blob') return notFound();
    return ok({ sha, node_id: `B_${Buffer.from(`blob:${sha}`).toString('base64url')}`, size: object.data.length, url: `${api}/git/blobs/${sha}`, content: wrapBase64(object.data), encoding: 'base64' });
  }
  if (kind === 'trees') {
    const resolved = object !== undefined && object.type === 'commit' ? parseCommit(object.data).tree : sha;
    const treeObject = forge.getObject(resolved);
    if (treeObject === undefined || treeObject.type !== 'tree') return notFound();
    const recursive = req.query.has('recursive') && req.query.get('recursive') !== '0' && req.query.get('recursive') !== 'false';
    const entries: Json[] = [];
    const walk = (oid: string, prefix: string): void => {
      const data = forge.getObject(oid);
      if (data === undefined) return;
      for (const entry of parseTree(data.data)) {
        const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
        const tree = isTreeMode(entry.mode);
        const item: Json = { path, mode: tree ? '040000' : entry.mode, type: tree ? 'tree' : entry.mode === '160000' ? 'commit' : 'blob', sha: entry.oid, url: `${api}/git/${tree ? 'trees' : 'blobs'}/${entry.oid}` };
        if (!tree) item.size = forge.getObject(entry.oid)?.data.length ?? 0;
        entries.push(item);
        if (tree && recursive) walk(entry.oid, path);
      }
    };
    walk(resolved, '');
    return ok({ sha: resolved, url: `${api}/git/trees/${resolved}`, tree: entries, truncated: false });
  }
  return notFound();
}

// ---------------------------------------------------------------- contents

function wrapBase64(data: Buffer): string {
  const text = data.toString('base64');
  const lines: string[] = [];
  for (let pos = 0; pos < text.length; pos += 60) lines.push(text.slice(pos, pos + 60));
  return lines.length === 0 ? '' : `${lines.join('\n')}\n`;
}

function contentEntryJson(origin: string, repo: Repo, path: string, type: string, sha: string, size: number, ref: string): Json {
  const api = `${apiRoot(origin)}/repos/${repo.owner.login}/${repo.name}`;
  const name = path.split('/').pop() ?? path;
  const self = `${api}/contents/${path}?ref=${encodeURIComponent(ref)}`;
  const html = `${origin}/${repo.owner.login}/${repo.name}/${type === 'dir' ? 'tree' : 'blob'}/${ref}/${path}`;
  const git = `${api}/git/${type === 'dir' ? 'trees' : 'blobs'}/${sha}`;
  return {
    name,
    path,
    sha,
    size,
    url: self,
    html_url: html,
    git_url: git,
    download_url: type === 'file' ? `${origin}/${repo.owner.login}/${repo.name}/raw/${ref}/${path}` : null,
    type,
    _links: { self, git, html },
  };
}

/** Commit one file change on a branch (creating the branch, or the repository's first commit, when needed). */
export function writeFile(
  forge: Forge,
  repo: Repo,
  input: { path: string; content: Buffer | null; message: string; branch?: string; sha?: string; author?: { name: string; email: string } },
): { status: number; commit: string; blob: string | null } {
  const path = input.path.replace(/^\/+|\/+$/g, '');
  if (path === '' || path.split('/').some((part) => part === '' || part === '.' || part === '..' || part === '.git')) throw new ForgeError(422, 'path is invalid');
  const branch = input.branch !== undefined && input.branch !== '' ? input.branch.replace(/^refs\/heads\//, '') : repo.defaultBranch;
  let parent = forge.branchTip(repo, branch);
  if (parent === null && repo.refs.size > 0) {
    // A write to a new branch starts it from the default branch, as a convenience.
    parent = forge.branchTip(repo, repo.defaultBranch);
    if (parent === null) throw new ForgeError(404, `Branch ${branch} not found`);
  }
  const existing = parent === null ? null : forge.lookupPath(parent, path);
  if (existing !== null && existing.type === 'tree') throw new ForgeError(422, `${path} is a directory`);
  if (input.content === null) {
    if (existing === null) throw new ForgeError(404, 'Not Found');
    if (input.sha === undefined || input.sha === '') throw new ForgeError(422, '"sha" wasn\'t supplied.');
    if (input.sha.toLowerCase() !== existing.oid) throw new ForgeError(409, `${path} does not match ${input.sha}`);
  } else if (existing !== null) {
    if (input.sha === undefined || input.sha === '') throw new ForgeError(422, 'Invalid request.\n\n"sha" wasn\'t supplied.');
    if (input.sha.toLowerCase() !== existing.oid) throw new ForgeError(409, `${path} does not match ${input.sha}`);
  }
  const user = input.author === undefined ? forge.signature(forge.viewer) : { ...forge.signature(forge.viewer), name: input.author.name, email: input.author.email };
  const committer = forge.signature(forge.viewer);
  const commit = forge.commitChanges(parent, [{ path, content: input.content }], input.message, user, committer);
  forge.setRef(repo, `refs/heads/${branch}`, commit);
  const blob = input.content === null ? null : forge.lookupPath(commit, path)?.oid ?? null;
  return { status: existing === null ? 201 : 200, commit, blob };
}

function contents(forge: Forge, req: ApiRequest, repo: Repo, rawPath: string): ApiResponse {
  const path = rawPath.replace(/^\/+|\/+$/g, '');
  if (req.method === 'GET') {
    if (repo.refs.size === 0) return notFound('This repository is empty.');
    const ref = req.query.get('ref') ?? repo.defaultBranch;
    const sha = forge.resolve(repo, ref);
    if (sha === null) return notFound(`No commit found for the ref ${ref}`);
    const entry = path === '' ? { type: 'tree' as const, mode: '40000', oid: (forge.commitOf(sha) as Commit).tree } : forge.lookupPath(sha, path);
    if (entry === null) return notFound();
    if (entry.type === 'tree') {
      const tree = forge.getObject(entry.oid);
      if (tree === undefined) return notFound();
      return ok(
        parseTree(tree.data).map((child) => {
          const childPath = path === '' ? child.name : `${path}/${child.name}`;
          const isDir = isTreeMode(child.mode);
          return contentEntryJson(req.origin, repo, childPath, isDir ? 'dir' : child.mode === '160000' ? 'submodule' : 'file', child.oid, isDir ? 0 : forge.getObject(child.oid)?.data.length ?? 0, ref);
        }),
      );
    }
    const blob = forge.getObject(entry.oid);
    if (blob === undefined) return notFound();
    return ok({ ...contentEntryJson(req.origin, repo, path, 'file', entry.oid, blob.data.length, ref), content: wrapBase64(blob.data), encoding: 'base64' });
  }
  if (req.method !== 'PUT' && req.method !== 'DELETE') return notFound();
  const body = bodyOf(req);
  const message = str(body.message);
  if (message === undefined || message === '') return invalid('Commit', 'message', 'missing_field');
  let content: Buffer | null = null;
  if (req.method === 'PUT') {
    const encoded = str(body.content);
    if (encoded === undefined) return invalid('Content', 'content', 'missing_field');
    if (!/^[A-Za-z0-9+/=\s_-]*$/.test(encoded)) return error(422, 'content is not valid Base64');
    content = Buffer.from(encoded, 'base64');
  }
  const author = isRecord(body.author) && typeof body.author.name === 'string' && typeof body.author.email === 'string' ? { name: body.author.name, email: body.author.email } : undefined;
  try {
    const result = writeFile(forge, repo, { path, content, message, branch: str(body.branch), sha: str(body.sha), author });
    const commit = forge.commitOf(result.commit) as Commit;
    const branch = str(body.branch) ?? repo.defaultBranch;
    return ok(
      {
        content: result.blob === null ? null : { ...contentEntryJson(req.origin, repo, path, 'file', result.blob, content === null ? 0 : content.length, branch), encoding: 'base64' },
        commit: gitCommitJson(req.origin, repo, result.commit, commit),
      },
      req.method === 'PUT' ? result.status : 200,
    );
  } catch (err) {
    if (err instanceof ForgeError) return err.status === 404 ? notFound(err.message) : error(err.status, err.message);
    throw err;
  }
}

function readme(forge: Forge, req: ApiRequest, repo: Repo): ApiResponse {
  if (req.method !== 'GET') return notFound();
  const sha = forge.resolve(repo, req.query.get('ref') ?? repo.defaultBranch);
  if (sha === null) return notFound();
  const tree = forge.getObject((forge.commitOf(sha) as Commit).tree);
  const entry = tree === undefined ? undefined : parseTree(tree.data).find((candidate) => /^readme(\.|$)/i.test(candidate.name) && !isTreeMode(candidate.mode));
  if (entry === undefined) return notFound();
  return contents(forge, { ...req, query: new URLSearchParams({ ref: req.query.get('ref') ?? repo.defaultBranch }) }, repo, entry.name);
}

// ---------------------------------------------------------------- labels

function labels(forge: Forge, req: ApiRequest, repo: Repo, tail: readonly string[]): ApiResponse {
  if (tail.length === 0) {
    if (req.method === 'GET') return listResponse(req, [...repo.labels.values()], (label) => labelJson(req.origin, repo, label));
    if (req.method === 'POST') {
      const body = bodyOf(req);
      const name = str(body.name);
      if (name === undefined || name.trim() === '') return invalid('Label', 'name', 'missing_field');
      if (repo.labels.has(name.toLowerCase())) return invalid('Label', 'name', 'already_exists');
      const color = (str(body.color) ?? 'ededed').replace(/^#/, '');
      return ok(labelJson(req.origin, repo, forge.createLabel(repo, name, color, str(body.description) ?? null)), 201);
    }
    return notFound();
  }
  const label = repo.labels.get(tail.join('/').toLowerCase());
  if (label === undefined) return notFound();
  if (req.method === 'GET') return ok(labelJson(req.origin, repo, label));
  if (req.method === 'PATCH') {
    const body = bodyOf(req);
    const name = str(body.new_name) ?? str(body.name);
    if (name !== undefined && name !== label.name) {
      repo.labels.delete(label.name.toLowerCase());
      label.name = name;
      repo.labels.set(name.toLowerCase(), label);
    }
    if (str(body.color) !== undefined) label.color = (str(body.color) as string).replace(/^#/, '');
    if (body.description !== undefined) label.description = str(body.description) ?? null;
    for (const issue of repo.issues.values()) issue.json = null;
    return ok(labelJson(req.origin, repo, label));
  }
  if (req.method === 'DELETE') {
    repo.labels.delete(label.name.toLowerCase());
    for (const issue of repo.issues.values()) {
      if (issue.labels.includes(label)) {
        issue.labels = issue.labels.filter((candidate) => candidate !== label);
        issue.json = null;
      }
    }
    return { status: 204 };
  }
  return notFound();
}

/** Resolve label names (or {name} objects) against a repository; the unknown ones are returned apart. */
export function resolveLabels(repo: Repo, value: unknown): { labels: Label[]; unknown: string[] } {
  const out: Label[] = [];
  const unknown: string[] = [];
  if (!Array.isArray(value)) return { labels: out, unknown };
  for (const item of value) {
    const name = typeof item === 'string' ? item : isRecord(item) && typeof item.name === 'string' ? item.name : null;
    if (name === null) continue;
    const label = repo.labels.get(name.toLowerCase());
    if (label === undefined) unknown.push(name);
    else if (!out.includes(label)) out.push(label);
  }
  return { labels: out, unknown };
}

// ---------------------------------------------------------------- issues

export function sortIssues(list: Issue[], sort: string, direction: string): Issue[] {
  const key = (issue: Issue): number | string => (sort === 'updated' ? issue.updatedAt : sort === 'comments' ? issue.comments.length : 0);
  list.sort((a, b) => {
    const ka = key(a);
    const kb = key(b);
    if (ka !== kb) return ka < kb ? -1 : 1;
    return a.number - b.number;
  });
  if (direction !== 'asc') list.reverse();
  return list;
}

export function filterIssues(repo: Repo, state: string, labelNames: string[]): Issue[] {
  const out: Issue[] = [];
  const wanted = labelNames.map((name) => name.trim().toLowerCase()).filter((name) => name !== '');
  for (const issue of repo.issues.values()) {
    if (state !== 'all' && issue.state !== state) continue;
    if (wanted.length > 0 && !wanted.every((name) => issue.labels.some((label) => label.name.toLowerCase() === name))) continue;
    out.push(issue);
  }
  return out;
}

function issues(forge: Forge, req: ApiRequest, repo: Repo, tail: readonly string[]): ApiResponse {
  const method = req.method;
  if (tail.length === 0) {
    if (method === 'GET') {
      const state = req.query.get('state') ?? 'open';
      const labelsParam = req.query.get('labels');
      let list = filterIssues(repo, state, labelsParam === null ? [] : labelsParam.split(','));
      const since = req.query.get('since');
      if (since !== null) list = list.filter((issue) => issue.updatedAt >= since);
      sortIssues(list, req.query.get('sort') ?? 'created', req.query.get('direction') ?? 'desc');
      return listResponse(req, list, (issue) => issueJson(req.origin, repo, issue));
    }
    if (method === 'POST') {
      if (!repo.hasIssues) return error(410, 'Issues are disabled for this repo');
      const body = bodyOf(req);
      const title = typeof body.title === 'number' ? String(body.title) : str(body.title);
      if (title === undefined || title.trim() === '') return invalid('Issue', 'title', 'missing_field');
      const resolved = resolveLabels(repo, body.labels);
      if (resolved.unknown.length > 0) return invalid('Label', 'labels', 'invalid', { value: resolved.unknown[0], message: `Label ${resolved.unknown[0]} does not exist` });
      const issue = forge.createIssue(repo, { title, body: str(body.body) ?? '', labels: resolved.labels });
      return { status: 201, body: issueJson(req.origin, repo, issue) };
    }
    return notFound();
  }
  if (tail[0] === 'comments') {
    if (tail.length === 1 && method === 'GET') {
      const all: Comment[] = [];
      for (const issue of repo.issues.values()) all.push(...issue.comments);
      for (const pull of repo.pulls.values()) all.push(...pull.comments);
      all.sort((a, b) => a.id - b.id);
      return listResponse(req, all, (comment) => commentJson(req.origin, repo, comment));
    }
    const found = forge.commentsById.get(Number(tail[1]));
    if (found === undefined || found.repo !== repo || tail.length !== 2) return notFound();
    const comment = found.comment;
    if (method === 'GET') return ok(commentJson(req.origin, repo, comment));
    if (method === 'PATCH') {
      const text = str(bodyOf(req).body);
      if (text === undefined) return invalid('IssueComment', 'body', 'missing_field');
      comment.body = text;
      comment.updatedAt = new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');
      return ok(commentJson(req.origin, repo, comment));
    }
    if (method === 'DELETE') {
      for (const target of [...repo.issues.values(), ...repo.pulls.values()]) {
        const index = target.comments.indexOf(comment);
        if (index >= 0) {
          target.comments.splice(index, 1);
          if ('json' in target) target.json = null;
        }
      }
      forge.commentsById.delete(comment.id);
      return { status: 204 };
    }
    return notFound();
  }
  const number = Number(tail[0]);
  if (!Number.isSafeInteger(number)) return notFound();
  const issue = repo.issues.get(number);
  const pull = issue === undefined ? repo.pulls.get(number) : undefined;
  if (issue === undefined && pull === undefined) return notFound();
  const target = (issue ?? pull) as Issue | Pull;
  if (tail.length === 1) {
    if (method === 'GET') return issue !== undefined ? { status: 200, body: issueJson(req.origin, repo, issue) } : ok(pullAsIssueJson(req.origin, repo, pull as Pull));
    if (method === 'PATCH') {
      const body = bodyOf(req);
      if (body.title !== undefined) {
        const title = str(body.title);
        if (title === undefined || title.trim() === '') return invalid('Issue', 'title');
        target.title = title;
      }
      if (body.body !== undefined) target.body = str(body.body) ?? '';
      if (body.labels !== undefined) {
        const resolved = resolveLabels(repo, body.labels);
        if (resolved.unknown.length > 0) return invalid('Label', 'labels', 'invalid', { value: resolved.unknown[0] });
        target.labels = resolved.labels;
      }
      if (body.state !== undefined) {
        if (body.state !== 'open' && body.state !== 'closed') return invalid('Issue', 'state');
        if (issue !== undefined) forge.setIssueState(issue, body.state, str(body.state_reason) ?? null);
        else if (body.state === 'closed') forge.closePull(repo, pull as Pull);
        else forge.reopenPull(pull as Pull);
      }
      if (issue !== undefined) {
        forge.touchIssue(issue);
        return { status: 200, body: issueJson(req.origin, repo, issue) };
      }
      return ok(pullAsIssueJson(req.origin, repo, pull as Pull));
    }
    return notFound();
  }
  if (tail[1] === 'comments' && tail.length === 2) {
    if (method === 'GET') return listResponse(req, target.comments, (comment) => commentJson(req.origin, repo, comment));
    if (method === 'POST') {
      const text = str(bodyOf(req).body);
      if (text === undefined || text === '') return invalid('IssueComment', 'body', 'missing_field');
      return ok(commentJson(req.origin, repo, forge.addComment(repo, target, text)), 201);
    }
    return notFound();
  }
  if (tail[1] === 'labels' && tail.length === 2) {
    if (method === 'GET') return ok(target.labels.map((label) => labelJson(req.origin, repo, label)));
    if (method === 'POST' || method === 'PUT') {
      const body = req.body;
      const names = Array.isArray(body) ? body : isRecord(body) ? body.labels : undefined;
      const resolved = resolveLabels(repo, names);
      if (resolved.unknown.length > 0) return invalid('Label', 'labels', 'invalid', { value: resolved.unknown[0] });
      target.labels = method === 'PUT' ? resolved.labels : [...target.labels, ...resolved.labels.filter((label) => !target.labels.includes(label))];
      if (issue !== undefined) forge.touchIssue(issue);
      return ok(target.labels.map((label) => labelJson(req.origin, repo, label)));
    }
    if (method === 'DELETE') {
      target.labels = [];
      if (issue !== undefined) forge.touchIssue(issue);
      return { status: 204 };
    }
  }
  return notFound();
}

// ---------------------------------------------------------------- pull requests

/** Open a pull request from a REST, GraphQL or MCP input; throws ForgeError(422) on a bad head or base. */
export function openPull(forge: Forge, repo: Repo, input: { title: string; body: string; head: string; base: string; draft: boolean }): Pull {
  const head = input.head.includes(':') ? input.head.slice(input.head.indexOf(':') + 1) : input.head;
  const base = input.base.replace(/^refs\/heads\//, '');
  if (input.title.trim() === '') throw new ForgeError(422, 'Validation Failed: title is missing');
  if (!forge.branchExists(repo, base)) throw new ForgeError(422, `Validation Failed: base ${base} does not exist`);
  if (!forge.branchExists(repo, head.replace(/^refs\/heads\//, ''))) throw new ForgeError(422, `Validation Failed: head ${head} does not exist`);
  const headName = head.replace(/^refs\/heads\//, '');
  if (headName === base) throw new ForgeError(422, `Validation Failed: head and base are both ${base}`);
  for (const pull of repo.pulls.values()) {
    if (pull.state === 'open' && pull.head === headName && pull.base === base) {
      throw new ForgeError(422, `A pull request already exists for ${repo.owner.login}:${headName}.`);
    }
  }
  return forge.createPull(repo, { title: input.title, body: input.body, head: headName, base, draft: input.draft });
}

function pulls(forge: Forge, req: ApiRequest, repo: Repo, tail: readonly string[]): ApiResponse {
  const method = req.method;
  if (tail.length === 0) {
    if (method === 'GET') {
      const state = req.query.get('state') ?? 'open';
      const headFilter = req.query.get('head');
      const baseFilter = req.query.get('base');
      let list = [...repo.pulls.values()].filter((pull) => state === 'all' || pull.state === state);
      if (headFilter !== null) {
        const name = headFilter.includes(':') ? headFilter.slice(headFilter.indexOf(':') + 1) : headFilter;
        list = list.filter((pull) => pull.head === name);
      }
      if (baseFilter !== null) list = list.filter((pull) => pull.base === baseFilter);
      const sort = req.query.get('sort') ?? 'created';
      const direction = req.query.get('direction') ?? 'desc';
      list.sort((a, b) => {
        const ka = sort === 'updated' ? a.updatedAt : '';
        const kb = sort === 'updated' ? b.updatedAt : '';
        return ka !== kb ? (ka < kb ? -1 : 1) : a.number - b.number;
      });
      if (direction !== 'asc') list.reverse();
      return listResponse(req, list, (pull) => pullJson(forge, req.origin, repo, pull, false));
    }
    if (method === 'POST') {
      const body = bodyOf(req);
      const title = str(body.title);
      const head = str(body.head);
      const base = str(body.base);
      if (title === undefined) return invalid('PullRequest', 'title', 'missing_field');
      if (head === undefined) return invalid('PullRequest', 'head', 'missing_field');
      if (base === undefined) return invalid('PullRequest', 'base', 'missing_field');
      try {
        const pull = openPull(forge, repo, { title, body: str(body.body) ?? '', head, base, draft: body.draft === true });
        return ok(pullJson(forge, req.origin, repo, pull, true), 201);
      } catch (err) {
        if (err instanceof ForgeError) return error(err.status, err.message, [{ resource: 'PullRequest', code: 'invalid', message: err.message }]);
        throw err;
      }
    }
    return notFound();
  }
  const number = Number(tail[0]);
  const pull = Number.isSafeInteger(number) ? repo.pulls.get(number) : undefined;
  if (pull === undefined) return notFound();
  if (tail.length === 1) {
    if (method === 'GET') return ok(pullJson(forge, req.origin, repo, pull, true));
    if (method === 'PATCH') {
      const body = bodyOf(req);
      if (str(body.title) !== undefined) pull.title = str(body.title) as string;
      if (body.body !== undefined) pull.body = str(body.body) ?? '';
      if (str(body.base) !== undefined) {
        const base = str(body.base) as string;
        if (!forge.branchExists(repo, base)) return invalid('PullRequest', 'base');
        pull.base = base;
      }
      if (body.state === 'closed') forge.closePull(repo, pull);
      else if (body.state === 'open') forge.reopenPull(pull);
      pull.updatedAt = new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');
      return ok(pullJson(forge, req.origin, repo, pull, true));
    }
    return notFound();
  }
  const sub = tail[1];
  if (sub === 'files' && method === 'GET') return listResponse(req, pullFilesJson(forge, req.origin, repo, pull), (file) => file);
  if (sub === 'commits' && method === 'GET') {
    return listResponse(req, pullCommits(forge, repo, pull), (oid) => commitJson(forge, req.origin, repo, oid, forge.commitOf(oid) as Commit));
  }
  if (sub === 'merge') {
    if (method === 'GET') return pull.merged ? { status: 204 } : notFound();
    if (method === 'PUT') {
      const body = bodyOf(req);
      try {
        const sha = forge.mergePull(repo, pull, {
          method: str(body.merge_method) ?? 'merge',
          title: str(body.commit_title),
          message: str(body.commit_message),
          sha: str(body.sha),
        });
        return ok({ sha, merged: true, message: 'Pull Request successfully merged' });
      } catch (err) {
        if (err instanceof ForgeError) return error(err.status, err.message);
        throw err;
      }
    }
    return notFound();
  }
  if ((sub === 'comments' || sub === 'reviews' || sub === 'requested_reviewers') && method === 'GET') return ok(sub === 'requested_reviewers' ? { users: [], teams: [] } : []);
  return notFound();
}

// ---------------------------------------------------------------- search

function search(forge: Forge, req: ApiRequest, kind: string): ApiResponse {
  const q = (req.query.get('q') ?? '').trim();
  const terms = q.split(/\s+/).filter((term) => term !== '');
  const qualifiers = new Map<string, string>();
  const words: string[] = [];
  for (const term of terms) {
    const colon = term.indexOf(':');
    if (colon > 0) qualifiers.set(term.slice(0, colon).toLowerCase(), term.slice(colon + 1));
    else words.push(term.toLowerCase());
  }
  const repos = forge.reposOf(forge.viewer).filter((repo) => {
    const named = qualifiers.get('repo');
    return named === undefined || `${repo.owner.login}/${repo.name}`.toLowerCase() === named.toLowerCase();
  });
  if (kind === 'repositories') {
    const hits = repos.filter((repo) => words.every((word) => repo.name.toLowerCase().includes(word) || (repo.description ?? '').toLowerCase().includes(word)));
    const { items } = paginate(req, hits);
    return ok({ total_count: hits.length, incomplete_results: false, items: items.map((repo) => repoJson(forge, req.origin, repo)) });
  }
  if (kind === 'issues') {
    const isPr = qualifiers.get('is') === 'pr' || qualifiers.get('type') === 'pr';
    const state = qualifiers.get('state') ?? (qualifiers.get('is') === 'open' || qualifiers.get('is') === 'closed' ? qualifiers.get('is') : undefined);
    const hits: Json[] = [];
    for (const repo of repos) {
      if (!isPr) {
        for (const issue of repo.issues.values()) {
          if (state !== undefined && issue.state !== state) continue;
          if (!words.every((word) => issue.title.toLowerCase().includes(word) || issue.body.toLowerCase().includes(word))) continue;
          hits.push(JSON.parse(issueJson(req.origin, repo, issue)) as Json);
        }
      }
      if (isPr || qualifiers.get('type') === undefined) {
        for (const pull of repo.pulls.values()) {
          if (!isPr && qualifiers.get('is') === 'issue') continue;
          if (state !== undefined && pull.state !== state) continue;
          if (!words.every((word) => pull.title.toLowerCase().includes(word) || pull.body.toLowerCase().includes(word))) continue;
          hits.push(pullAsIssueJson(req.origin, repo, pull));
        }
      }
    }
    const { items } = paginate(req, hits);
    return ok({ total_count: hits.length, incomplete_results: false, items });
  }
  return notFound();
}
