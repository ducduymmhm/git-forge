// GraphQL v4 at /api/graphql: a small spec-shaped engine (lexer, parser, validation against the schema,
// execution, introspection) over a GitHub-flavoured schema that serves the documents gh and
// github-mcp-server send: viewer, repository, issues, pull requests, node(id:), search and the
// create/close/merge/comment mutations.

import { ForgeError, parseGlobalId } from './store.ts';
import type { Comment, Forge, Issue, Label, Pull, Repo, User } from './store.ts';
import { filterIssues, openPull, pullCommits, resolveLabels } from './rest.ts';
import { lineStats } from './store.ts';
import type { Commit } from './git.ts';
import { isoFromEpoch } from './store.ts';

// ================================================================ lexer and parser

interface Loc {
  line: number;
  column: number;
}

class GraphQLSyntaxError extends Error {
  readonly loc: Loc;
  constructor(message: string, loc: Loc) {
    super(message);
    this.loc = loc;
  }
}

type TokenKind = 'punct' | 'name' | 'int' | 'float' | 'string' | 'eof';

interface Token {
  kind: TokenKind;
  value: string;
  loc: Loc;
}

function lex(source: string): Token[] {
  const tokens: Token[] = [];
  let pos = 0;
  let line = 1;
  let lineStart = 0;
  const loc = (at: number): Loc => ({ line, column: at - lineStart + 1 });
  while (pos < source.length) {
    const ch = source[pos];
    if (ch === '\n') {
      pos += 1;
      line += 1;
      lineStart = pos;
      continue;
    }
    if (ch === ' ' || ch === '\t' || ch === '\r' || ch === ',' || ch === '﻿') {
      pos += 1;
      continue;
    }
    if (ch === '#') {
      while (pos < source.length && source[pos] !== '\n') pos += 1;
      continue;
    }
    if (source.startsWith('...', pos)) {
      tokens.push({ kind: 'punct', value: '...', loc: loc(pos) });
      pos += 3;
      continue;
    }
    if ('!$&()/:=@[]{}|'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch, loc: loc(pos) });
      pos += 1;
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      const start = pos;
      while (pos < source.length && /[A-Za-z0-9_]/.test(source[pos])) pos += 1;
      tokens.push({ kind: 'name', value: source.slice(start, pos), loc: loc(start) });
      continue;
    }
    if (/[-0-9]/.test(ch)) {
      const start = pos;
      if (source[pos] === '-') pos += 1;
      while (pos < source.length && /[0-9]/.test(source[pos])) pos += 1;
      let kind: TokenKind = 'int';
      if (source[pos] === '.') {
        kind = 'float';
        pos += 1;
        while (pos < source.length && /[0-9]/.test(source[pos])) pos += 1;
      }
      if (source[pos] === 'e' || source[pos] === 'E') {
        kind = 'float';
        pos += 1;
        if (source[pos] === '+' || source[pos] === '-') pos += 1;
        while (pos < source.length && /[0-9]/.test(source[pos])) pos += 1;
      }
      tokens.push({ kind, value: source.slice(start, pos), loc: loc(start) });
      continue;
    }
    if (ch === '"') {
      const start = pos;
      if (source.startsWith('"""', pos)) {
        pos += 3;
        let value = '';
        while (pos < source.length && !source.startsWith('"""', pos)) {
          if (source.startsWith('\\"""', pos)) {
            value += '"""';
            pos += 4;
            continue;
          }
          if (source[pos] === '\n') {
            line += 1;
            lineStart = pos + 1;
          }
          value += source[pos];
          pos += 1;
        }
        if (pos >= source.length) throw new GraphQLSyntaxError('Unterminated string', loc(start));
        pos += 3;
        tokens.push({ kind: 'string', value: blockString(value), loc: loc(start) });
        continue;
      }
      pos += 1;
      let value = '';
      while (pos < source.length && source[pos] !== '"') {
        if (source[pos] === '\n') throw new GraphQLSyntaxError('Unterminated string', loc(start));
        if (source[pos] === '\\') {
          const next = source[pos + 1];
          const map: Record<string, string> = { '"': '"', '\\': '\\', '/': '/', b: '\b', f: '\f', n: '\n', r: '\r', t: '\t' };
          if (next === 'u') {
            value += String.fromCharCode(parseInt(source.slice(pos + 2, pos + 6), 16));
            pos += 6;
            continue;
          }
          if (map[next] === undefined) throw new GraphQLSyntaxError(`Invalid escape \\${next}`, loc(pos));
          value += map[next];
          pos += 2;
          continue;
        }
        value += source[pos];
        pos += 1;
      }
      if (pos >= source.length) throw new GraphQLSyntaxError('Unterminated string', loc(start));
      pos += 1;
      tokens.push({ kind: 'string', value, loc: loc(start) });
      continue;
    }
    throw new GraphQLSyntaxError(`Unexpected character ${JSON.stringify(ch)}`, loc(pos));
  }
  tokens.push({ kind: 'eof', value: '', loc: loc(pos) });
  return tokens;
}

function blockString(raw: string): string {
  const lines = raw.split(/\r\n|\n|\r/);
  let common: number | null = null;
  for (let index = 1; index < lines.length; index += 1) {
    const indent = lines[index].length - lines[index].trimStart().length;
    if (indent < lines[index].length && (common === null || indent < common)) common = indent;
  }
  if (common !== null) for (let index = 1; index < lines.length; index += 1) lines[index] = lines[index].slice(common);
  while (lines.length > 0 && lines[0].trim() === '') lines.shift();
  while (lines.length > 0 && lines[lines.length - 1].trim() === '') lines.pop();
  return lines.join('\n');
}

type ValueNode =
  | { kind: 'Variable'; name: string }
  | { kind: 'Int' | 'Float' | 'String' | 'Enum'; value: string }
  | { kind: 'Boolean'; value: boolean }
  | { kind: 'Null' }
  | { kind: 'List'; values: ValueNode[] }
  | { kind: 'Object'; fields: Array<{ name: string; value: ValueNode }> };

interface Directive {
  name: string;
  args: Array<{ name: string; value: ValueNode }>;
}

interface FieldNode {
  kind: 'Field';
  alias: string | null;
  name: string;
  args: Array<{ name: string; value: ValueNode }>;
  directives: Directive[];
  selections: Selection[] | null;
  loc: Loc;
}

interface FragmentSpread {
  kind: 'FragmentSpread';
  name: string;
  directives: Directive[];
  loc: Loc;
}

interface InlineFragment {
  kind: 'InlineFragment';
  typeCondition: string | null;
  directives: Directive[];
  selections: Selection[];
  loc: Loc;
}

type Selection = FieldNode | FragmentSpread | InlineFragment;

type TypeNode = { kind: 'Named'; name: string } | { kind: 'List'; of: TypeNode } | { kind: 'NonNull'; of: TypeNode };

interface Operation {
  kind: 'Operation';
  operation: 'query' | 'mutation' | 'subscription';
  name: string | null;
  variables: Array<{ name: string; type: TypeNode; defaultValue: ValueNode | null }>;
  selections: Selection[];
  loc: Loc;
}

interface Fragment {
  kind: 'Fragment';
  name: string;
  typeCondition: string;
  selections: Selection[];
}

interface Document {
  operations: Operation[];
  fragments: Map<string, Fragment>;
}

class Parser {
  private pos = 0;
  private readonly tokens: Token[];
  constructor(source: string) {
    this.tokens = lex(source);
  }
  private peek(): Token {
    return this.tokens[this.pos];
  }
  private next(): Token {
    const token = this.tokens[this.pos];
    this.pos += 1;
    return token;
  }
  private is(value: string): boolean {
    const token = this.peek();
    return token.kind === 'punct' && token.value === value;
  }
  private expect(value: string): Token {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) {
      throw new GraphQLSyntaxError(`Expected "${value}", found ${token.kind === 'eof' ? '<EOF>' : JSON.stringify(token.value)}`, token.loc);
    }
    return token;
  }
  private name(): string {
    const token = this.next();
    if (token.kind !== 'name') throw new GraphQLSyntaxError(`Expected a name, found ${token.kind === 'eof' ? '<EOF>' : JSON.stringify(token.value)}`, token.loc);
    return token.value;
  }

  document(): Document {
    const operations: Operation[] = [];
    const fragments = new Map<string, Fragment>();
    while (this.peek().kind !== 'eof') {
      const token = this.peek();
      if (token.kind === 'punct' && token.value === '{') {
        operations.push({ kind: 'Operation', operation: 'query', name: null, variables: [], selections: this.selectionSet(), loc: token.loc });
        continue;
      }
      if (token.kind !== 'name') throw new GraphQLSyntaxError(`Unexpected ${JSON.stringify(token.value)}`, token.loc);
      if (token.value === 'fragment') {
        this.next();
        const name = this.name();
        const on = this.name();
        if (on !== 'on') throw new GraphQLSyntaxError('Expected "on"', this.peek().loc);
        const typeCondition = this.name();
        this.directives();
        fragments.set(name, { kind: 'Fragment', name, typeCondition, selections: this.selectionSet() });
        continue;
      }
      if (token.value === 'query' || token.value === 'mutation' || token.value === 'subscription') {
        this.next();
        let name: string | null = null;
        if (this.peek().kind === 'name') name = this.name();
        const variables: Operation['variables'] = [];
        if (this.is('(')) {
          this.next();
          while (!this.is(')')) {
            this.expect('$');
            const variable = this.name();
            this.expect(':');
            const type = this.type();
            let defaultValue: ValueNode | null = null;
            if (this.is('=')) {
              this.next();
              defaultValue = this.value(true);
            }
            this.directives();
            variables.push({ name: variable, type, defaultValue });
          }
          this.expect(')');
        }
        this.directives();
        operations.push({ kind: 'Operation', operation: token.value, name, variables, selections: this.selectionSet(), loc: token.loc });
        continue;
      }
      throw new GraphQLSyntaxError(`Unexpected ${JSON.stringify(token.value)}`, token.loc);
    }
    return { operations, fragments };
  }

  private type(): TypeNode {
    let type: TypeNode;
    if (this.is('[')) {
      this.next();
      type = { kind: 'List', of: this.type() };
      this.expect(']');
    } else {
      type = { kind: 'Named', name: this.name() };
    }
    if (this.is('!')) {
      this.next();
      type = { kind: 'NonNull', of: type };
    }
    return type;
  }

  private selectionSet(): Selection[] {
    this.expect('{');
    const selections: Selection[] = [];
    while (!this.is('}')) {
      if (this.peek().kind === 'eof') throw new GraphQLSyntaxError('Expected "}", found <EOF>', this.peek().loc);
      selections.push(this.selection());
    }
    this.expect('}');
    return selections;
  }

  private selection(): Selection {
    const start = this.peek();
    if (this.is('...')) {
      this.next();
      const token = this.peek();
      if (token.kind === 'name' && token.value === 'on') {
        this.next();
        const typeCondition = this.name();
        const directives = this.directives();
        return { kind: 'InlineFragment', typeCondition, directives, selections: this.selectionSet(), loc: start.loc };
      }
      if (token.kind === 'name') {
        const name = this.name();
        return { kind: 'FragmentSpread', name, directives: this.directives(), loc: start.loc };
      }
      const directives = this.directives();
      return { kind: 'InlineFragment', typeCondition: null, directives, selections: this.selectionSet(), loc: start.loc };
    }
    let name = this.name();
    let alias: string | null = null;
    if (this.is(':')) {
      this.next();
      alias = name;
      name = this.name();
    }
    const args = this.is('(') ? this.arguments() : [];
    const directives = this.directives();
    const selections = this.is('{') ? this.selectionSet() : null;
    return { kind: 'Field', alias, name, args, directives, selections, loc: start.loc };
  }

  private arguments(): Array<{ name: string; value: ValueNode }> {
    this.expect('(');
    const args: Array<{ name: string; value: ValueNode }> = [];
    while (!this.is(')')) {
      const name = this.name();
      this.expect(':');
      args.push({ name, value: this.value(false) });
    }
    this.expect(')');
    return args;
  }

  private directives(): Directive[] {
    const out: Directive[] = [];
    while (this.is('@')) {
      this.next();
      const name = this.name();
      out.push({ name, args: this.is('(') ? this.arguments() : [] });
    }
    return out;
  }

  private value(isConst: boolean): ValueNode {
    const token = this.next();
    if (token.kind === 'punct') {
      if (token.value === '$' && !isConst) return { kind: 'Variable', name: this.name() };
      if (token.value === '[') {
        const values: ValueNode[] = [];
        while (!this.is(']')) values.push(this.value(isConst));
        this.expect(']');
        return { kind: 'List', values };
      }
      if (token.value === '{') {
        const fields: Array<{ name: string; value: ValueNode }> = [];
        while (!this.is('}')) {
          const name = this.name();
          this.expect(':');
          fields.push({ name, value: this.value(isConst) });
        }
        this.expect('}');
        return { kind: 'Object', fields };
      }
      throw new GraphQLSyntaxError(`Unexpected ${JSON.stringify(token.value)}`, token.loc);
    }
    if (token.kind === 'int') return { kind: 'Int', value: token.value };
    if (token.kind === 'float') return { kind: 'Float', value: token.value };
    if (token.kind === 'string') return { kind: 'String', value: token.value };
    if (token.kind === 'name') {
      if (token.value === 'true' || token.value === 'false') return { kind: 'Boolean', value: token.value === 'true' };
      if (token.value === 'null') return { kind: 'Null' };
      return { kind: 'Enum', value: token.value };
    }
    throw new GraphQLSyntaxError('Unexpected <EOF>', token.loc);
  }
}

// ================================================================ the schema

type Resolver = (src: any, args: Record<string, any>, ctx: Ctx) => unknown;

interface ArgDef {
  type: string;
  defaultValue?: unknown;
}

interface FieldDef {
  type: string;
  args: Record<string, ArgDef>;
  description: string;
  resolve?: Resolver;
}

interface TypeDef {
  kind: 'OBJECT' | 'INTERFACE' | 'UNION' | 'ENUM' | 'SCALAR' | 'INPUT_OBJECT';
  name: string;
  description: string;
  fields: Record<string, FieldDef>;
  interfaces: string[];
  possibleTypes: string[];
  enumValues: string[];
  inputFields: Record<string, ArgDef>;
}

const TYPES = new Map<string, TypeDef>();

function typeDef(kind: TypeDef['kind'], name: string, description: string, extra: Partial<TypeDef> = {}): TypeDef {
  const def: TypeDef = { kind, name, description, fields: {}, interfaces: [], possibleTypes: [], enumValues: [], inputFields: {}, ...extra };
  TYPES.set(name, def);
  return def;
}

type FieldSpec = string | [string, Record<string, string | ArgDef>?, Resolver?];

/** Declare an object or interface type's fields: `name: 'Type!'` or `name: ['Type', {arg: 'Int'}, resolver]`. */
function fields(def: TypeDef, specs: Record<string, FieldSpec>): void {
  for (const [name, spec] of Object.entries(specs)) {
    const [type, rawArgs, resolve] = typeof spec === 'string' ? [spec, undefined, undefined] : spec;
    const args: Record<string, ArgDef> = {};
    for (const [argName, arg] of Object.entries(rawArgs ?? {})) args[argName] = typeof arg === 'string' ? { type: arg } : arg;
    def.fields[name] = { type, args, description: `The ${name} of the ${def.name}.`, resolve };
  }
}

for (const scalar of ['String', 'Int', 'Float', 'Boolean', 'ID', 'DateTime', 'URI', 'GitObjectID', 'HTML', 'GitTimestamp', 'GitSSHRemote', 'X509Certificate', 'Base64String']) {
  typeDef('SCALAR', scalar, `The ${scalar} scalar.`);
}

const ENUMS: Record<string, string[]> = {
  IssueState: ['OPEN', 'CLOSED'],
  IssueStateReason: ['COMPLETED', 'NOT_PLANNED', 'REOPENED', 'DUPLICATE'],
  IssueClosedStateReason: ['COMPLETED', 'NOT_PLANNED', 'DUPLICATE'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  PullRequestMergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
  SearchType: ['ISSUE', 'ISSUE_ADVANCED', 'REPOSITORY', 'USER', 'DISCUSSION'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  PullRequestOrderField: ['CREATED_AT', 'UPDATED_AT'],
  RepositoryOrderField: ['CREATED_AT', 'UPDATED_AT', 'PUSHED_AT', 'NAME', 'STARGAZERS'],
  RefOrderField: ['TAG_COMMIT_DATE', 'ALPHABETICAL'],
  OrderDirection: ['ASC', 'DESC'],
  RepositoryVisibility: ['PRIVATE', 'PUBLIC', 'INTERNAL'],
  PatchStatus: ['ADDED', 'DELETED', 'RENAMED', 'COPIED', 'MODIFIED', 'CHANGED'],
  CommentAuthorAssociation: ['OWNER', 'MEMBER', 'COLLABORATOR', 'CONTRIBUTOR', 'NONE'],
  PullRequestReviewDecision: ['APPROVED', 'CHANGES_REQUESTED', 'REVIEW_REQUIRED'],
  __TypeKind: ['SCALAR', 'OBJECT', 'INTERFACE', 'UNION', 'ENUM', 'INPUT_OBJECT', 'LIST', 'NON_NULL'],
};
for (const [name, values] of Object.entries(ENUMS)) typeDef('ENUM', name, `The ${name} enum.`, { enumValues: values });

const INPUTS: Record<string, Record<string, string>> = {
  IssueOrder: { field: 'IssueOrderField!', direction: 'OrderDirection!' },
  IssueFilters: { states: '[IssueState!]', labels: '[String!]', assignee: 'String', createdBy: 'String', since: 'DateTime' },
  PullRequestOrder: { field: 'PullRequestOrderField!', direction: 'OrderDirection!' },
  RepositoryOrder: { field: 'RepositoryOrderField!', direction: 'OrderDirection!' },
  RefOrder: { field: 'RefOrderField!', direction: 'OrderDirection!' },
  CreateIssueInput: { repositoryId: 'ID!', title: 'String!', body: 'String', labelIds: '[ID!]', assigneeIds: '[ID!]', milestoneId: 'ID', clientMutationId: 'String' },
  CloseIssueInput: { issueId: 'ID!', stateReason: 'IssueClosedStateReason', clientMutationId: 'String' },
  ReopenIssueInput: { issueId: 'ID!', clientMutationId: 'String' },
  UpdateIssueInput: { id: 'ID!', title: 'String', body: 'String', state: 'IssueState', labelIds: '[ID!]', clientMutationId: 'String' },
  CreatePullRequestInput: { repositoryId: 'ID!', baseRefName: 'String!', headRefName: 'String!', title: 'String!', body: 'String', draft: 'Boolean', maintainerCanModify: 'Boolean', headRepositoryId: 'ID', clientMutationId: 'String' },
  MergePullRequestInput: { pullRequestId: 'ID!', mergeMethod: 'PullRequestMergeMethod', commitHeadline: 'String', commitBody: 'String', expectedHeadOid: 'GitObjectID', authorEmail: 'String', clientMutationId: 'String' },
  ClosePullRequestInput: { pullRequestId: 'ID!', clientMutationId: 'String' },
  ReopenPullRequestInput: { pullRequestId: 'ID!', clientMutationId: 'String' },
  UpdatePullRequestInput: { pullRequestId: 'ID!', title: 'String', body: 'String', baseRefName: 'String', state: 'PullRequestUpdateState', clientMutationId: 'String' },
  AddCommentInput: { subjectId: 'ID!', body: 'String!', clientMutationId: 'String' },
  CreateRepositoryInput: { name: 'String!', description: 'String', visibility: 'RepositoryVisibility!', ownerId: 'ID', hasIssuesEnabled: 'Boolean', clientMutationId: 'String' },
  CreateRefInput: { repositoryId: 'ID!', name: 'String!', oid: 'GitObjectID!', clientMutationId: 'String' },
  AddLabelsToLabelableInput: { labelableId: 'ID!', labelIds: '[ID!]!', clientMutationId: 'String' },
};
typeDef('ENUM', 'PullRequestUpdateState', 'The PullRequestUpdateState enum.', { enumValues: ['OPEN', 'CLOSED'] });
for (const [name, inputFields] of Object.entries(INPUTS)) {
  const def = typeDef('INPUT_OBJECT', name, `The ${name} input.`);
  for (const [field, type] of Object.entries(inputFields)) def.inputFields[field] = { type };
}

// ---------------------------------------------------------------- execution context and sources

interface Ctx {
  readonly forge: Forge;
  readonly origin: string;
  readonly variables: Record<string, unknown>;
  readonly fragments: Map<string, Fragment>;
  readonly errors: GqlError[];
}

interface GqlError {
  message: string;
  locations?: Loc[];
  path?: Array<string | number>;
  type?: string;
  extensions?: Record<string, unknown>;
}

class FieldError extends Error {
  readonly type: string;
  constructor(type: string, message: string) {
    super(message);
    this.type = type;
  }
}

interface RepoSrc {
  __typename: 'Repository';
  repo: Repo;
}
interface IssueSrc {
  __typename: 'Issue';
  repo: Repo;
  issue: Issue;
}
interface PullSrc {
  __typename: 'PullRequest';
  repo: Repo;
  pull: Pull;
}
interface UserSrc {
  __typename: 'User';
  user: User;
}

const repoSrc = (repo: Repo): RepoSrc => ({ __typename: 'Repository', repo });
const issueSrc = (repo: Repo, issue: Issue): IssueSrc => ({ __typename: 'Issue', repo, issue });
const pullSrc = (repo: Repo, pull: Pull): PullSrc => ({ __typename: 'PullRequest', repo, pull });
const userSrc = (user: User): UserSrc => ({ __typename: 'User', user });
const labelSrc = (repo: Repo, label: Label) => ({ __typename: 'Label', repo, label });
const commentSrc = (repo: Repo, comment: Comment) => ({ __typename: 'IssueComment', repo, comment });
const commitSrc = (repo: Repo, oid: string) => ({ __typename: 'Commit', repo, oid });
const refSrc = (repo: Repo, name: string, oid: string) => ({ __typename: 'Ref', repo, name, oid });

function encodeCursor(index: number): string {
  return Buffer.from(`cursor:v2:${index}`, 'utf8').toString('base64');
}

function decodeCursor(cursor: unknown): number | null {
  if (typeof cursor !== 'string' || cursor === '') return null;
  const match = /^cursor:v2:(\d+)$/.exec(Buffer.from(cursor, 'base64').toString('utf8'));
  if (!match) throw new FieldError('INVALID_CURSOR_ARGUMENTS', `\`${cursor}\` does not appear to be a valid cursor.`);
  return Number(match[1]);
}

/** A Relay connection over a fully materialised list. */
function connection<T>(typename: string, items: readonly T[], args: Record<string, any>, toNode: (item: T) => unknown): Record<string, unknown> {
  let start = 0;
  let end = items.length;
  const after = decodeCursor(args.after);
  const before = decodeCursor(args.before);
  if (after !== null) start = Math.max(start, after + 1);
  if (before !== null) end = Math.min(end, before);
  if (typeof args.first === 'number') {
    if (args.first < 0) throw new FieldError('INVALID_PAGINATION', '`first` must be non-negative');
    if (args.first > 100) throw new FieldError('EXCESSIVE_PAGINATION', `Requesting ${args.first} records on the connection exceeds the \`first\` limit of 100 records.`);
    end = Math.min(end, start + args.first);
  }
  if (typeof args.last === 'number') start = Math.max(start, end - args.last);
  const indexes: number[] = [];
  for (let index = start; index < end; index += 1) indexes.push(index);
  const nodes = indexes.map((index) => toNode(items[index]));
  return {
    __typename: typename,
    totalCount: items.length,
    nodes,
    edges: indexes.map((index, at) => ({ __typename: `${typename.replace(/Connection$/, '')}Edge`, cursor: encodeCursor(index), node: nodes[at] })),
    pageInfo: {
      __typename: 'PageInfo',
      hasNextPage: end < items.length,
      hasPreviousPage: start > 0,
      startCursor: indexes.length > 0 ? encodeCursor(indexes[0]) : null,
      endCursor: indexes.length > 0 ? encodeCursor(indexes[indexes.length - 1]) : null,
    },
  };
}

function htmlUrl(ctx: Ctx, repo: Repo, tail = ''): string {
  return `${ctx.origin}/${repo.owner.login}/${repo.name}${tail}`;
}

function orderIssuesLike<T extends { number: number; createdAt: string; updatedAt: string; comments: Comment[] }>(list: T[], orderBy: any): T[] {
  const field = orderBy?.field ?? 'CREATED_AT';
  const direction = orderBy?.direction ?? 'ASC';
  const key = (item: T): string | number => (field === 'UPDATED_AT' ? item.updatedAt : field === 'COMMENTS' ? item.comments.length : item.createdAt);
  list.sort((a, b) => {
    const ka = key(a);
    const kb = key(b);
    if (ka !== kb) return ka < kb ? -1 : 1;
    return a.number - b.number;
  });
  if (direction === 'DESC') list.reverse();
  return list;
}

function pullState(pull: Pull): string {
  return pull.merged ? 'MERGED' : pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

// ---------------------------------------------------------------- object types

const connectionArgs = { first: 'Int', last: 'Int', after: 'String', before: 'String' };

const Node = typeDef('INTERFACE', 'Node', 'An object with an ID.', { possibleTypes: ['Repository', 'Issue', 'PullRequest', 'User', 'IssueComment', 'Label', 'Ref', 'Commit'] });
fields(Node, { id: 'ID!' });
const Actor = typeDef('INTERFACE', 'Actor', 'An object that can take actions.', { possibleTypes: ['User'] });
fields(Actor, { login: 'String!', avatarUrl: ['URI!', { size: 'Int' }], url: 'URI!', resourcePath: 'URI!' });
const RepositoryOwner = typeDef('INTERFACE', 'RepositoryOwner', 'An object that can own repositories.', { possibleTypes: ['User'] });
fields(RepositoryOwner, { id: 'ID!', login: 'String!', url: 'URI!', avatarUrl: ['URI!', { size: 'Int' }], repository: ['Repository', { name: 'String!' }] });
const GitObject = typeDef('INTERFACE', 'GitObject', 'A git object.', { possibleTypes: ['Commit'] });
fields(GitObject, { id: 'ID!', oid: 'GitObjectID!', abbreviatedOid: 'String!', commitUrl: 'URI!', repository: 'Repository!' });
typeDef('UNION', 'IssueOrPullRequest', 'An issue or a pull request.', { possibleTypes: ['Issue', 'PullRequest'] });
typeDef('UNION', 'SearchResultItem', 'A search result.', { possibleTypes: ['Issue', 'PullRequest', 'Repository', 'User'] });

const PageInfo = typeDef('OBJECT', 'PageInfo', 'Information about pagination in a connection.');
fields(PageInfo, { hasNextPage: 'Boolean!', hasPreviousPage: 'Boolean!', startCursor: 'String', endCursor: 'String' });

function connectionTypes(node: string): void {
  const conn = typeDef('OBJECT', `${node}Connection`, `The connection type for ${node}.`);
  fields(conn, { nodes: `[${node}]`, edges: `[${node}Edge]`, pageInfo: 'PageInfo!', totalCount: 'Int!' });
  const edge = typeDef('OBJECT', `${node}Edge`, `An edge in a ${node} connection.`);
  fields(edge, { cursor: 'String!', node: node });
}
for (const node of ['Repository', 'Issue', 'PullRequest', 'Label', 'IssueComment', 'User', 'Ref', 'PullRequestChangedFile', 'PullRequestCommit', 'Commit']) connectionTypes(node);
const SearchConn = typeDef('OBJECT', 'SearchResultItemConnection', 'Search results.');
fields(SearchConn, { nodes: '[SearchResultItem]', edges: '[SearchResultItemEdge]', pageInfo: 'PageInfo!', issueCount: 'Int!', repositoryCount: 'Int!', userCount: 'Int!', discussionCount: 'Int!', codeCount: 'Int!', wikiCount: 'Int!' });
fields(typeDef('OBJECT', 'SearchResultItemEdge', 'A search result edge.'), { cursor: 'String!', node: 'SearchResultItem' });

const UserType = typeDef('OBJECT', 'User', 'A user.', { interfaces: ['Node', 'Actor', 'RepositoryOwner'] });
fields(UserType, {
  id: ['ID!', {}, (src: UserSrc) => src.user.nodeId],
  databaseId: ['Int', {}, (src: UserSrc) => src.user.id],
  login: ['String!', {}, (src: UserSrc) => src.user.login],
  name: ['String', {}, (src: UserSrc) => src.user.name],
  email: ['String!', {}, (src: UserSrc) => src.user.email],
  bio: ['String', {}, () => null],
  company: ['String', {}, () => null],
  location: ['String', {}, () => null],
  isViewer: ['Boolean!', {}, (src: UserSrc, _a, ctx) => src.user === ctx.forge.viewer],
  url: ['URI!', {}, (src: UserSrc, _a, ctx) => `${ctx.origin}/${src.user.login}`],
  resourcePath: ['URI!', {}, (src: UserSrc) => `/${src.user.login}`],
  avatarUrl: ['URI!', { size: 'Int' }, (src: UserSrc, _a, ctx) => `${ctx.origin}/avatars/${src.user.login}`],
  createdAt: ['DateTime!', {}, () => '2026-01-01T00:00:00Z'],
  repository: ['Repository', { name: 'String!' }, (src: UserSrc, args, ctx) => {
    const repo = ctx.forge.getRepo(src.user.login, args.name);
    return repo === undefined ? null : repoSrc(repo);
  }],
  repositories: ['RepositoryConnection!', { ...connectionArgs, orderBy: 'RepositoryOrder', privacy: 'RepositoryVisibility', ownerAffiliations: '[String]', isFork: 'Boolean' }, (src: UserSrc, args, ctx) => {
    let repos = ctx.forge.reposOf(src.user);
    if (args.privacy === 'PRIVATE') repos = repos.filter((repo) => repo.private);
    if (args.privacy === 'PUBLIC') repos = repos.filter((repo) => !repo.private);
    repos.sort((a, b) => a.id - b.id);
    if (args.orderBy?.field === 'NAME') repos.sort((a, b) => (a.name.toLowerCase() < b.name.toLowerCase() ? -1 : 1));
    if (args.orderBy?.direction === 'DESC') repos.reverse();
    return connection('RepositoryConnection', repos, args, repoSrc);
  }],
});

const RepositoryType = typeDef('OBJECT', 'Repository', 'A repository contains the content for a project.', { interfaces: ['Node'] });
fields(RepositoryType, {
  id: ['ID!', {}, (src: RepoSrc) => src.repo.nodeId],
  databaseId: ['Int', {}, (src: RepoSrc) => src.repo.id],
  name: ['String!', {}, (src: RepoSrc) => src.repo.name],
  nameWithOwner: ['String!', {}, (src: RepoSrc) => `${src.repo.owner.login}/${src.repo.name}`],
  owner: ['RepositoryOwner!', {}, (src: RepoSrc) => userSrc(src.repo.owner)],
  description: ['String', {}, (src: RepoSrc) => src.repo.description],
  homepageUrl: ['URI', {}, () => null],
  url: ['URI!', {}, (src: RepoSrc, _a, ctx) => htmlUrl(ctx, src.repo)],
  sshUrl: ['GitSSHRemote!', {}, (src: RepoSrc) => `git@forge:${src.repo.owner.login}/${src.repo.name}.git`],
  resourcePath: ['URI!', {}, (src: RepoSrc) => `/${src.repo.owner.login}/${src.repo.name}`],
  isPrivate: ['Boolean!', {}, (src: RepoSrc) => src.repo.private],
  visibility: ['RepositoryVisibility!', {}, (src: RepoSrc) => (src.repo.private ? 'PRIVATE' : 'PUBLIC')],
  isFork: ['Boolean!', {}, () => false],
  isArchived: ['Boolean!', {}, () => false],
  isTemplate: ['Boolean!', {}, () => false],
  isEmpty: ['Boolean!', {}, (src: RepoSrc) => src.repo.refs.size === 0],
  isInOrganization: ['Boolean!', {}, () => false],
  isMirror: ['Boolean!', {}, () => false],
  isLocked: ['Boolean!', {}, () => false],
  isDisabled: ['Boolean!', {}, () => false],
  createdAt: ['DateTime!', {}, (src: RepoSrc) => src.repo.createdAt],
  updatedAt: ['DateTime!', {}, (src: RepoSrc) => src.repo.updatedAt],
  pushedAt: ['DateTime', {}, (src: RepoSrc) => src.repo.pushedAt],
  diskUsage: ['Int', {}, () => 0],
  forkCount: ['Int!', {}, () => 0],
  stargazerCount: ['Int!', {}, () => 0],
  viewerHasStarred: ['Boolean!', {}, () => false],
  viewerPermission: ['RepositoryPermission', {}, () => 'ADMIN'],
  viewerCanAdminister: ['Boolean!', {}, () => true],
  viewerDefaultMergeMethod: ['PullRequestMergeMethod!', {}, () => 'MERGE'],
  hasIssuesEnabled: ['Boolean!', {}, (src: RepoSrc) => src.repo.hasIssues],
  hasWikiEnabled: ['Boolean!', {}, () => false],
  hasProjectsEnabled: ['Boolean!', {}, () => false],
  hasDiscussionsEnabled: ['Boolean!', {}, () => false],
  mergeCommitAllowed: ['Boolean!', {}, () => true],
  squashMergeAllowed: ['Boolean!', {}, () => true],
  rebaseMergeAllowed: ['Boolean!', {}, () => true],
  deleteBranchOnMerge: ['Boolean!', {}, () => false],
  primaryLanguage: ['Language', {}, () => null],
  parent: ['Repository', {}, () => null],
  defaultBranchRef: ['Ref', {}, (src: RepoSrc, _a, ctx) => {
    const name = `refs/heads/${src.repo.defaultBranch}`;
    const oid = src.repo.refs.get(name);
    return oid === undefined ? null : refSrc(src.repo, name, oid);
  }],
  ref: ['Ref', { qualifiedName: 'String!' }, (src: RepoSrc, args) => {
    const name: string = args.qualifiedName.startsWith('refs/') ? args.qualifiedName : `refs/heads/${args.qualifiedName}`;
    const oid = src.repo.refs.get(name) ?? (args.qualifiedName.startsWith('refs/') ? undefined : src.repo.refs.get(`refs/tags/${args.qualifiedName}`));
    const resolvedName = src.repo.refs.has(name) ? name : `refs/tags/${args.qualifiedName}`;
    return oid === undefined ? null : refSrc(src.repo, resolvedName, oid);
  }],
  refs: ['RefConnection', { ...connectionArgs, refPrefix: 'String!', query: 'String', orderBy: 'RefOrder', direction: 'OrderDirection' }, (src: RepoSrc, args) => {
    const prefix: string = args.refPrefix;
    const list = [...src.repo.refs.entries()].filter(([name]) => name.startsWith(prefix) && (args.query == null || name.includes(args.query))).sort(([a], [b]) => (a < b ? -1 : 1));
    return connection('RefConnection', list, args, ([name, oid]) => refSrc(src.repo, name, oid));
  }],
  object: ['GitObject', { expression: 'String', oid: 'GitObjectID' }, (src: RepoSrc, args, ctx) => {
    const target = args.oid ?? args.expression;
    if (typeof target !== 'string') return null;
    const oid = ctx.forge.resolve(src.repo, target.replace(/:.*$/, ''));
    return oid === null ? null : commitSrc(src.repo, oid);
  }],
  issue: ['Issue', { number: 'Int!' }, (src: RepoSrc, args) => {
    const issue = src.repo.issues.get(args.number);
    return issue === undefined ? null : issueSrc(src.repo, issue);
  }],
  issueOrPullRequest: ['IssueOrPullRequest', { number: 'Int!' }, (src: RepoSrc, args) => {
    const issue = src.repo.issues.get(args.number);
    if (issue !== undefined) return issueSrc(src.repo, issue);
    const pull = src.repo.pulls.get(args.number);
    return pull === undefined ? null : pullSrc(src.repo, pull);
  }],
  issues: ['IssueConnection!', { ...connectionArgs, states: '[IssueState!]', labels: '[String!]', orderBy: 'IssueOrder', filterBy: 'IssueFilters' }, (src: RepoSrc, args) => {
    const states: string[] | undefined = args.states ?? args.filterBy?.states;
    const labels: string[] = args.labels ?? args.filterBy?.labels ?? [];
    let list = filterIssues(src.repo, 'all', labels);
    if (Array.isArray(states) && states.length > 0) list = list.filter((issue) => states.includes(issue.state.toUpperCase()));
    orderIssuesLike(list, args.orderBy);
    return connection('IssueConnection', list, args, (issue) => issueSrc(src.repo, issue));
  }],
  pullRequest: ['PullRequest', { number: 'Int!' }, (src: RepoSrc, args) => {
    const pull = src.repo.pulls.get(args.number);
    return pull === undefined ? null : pullSrc(src.repo, pull);
  }],
  pullRequests: ['PullRequestConnection!', { ...connectionArgs, states: '[PullRequestState!]', labels: '[String!]', headRefName: 'String', baseRefName: 'String', orderBy: 'IssueOrder' }, (src: RepoSrc, args) => {
    let list = [...src.repo.pulls.values()];
    if (Array.isArray(args.states) && args.states.length > 0) list = list.filter((pull) => args.states.includes(pullState(pull)));
    if (typeof args.headRefName === 'string') list = list.filter((pull) => pull.head === args.headRefName);
    if (typeof args.baseRefName === 'string') list = list.filter((pull) => pull.base === args.baseRefName);
    orderIssuesLike(list, args.orderBy);
    return connection('PullRequestConnection', list, args, (pull) => pullSrc(src.repo, pull));
  }],
  labels: ['LabelConnection', { ...connectionArgs, query: 'String', orderBy: 'IssueOrder' }, (src: RepoSrc, args) => {
    const list = [...src.repo.labels.values()].filter((label) => args.query == null || label.name.toLowerCase().includes(String(args.query).toLowerCase()));
    return connection('LabelConnection', list, args, (label) => labelSrc(src.repo, label));
  }],
  label: ['Label', { name: 'String!' }, (src: RepoSrc, args) => {
    const label = src.repo.labels.get(String(args.name).toLowerCase());
    return label === undefined ? null : labelSrc(src.repo, label);
  }],
});
typeDef('OBJECT', 'Language', 'A programming language.').fields.name = { type: 'String!', args: {}, description: 'The name.' };

const IssueType = typeDef('OBJECT', 'Issue', 'An issue in a repository.', { interfaces: ['Node'] });
fields(IssueType, {
  id: ['ID!', {}, (src: IssueSrc) => src.issue.nodeId],
  databaseId: ['Int', {}, (src: IssueSrc) => src.issue.id],
  number: ['Int!', {}, (src: IssueSrc) => src.issue.number],
  title: ['String!', {}, (src: IssueSrc) => src.issue.title],
  body: ['String!', {}, (src: IssueSrc) => src.issue.body],
  bodyText: ['String!', {}, (src: IssueSrc) => src.issue.body],
  bodyHTML: ['HTML!', {}, (src: IssueSrc) => src.issue.body],
  state: ['IssueState!', {}, (src: IssueSrc) => src.issue.state.toUpperCase()],
  stateReason: ['IssueStateReason', {}, (src: IssueSrc) => (src.issue.stateReason === null ? null : src.issue.stateReason.toUpperCase())],
  closed: ['Boolean!', {}, (src: IssueSrc) => src.issue.state === 'closed'],
  closedAt: ['DateTime', {}, (src: IssueSrc) => src.issue.closedAt],
  createdAt: ['DateTime!', {}, (src: IssueSrc) => src.issue.createdAt],
  updatedAt: ['DateTime!', {}, (src: IssueSrc) => src.issue.updatedAt],
  locked: ['Boolean!', {}, () => false],
  url: ['URI!', {}, (src: IssueSrc, _a, ctx) => htmlUrl(ctx, src.repo, `/issues/${src.issue.number}`)],
  resourcePath: ['URI!', {}, (src: IssueSrc) => `/${src.repo.owner.login}/${src.repo.name}/issues/${src.issue.number}`],
  author: ['Actor', {}, (src: IssueSrc) => userSrc(src.issue.user)],
  authorAssociation: ['CommentAuthorAssociation!', {}, () => 'OWNER'],
  repository: ['Repository!', {}, (src: IssueSrc) => repoSrc(src.repo)],
  viewerCanUpdate: ['Boolean!', {}, () => true],
  labels: ['LabelConnection', { ...connectionArgs, orderBy: 'IssueOrder' }, (src: IssueSrc, args) => connection('LabelConnection', src.issue.labels, args, (label) => labelSrc(src.repo, label))],
  comments: ['IssueCommentConnection!', { ...connectionArgs, orderBy: 'IssueOrder' }, (src: IssueSrc, args) => connection('IssueCommentConnection', src.issue.comments, args, (comment) => commentSrc(src.repo, comment))],
  assignees: ['UserConnection!', connectionArgs, (_src: IssueSrc, args) => connection('UserConnection', [], args, userSrc)],
  participants: ['UserConnection!', connectionArgs, (src: IssueSrc, args) => connection('UserConnection', [src.issue.user], args, userSrc)],
});

const PullType = typeDef('OBJECT', 'PullRequest', 'A repository pull request.', { interfaces: ['Node'] });
fields(PullType, {
  id: ['ID!', {}, (src: PullSrc) => src.pull.nodeId],
  databaseId: ['Int', {}, (src: PullSrc) => src.pull.id],
  number: ['Int!', {}, (src: PullSrc) => src.pull.number],
  title: ['String!', {}, (src: PullSrc) => src.pull.title],
  body: ['String!', {}, (src: PullSrc) => src.pull.body],
  bodyText: ['String!', {}, (src: PullSrc) => src.pull.body],
  state: ['PullRequestState!', {}, (src: PullSrc) => pullState(src.pull)],
  closed: ['Boolean!', {}, (src: PullSrc) => src.pull.state === 'closed'],
  merged: ['Boolean!', {}, (src: PullSrc) => src.pull.merged],
  mergedAt: ['DateTime', {}, (src: PullSrc) => src.pull.mergedAt],
  mergedBy: ['Actor', {}, (src: PullSrc, _a, ctx) => (src.pull.merged ? userSrc(ctx.forge.viewer) : null)],
  closedAt: ['DateTime', {}, (src: PullSrc) => src.pull.closedAt],
  createdAt: ['DateTime!', {}, (src: PullSrc) => src.pull.createdAt],
  updatedAt: ['DateTime!', {}, (src: PullSrc) => src.pull.updatedAt],
  isDraft: ['Boolean!', {}, (src: PullSrc) => src.pull.draft],
  locked: ['Boolean!', {}, () => false],
  isCrossRepository: ['Boolean!', {}, () => false],
  maintainerCanModify: ['Boolean!', {}, () => false],
  mergeable: ['MergeableState!', {}, (src: PullSrc, _a, ctx) => {
    const result = ctx.forge.mergeable(src.repo, src.pull);
    return result === null ? 'UNKNOWN' : result ? 'MERGEABLE' : 'CONFLICTING';
  }],
  reviewDecision: ['PullRequestReviewDecision', {}, () => null],
  headRefName: ['String!', {}, (src: PullSrc) => src.pull.head],
  baseRefName: ['String!', {}, (src: PullSrc) => src.pull.base],
  headRefOid: ['GitObjectID!', {}, (src: PullSrc, _a, ctx) => ctx.forge.pullHeadSha(src.repo, src.pull)],
  baseRefOid: ['GitObjectID!', {}, (src: PullSrc, _a, ctx) => ctx.forge.pullBaseSha(src.repo, src.pull)],
  headRef: ['Ref', {}, (src: PullSrc, _a, ctx) => {
    const oid = ctx.forge.branchTip(src.repo, src.pull.head);
    return oid === null ? null : refSrc(src.repo, `refs/heads/${src.pull.head}`, oid);
  }],
  baseRef: ['Ref', {}, (src: PullSrc, _a, ctx) => {
    const oid = ctx.forge.branchTip(src.repo, src.pull.base);
    return oid === null ? null : refSrc(src.repo, `refs/heads/${src.pull.base}`, oid);
  }],
  headRepository: ['Repository', {}, (src: PullSrc) => repoSrc(src.repo)],
  headRepositoryOwner: ['RepositoryOwner', {}, (src: PullSrc) => userSrc(src.repo.owner)],
  baseRepository: ['Repository', {}, (src: PullSrc) => repoSrc(src.repo)],
  repository: ['Repository!', {}, (src: PullSrc) => repoSrc(src.repo)],
  url: ['URI!', {}, (src: PullSrc, _a, ctx) => htmlUrl(ctx, src.repo, `/pull/${src.pull.number}`)],
  permalink: ['URI!', {}, (src: PullSrc, _a, ctx) => htmlUrl(ctx, src.repo, `/pull/${src.pull.number}`)],
  resourcePath: ['URI!', {}, (src: PullSrc) => `/${src.repo.owner.login}/${src.repo.name}/pull/${src.pull.number}`],
  author: ['Actor', {}, (src: PullSrc) => userSrc(src.pull.user)],
  authorAssociation: ['CommentAuthorAssociation!', {}, () => 'OWNER'],
  mergeCommit: ['Commit', {}, (src: PullSrc) => (src.pull.mergeCommitSha === null ? null : commitSrc(src.repo, src.pull.mergeCommitSha))],
  additions: ['Int!', {}, (src: PullSrc, _a, ctx) => changedFiles(ctx, src).reduce((sum, file) => sum + file.additions, 0)],
  deletions: ['Int!', {}, (src: PullSrc, _a, ctx) => changedFiles(ctx, src).reduce((sum, file) => sum + file.deletions, 0)],
  changedFiles: ['Int!', {}, (src: PullSrc, _a, ctx) => changedFiles(ctx, src).length],
  files: ['PullRequestChangedFileConnection', connectionArgs, (src: PullSrc, args, ctx) => connection('PullRequestChangedFileConnection', changedFiles(ctx, src), args, (file) => file)],
  commits: ['PullRequestCommitConnection!', connectionArgs, (src: PullSrc, args, ctx) =>
    connection('PullRequestCommitConnection', pullCommits(ctx.forge, src.repo, src.pull), args, (oid) => ({ __typename: 'PullRequestCommit', id: `PRC_${oid}`, commit: commitSrc(src.repo, oid), url: htmlUrl(ctx, src.repo, `/pull/${src.pull.number}/commits/${oid}`) }))],
  labels: ['LabelConnection', connectionArgs, (src: PullSrc, args) => connection('LabelConnection', src.pull.labels, args, (label) => labelSrc(src.repo, label))],
  comments: ['IssueCommentConnection!', connectionArgs, (src: PullSrc, args) => connection('IssueCommentConnection', src.pull.comments, args, (comment) => commentSrc(src.repo, comment))],
  assignees: ['UserConnection!', connectionArgs, (_src: PullSrc, args) => connection('UserConnection', [], args, userSrc)],
});

function changedFiles(ctx: Ctx, src: PullSrc): Array<{ __typename: string; path: string; additions: number; deletions: number; changeType: string }> {
  return ctx.forge.pullDiff(src.repo, src.pull).map((file) => {
    const before = file.oldOid === null ? null : ctx.forge.getObject(file.oldOid)?.data ?? null;
    const after = file.newOid === null ? null : ctx.forge.getObject(file.newOid)?.data ?? null;
    const stats = lineStats(before, after);
    return { __typename: 'PullRequestChangedFile', path: file.path, additions: stats.additions, deletions: stats.deletions, changeType: file.status === 'added' ? 'ADDED' : file.status === 'removed' ? 'DELETED' : 'MODIFIED' };
  });
}

fields(typeDef('OBJECT', 'PullRequestChangedFile', 'A file changed in a pull request.'), { path: 'String!', additions: 'Int!', deletions: 'Int!', changeType: 'PatchStatus!' });
fields(typeDef('OBJECT', 'PullRequestCommit', 'A commit of a pull request.', { interfaces: ['Node'] }), { id: 'ID!', commit: 'Commit!', url: 'URI!' });

fields(typeDef('OBJECT', 'Label', 'A label for an issue or pull request.', { interfaces: ['Node'] }), {
  id: ['ID!', {}, (src) => src.label.nodeId],
  name: ['String!', {}, (src) => src.label.name],
  color: ['String!', {}, (src) => src.label.color],
  description: ['String', {}, (src) => src.label.description],
  isDefault: ['Boolean!', {}, () => false],
  url: ['URI!', {}, (src, _a, ctx) => htmlUrl(ctx, src.repo, `/labels/${encodeURIComponent(src.label.name)}`)],
});

fields(typeDef('OBJECT', 'IssueComment', 'A comment on an issue or pull request.', { interfaces: ['Node'] }), {
  id: ['ID!', {}, (src) => src.comment.nodeId],
  databaseId: ['Int', {}, (src) => src.comment.id],
  body: ['String!', {}, (src) => src.comment.body],
  bodyText: ['String!', {}, (src) => src.comment.body],
  createdAt: ['DateTime!', {}, (src) => src.comment.createdAt],
  updatedAt: ['DateTime!', {}, (src) => src.comment.updatedAt],
  author: ['Actor', {}, (src) => userSrc(src.comment.user)],
  authorAssociation: ['CommentAuthorAssociation!', {}, () => 'OWNER'],
  url: ['URI!', {}, (src, _a, ctx) => htmlUrl(ctx, src.repo, `/issues/${src.comment.number}#issuecomment-${src.comment.id}`)],
  viewerCanUpdate: ['Boolean!', {}, () => true],
});

fields(typeDef('OBJECT', 'Ref', 'A git reference.', { interfaces: ['Node'] }), {
  id: ['ID!', {}, (src) => `REF_${Buffer.from(`ref:${src.repo.id}:${src.name}`).toString('base64url')}`],
  name: ['String!', {}, (src) => src.name.replace(/^refs\/(heads|tags)\//, '')],
  prefix: ['String!', {}, (src) => src.name.replace(/[^/]+$/, '').replace(/^(refs\/(heads|tags)\/).*$/, '$1')],
  target: ['GitObject', {}, (src, _a, ctx) => commitSrc(src.repo, ctx.forge.peel(src.oid))],
  repository: ['Repository!', {}, (src) => repoSrc(src.repo)],
});

function commitData(ctx: Ctx, src: { repo: Repo; oid: string }): Commit {
  const commit = ctx.forge.commitOf(src.oid);
  if (commit === null) throw new FieldError('NOT_FOUND', `Could not resolve to a commit with the oid ${src.oid}.`);
  return commit;
}

const signature = (sig: Commit['author']) => ({ __typename: 'GitActor', name: sig.name, email: sig.email, date: isoFromEpoch(sig.time) });
fields(typeDef('OBJECT', 'GitActor', 'Git actor information.'), { name: 'String', email: 'String', date: 'GitTimestamp' });
fields(typeDef('OBJECT', 'Tree', 'A git tree.'), { oid: 'GitObjectID!' });
fields(typeDef('OBJECT', 'Commit', 'A git commit.', { interfaces: ['Node', 'GitObject'] }), {
  id: ['ID!', {}, (src) => `C_${Buffer.from(`commit:${src.oid}`).toString('base64url')}`],
  oid: ['GitObjectID!', {}, (src) => src.oid],
  abbreviatedOid: ['String!', {}, (src) => src.oid.slice(0, 7)],
  commitUrl: ['URI!', {}, (src, _a, ctx) => htmlUrl(ctx, src.repo, `/commit/${src.oid}`)],
  url: ['URI!', {}, (src, _a, ctx) => htmlUrl(ctx, src.repo, `/commit/${src.oid}`)],
  repository: ['Repository!', {}, (src) => repoSrc(src.repo)],
  message: ['String!', {}, (src, _a, ctx) => commitData(ctx, src).message.replace(/\n$/, '')],
  messageHeadline: ['String!', {}, (src, _a, ctx) => commitData(ctx, src).message.split('\n')[0]],
  messageBody: ['String!', {}, (src, _a, ctx) => commitData(ctx, src).message.split('\n').slice(2).join('\n').replace(/\n$/, '')],
  committedDate: ['DateTime!', {}, (src, _a, ctx) => isoFromEpoch(commitData(ctx, src).committer.time)],
  authoredDate: ['DateTime!', {}, (src, _a, ctx) => isoFromEpoch(commitData(ctx, src).author.time)],
  author: ['GitActor', {}, (src, _a, ctx) => signature(commitData(ctx, src).author)],
  committer: ['GitActor', {}, (src, _a, ctx) => signature(commitData(ctx, src).committer)],
  tree: ['Tree!', {}, (src, _a, ctx) => ({ __typename: 'Tree', oid: commitData(ctx, src).tree })],
  parents: ['CommitConnection!', connectionArgs, (src, args, ctx) => connection('CommitConnection', commitData(ctx, src).parents, args, (oid) => commitSrc(src.repo, oid))],
  history: ['CommitConnection!', connectionArgs, (src, args, ctx) => connection('CommitConnection', ctx.forge.walkCommits([src.oid], 10_000), args, (oid) => commitSrc(src.repo, oid))],
});

fields(typeDef('OBJECT', 'RateLimit', 'The API rate limit.'), { limit: 'Int!', cost: 'Int!', remaining: 'Int!', resetAt: 'DateTime!', used: 'Int!', nodeCount: 'Int!' });

// ---------------------------------------------------------------- lookups by global id

function nodeById(ctx: Ctx, id: unknown): unknown {
  if (typeof id !== 'string') return null;
  const parsed = parseGlobalId(id);
  const forge = ctx.forge;
  if (parsed !== null) {
    switch (parsed.kind) {
      case 'repo': {
        const repo = forge.repoById(parsed.id);
        return repo === undefined ? null : repoSrc(repo);
      }
      case 'issue': {
        const found = forge.issuesById.get(parsed.id);
        return found === undefined ? null : issueSrc(found.repo, found.issue);
      }
      case 'pull': {
        const found = forge.pullsById.get(parsed.id);
        return found === undefined ? null : pullSrc(found.repo, found.pull);
      }
      case 'comment': {
        const found = forge.commentsById.get(parsed.id);
        return found === undefined ? null : commentSrc(found.repo, found.comment);
      }
      case 'user':
        return parsed.id === forge.viewer.id ? userSrc(forge.viewer) : null;
      case 'label':
        for (const repo of forge.reposOf(forge.viewer)) for (const label of repo.labels.values()) if (label.id === parsed.id) return labelSrc(repo, label);
        return null;
      default:
        return null;
    }
  }
  return null;
}

function need<T>(value: unknown, typename: string, id: unknown): T {
  if (value === null || (value as { __typename?: string }).__typename !== typename) {
    throw new FieldError('NOT_FOUND', `Could not resolve to a node with the global id of '${String(id)}'.`);
  }
  return value as T;
}

function wrapForge<T>(fn: () => T): T {
  try {
    return fn();
  } catch (err) {
    if (err instanceof ForgeError) throw new FieldError(err.status === 404 ? 'NOT_FOUND' : 'UNPROCESSABLE', err.message);
    throw err;
  }
}

function labelIdsToLabels(ctx: Ctx, repo: Repo, ids: unknown): Label[] {
  if (!Array.isArray(ids)) return [];
  const out: Label[] = [];
  for (const id of ids) {
    const node = nodeById(ctx, id) as { __typename?: string; label?: Label } | null;
    if (node === null || node.__typename !== 'Label' || node.label === undefined || !repo.labels.has(node.label.name.toLowerCase())) {
      throw new FieldError('NOT_FOUND', `Could not resolve to a node with the global id of '${String(id)}'.`);
    }
    out.push(node.label);
  }
  return out;
}

// ---------------------------------------------------------------- Query and Mutation

const Query = typeDef('OBJECT', 'Query', 'The query root of the GraphQL interface.');
fields(Query, {
  viewer: ['User!', {}, (_src, _a, ctx) => userSrc(ctx.forge.viewer)],
  user: ['User', { login: 'String!' }, (_src, args, ctx) => {
    const user = ctx.forge.users.get(String(args.login).toLowerCase());
    return user === undefined ? null : userSrc(user);
  }],
  repositoryOwner: ['RepositoryOwner', { login: 'String!' }, (_src, args, ctx) => {
    const user = ctx.forge.users.get(String(args.login).toLowerCase());
    return user === undefined ? null : userSrc(user);
  }],
  organization: ['User', { login: 'String!' }, () => null],
  repository: ['Repository', { owner: 'String!', name: 'String!', followRenames: 'Boolean' }, (_src, args, ctx) => {
    const repo = ctx.forge.getRepo(String(args.owner), String(args.name));
    return repo === undefined ? null : repoSrc(repo);
  }],
  node: ['Node', { id: 'ID!' }, (_src, args, ctx) => nodeById(ctx, args.id)],
  nodes: ['[Node]!', { ids: '[ID!]!' }, (_src, args, ctx) => (Array.isArray(args.ids) ? args.ids.map((id: unknown) => nodeById(ctx, id)) : [])],
  rateLimit: ['RateLimit', { dryRun: 'Boolean' }, () => ({ __typename: 'RateLimit', limit: 5000, cost: 1, remaining: 4999, resetAt: new Date(Date.now() + 3_600_000).toISOString().replace(/\.\d{3}Z$/, 'Z'), used: 1, nodeCount: 1 })],
  search: ['SearchResultItemConnection!', { ...connectionArgs, query: 'String!', type: 'SearchType!' }, (_src, args, ctx) => searchItems(ctx, args)],
});

function searchItems(ctx: Ctx, args: Record<string, any>): Record<string, unknown> {
  const text = String(args.query ?? '');
  const qualifiers = new Map<string, string>();
  const words: string[] = [];
  for (const term of text.split(/\s+/).filter((part) => part !== '')) {
    const colon = term.indexOf(':');
    if (colon > 0) qualifiers.set(term.slice(0, colon).toLowerCase(), term.slice(colon + 1));
    else words.push(term.toLowerCase());
  }
  const named = qualifiers.get('repo')?.toLowerCase();
  const repos = ctx.forge.reposOf(ctx.forge.viewer).filter((repo) => named === undefined || `${repo.owner.login}/${repo.name}`.toLowerCase() === named);
  const matches = (title: string, body: string): boolean => words.every((word) => title.toLowerCase().includes(word) || body.toLowerCase().includes(word));
  const items: unknown[] = [];
  let issueCount = 0;
  let repositoryCount = 0;
  if (args.type === 'REPOSITORY') {
    for (const repo of repos) if (matches(repo.name, repo.description ?? '')) items.push(repoSrc(repo));
    repositoryCount = items.length;
  } else if (args.type === 'USER') {
    items.push(userSrc(ctx.forge.viewer));
  } else {
    const is = qualifiers.get('is');
    const type = qualifiers.get('type') ?? (is === 'pr' || is === 'issue' ? is : undefined);
    const state = qualifiers.get('state') ?? (is === 'open' || is === 'closed' ? is : undefined);
    for (const repo of repos) {
      if (type !== 'pr') for (const issue of repo.issues.values()) if ((state === undefined || issue.state === state) && matches(issue.title, issue.body)) items.push(issueSrc(repo, issue));
      if (type !== 'issue') for (const pull of repo.pulls.values()) if ((state === undefined || pull.state === state) && matches(pull.title, pull.body)) items.push(pullSrc(repo, pull));
    }
    issueCount = items.length;
  }
  return { ...connection('SearchResultItemConnection', items, args, (item) => item), issueCount, repositoryCount, userCount: args.type === 'USER' ? items.length : 0, discussionCount: 0, codeCount: 0, wikiCount: 0 };
}

const payload = (name: string, specs: Record<string, string>): void => fields(typeDef('OBJECT', name, `The ${name}.`), { clientMutationId: 'String', ...specs });
payload('CreateIssuePayload', { issue: 'Issue' });
payload('CloseIssuePayload', { issue: 'Issue' });
payload('ReopenIssuePayload', { issue: 'Issue' });
payload('UpdateIssuePayload', { issue: 'Issue' });
payload('CreatePullRequestPayload', { pullRequest: 'PullRequest' });
payload('MergePullRequestPayload', { pullRequest: 'PullRequest' });
payload('ClosePullRequestPayload', { pullRequest: 'PullRequest' });
payload('ReopenPullRequestPayload', { pullRequest: 'PullRequest' });
payload('UpdatePullRequestPayload', { pullRequest: 'PullRequest' });
payload('AddCommentPayload', { commentEdge: 'IssueCommentEdge', subject: 'Node', timelineEdge: 'IssueCommentEdge' });
payload('CreateRepositoryPayload', { repository: 'Repository' });
payload('CreateRefPayload', { ref: 'Ref' });
payload('AddLabelsToLabelablePayload', { labelable: 'IssueOrPullRequest' });

const Mutation = typeDef('OBJECT', 'Mutation', 'The root query for implementing GraphQL mutations.');
fields(Mutation, {
  createIssue: ['CreateIssuePayload', { input: 'CreateIssueInput!' }, (_src, { input }, ctx) => {
    const repo = need<RepoSrc>(nodeById(ctx, input.repositoryId), 'Repository', input.repositoryId).repo;
    if (typeof input.title !== 'string' || input.title.trim() === '') throw new FieldError('UNPROCESSABLE', 'Title can\'t be blank');
    const labels = labelIdsToLabels(ctx, repo, input.labelIds);
    const issue = ctx.forge.createIssue(repo, { title: input.title, body: typeof input.body === 'string' ? input.body : '', labels });
    return { clientMutationId: input.clientMutationId ?? null, issue: issueSrc(repo, issue) };
  }],
  closeIssue: ['CloseIssuePayload', { input: 'CloseIssueInput!' }, (_src, { input }, ctx) => {
    const src = need<IssueSrc>(nodeById(ctx, input.issueId), 'Issue', input.issueId);
    ctx.forge.setIssueState(src.issue, 'closed', typeof input.stateReason === 'string' ? input.stateReason.toLowerCase() : null);
    return { clientMutationId: input.clientMutationId ?? null, issue: src };
  }],
  reopenIssue: ['ReopenIssuePayload', { input: 'ReopenIssueInput!' }, (_src, { input }, ctx) => {
    const src = need<IssueSrc>(nodeById(ctx, input.issueId), 'Issue', input.issueId);
    ctx.forge.setIssueState(src.issue, 'open', 'reopened');
    return { clientMutationId: input.clientMutationId ?? null, issue: src };
  }],
  updateIssue: ['UpdateIssuePayload', { input: 'UpdateIssueInput!' }, (_src, { input }, ctx) => {
    const src = need<IssueSrc>(nodeById(ctx, input.id), 'Issue', input.id);
    if (typeof input.title === 'string') src.issue.title = input.title;
    if (typeof input.body === 'string') src.issue.body = input.body;
    if (input.labelIds !== undefined && input.labelIds !== null) src.issue.labels = labelIdsToLabels(ctx, src.repo, input.labelIds);
    if (input.state === 'OPEN' || input.state === 'CLOSED') ctx.forge.setIssueState(src.issue, input.state === 'OPEN' ? 'open' : 'closed');
    ctx.forge.touchIssue(src.issue);
    return { clientMutationId: input.clientMutationId ?? null, issue: src };
  }],
  addComment: ['AddCommentPayload', { input: 'AddCommentInput!' }, (_src, { input }, ctx) => {
    const node = nodeById(ctx, input.subjectId) as IssueSrc | PullSrc | null;
    if (node === null || (node.__typename !== 'Issue' && node.__typename !== 'PullRequest')) {
      throw new FieldError('NOT_FOUND', `Could not resolve to a node with the global id of '${String(input.subjectId)}'.`);
    }
    if (typeof input.body !== 'string' || input.body === '') throw new FieldError('UNPROCESSABLE', 'Body can\'t be blank');
    const target = node.__typename === 'Issue' ? node.issue : node.pull;
    const comment = ctx.forge.addComment(node.repo, target, input.body);
    const edge = { __typename: 'IssueCommentEdge', cursor: encodeCursor(target.comments.length - 1), node: commentSrc(node.repo, comment) };
    return { clientMutationId: input.clientMutationId ?? null, commentEdge: edge, timelineEdge: edge, subject: node };
  }],
  createPullRequest: ['CreatePullRequestPayload', { input: 'CreatePullRequestInput!' }, (_src, { input }, ctx) => {
    const repo = need<RepoSrc>(nodeById(ctx, input.repositoryId), 'Repository', input.repositoryId).repo;
    const pull = wrapForge(() => openPull(ctx.forge, repo, {
      title: String(input.title ?? ''),
      body: typeof input.body === 'string' ? input.body : '',
      head: String(input.headRefName ?? ''),
      base: String(input.baseRefName ?? ''),
      draft: input.draft === true,
    }));
    return { clientMutationId: input.clientMutationId ?? null, pullRequest: pullSrc(repo, pull) };
  }],
  mergePullRequest: ['MergePullRequestPayload', { input: 'MergePullRequestInput!' }, (_src, { input }, ctx) => {
    const src = need<PullSrc>(nodeById(ctx, input.pullRequestId), 'PullRequest', input.pullRequestId);
    const method = typeof input.mergeMethod === 'string' ? input.mergeMethod.toLowerCase() : 'merge';
    wrapForge(() => ctx.forge.mergePull(src.repo, src.pull, { method, title: input.commitHeadline ?? undefined, message: input.commitBody ?? undefined, sha: input.expectedHeadOid ?? undefined }));
    return { clientMutationId: input.clientMutationId ?? null, pullRequest: src };
  }],
  closePullRequest: ['ClosePullRequestPayload', { input: 'ClosePullRequestInput!' }, (_src, { input }, ctx) => {
    const src = need<PullSrc>(nodeById(ctx, input.pullRequestId), 'PullRequest', input.pullRequestId);
    ctx.forge.closePull(src.repo, src.pull);
    return { clientMutationId: input.clientMutationId ?? null, pullRequest: src };
  }],
  reopenPullRequest: ['ReopenPullRequestPayload', { input: 'ReopenPullRequestInput!' }, (_src, { input }, ctx) => {
    const src = need<PullSrc>(nodeById(ctx, input.pullRequestId), 'PullRequest', input.pullRequestId);
    ctx.forge.reopenPull(src.pull);
    return { clientMutationId: input.clientMutationId ?? null, pullRequest: src };
  }],
  updatePullRequest: ['UpdatePullRequestPayload', { input: 'UpdatePullRequestInput!' }, (_src, { input }, ctx) => {
    const src = need<PullSrc>(nodeById(ctx, input.pullRequestId), 'PullRequest', input.pullRequestId);
    if (typeof input.title === 'string') src.pull.title = input.title;
    if (typeof input.body === 'string') src.pull.body = input.body;
    if (typeof input.baseRefName === 'string' && ctx.forge.branchExists(src.repo, input.baseRefName)) src.pull.base = input.baseRefName;
    if (input.state === 'CLOSED') ctx.forge.closePull(src.repo, src.pull);
    if (input.state === 'OPEN') ctx.forge.reopenPull(src.pull);
    return { clientMutationId: input.clientMutationId ?? null, pullRequest: src };
  }],
  createRepository: ['CreateRepositoryPayload', { input: 'CreateRepositoryInput!' }, (_src, { input }, ctx) => {
    if (ctx.forge.getRepo(ctx.forge.viewer.login, String(input.name)) !== undefined) throw new FieldError('UNPROCESSABLE', 'Name already exists on this account');
    if (!/^[A-Za-z0-9._-]{1,100}$/.test(String(input.name))) throw new FieldError('UNPROCESSABLE', 'Name is invalid');
    const repo = ctx.forge.createRepo(ctx.forge.viewer, String(input.name), { description: typeof input.description === 'string' ? input.description : null, private: input.visibility !== 'PUBLIC', hasIssues: input.hasIssuesEnabled !== false });
    return { clientMutationId: input.clientMutationId ?? null, repository: repoSrc(repo) };
  }],
  createRef: ['CreateRefPayload', { input: 'CreateRefInput!' }, (_src, { input }, ctx) => {
    const repo = need<RepoSrc>(nodeById(ctx, input.repositoryId), 'Repository', input.repositoryId).repo;
    const name = String(input.name);
    const oid = String(input.oid).toLowerCase();
    if (!name.startsWith('refs/')) throw new FieldError('UNPROCESSABLE', 'Reference name must start with refs/');
    if (ctx.forge.getObject(oid) === undefined) throw new FieldError('NOT_FOUND', `Could not resolve to a commit with the oid ${oid}.`);
    if (repo.refs.has(name)) throw new FieldError('UNPROCESSABLE', `A ref named "${name}" already exists in the repository.`);
    ctx.forge.setRef(repo, name, oid);
    return { clientMutationId: input.clientMutationId ?? null, ref: refSrc(repo, name, oid) };
  }],
  addLabelsToLabelable: ['AddLabelsToLabelablePayload', { input: 'AddLabelsToLabelableInput!' }, (_src, { input }, ctx) => {
    const node = nodeById(ctx, input.labelableId) as IssueSrc | PullSrc | null;
    if (node === null || (node.__typename !== 'Issue' && node.__typename !== 'PullRequest')) throw new FieldError('NOT_FOUND', `Could not resolve to a node with the global id of '${String(input.labelableId)}'.`);
    const labels = labelIdsToLabels(ctx, node.repo, input.labelIds);
    const target = node.__typename === 'Issue' ? node.issue : node.pull;
    for (const label of labels) if (!target.labels.includes(label)) target.labels.push(label);
    if (node.__typename === 'Issue') ctx.forge.touchIssue(node.issue);
    return { clientMutationId: input.clientMutationId ?? null, labelable: node };
  }],
});

// ---------------------------------------------------------------- introspection

const __Schema = typeDef('OBJECT', '__Schema', 'A GraphQL schema.');
fields(__Schema, { description: 'String', types: '[__Type!]!', queryType: '__Type!', mutationType: '__Type', subscriptionType: '__Type', directives: '[__Directive!]!' });
const __Type = typeDef('OBJECT', '__Type', 'A GraphQL type.');
fields(__Type, {
  kind: '__TypeKind!',
  name: 'String',
  description: 'String',
  specifiedByURL: 'String',
  isOneOf: 'Boolean',
  fields: ['[__Field!]', { includeDeprecated: { type: 'Boolean', defaultValue: false } }],
  interfaces: '[__Type!]',
  possibleTypes: '[__Type!]',
  enumValues: ['[__EnumValue!]', { includeDeprecated: { type: 'Boolean', defaultValue: false } }],
  inputFields: ['[__InputValue!]', { includeDeprecated: { type: 'Boolean', defaultValue: false } }],
  ofType: '__Type',
});
fields(typeDef('OBJECT', '__Field', 'A field of an object or interface.'), {
  name: 'String!',
  description: 'String',
  args: ['[__InputValue!]!', { includeDeprecated: { type: 'Boolean', defaultValue: false } }],
  type: '__Type!',
  isDeprecated: 'Boolean!',
  deprecationReason: 'String',
});
fields(typeDef('OBJECT', '__InputValue', 'An argument or input field.'), { name: 'String!', description: 'String', type: '__Type!', defaultValue: 'String', isDeprecated: 'Boolean!', deprecationReason: 'String' });
fields(typeDef('OBJECT', '__EnumValue', 'An enum value.'), { name: 'String!', description: 'String', isDeprecated: 'Boolean!', deprecationReason: 'String' });
fields(typeDef('OBJECT', '__Directive', 'A directive.'), { name: 'String!', description: 'String', isRepeatable: 'Boolean!', locations: '[String!]!', args: ['[__InputValue!]!', { includeDeprecated: 'Boolean' }] });

function introspectRef(ref: string): Record<string, unknown> | null {
  if (ref.endsWith('!')) return { __typename: '__Type', kind: 'NON_NULL', name: null, description: null, ofType: introspectRef(ref.slice(0, -1)), ...emptyTypeLists() };
  if (ref.startsWith('[')) return { __typename: '__Type', kind: 'LIST', name: null, description: null, ofType: introspectRef(ref.slice(1, -1)), ...emptyTypeLists() };
  return introspectNamed(ref);
}

function emptyTypeLists(): Record<string, unknown> {
  return { fields: null, interfaces: null, possibleTypes: null, enumValues: null, inputFields: null, specifiedByURL: null, isOneOf: null };
}

function inputValue(name: string, arg: ArgDef): Record<string, unknown> {
  return { __typename: '__InputValue', name, description: null, type: introspectRef(arg.type), defaultValue: arg.defaultValue === undefined ? null : JSON.stringify(arg.defaultValue), isDeprecated: false, deprecationReason: null };
}

function introspectNamed(name: string): Record<string, unknown> | null {
  const def = TYPES.get(name);
  if (def === undefined) return null;
  const objectLike = def.kind === 'OBJECT' || def.kind === 'INTERFACE';
  return {
    __typename: '__Type',
    kind: def.kind,
    name: def.name,
    description: def.description,
    specifiedByURL: null,
    isOneOf: def.kind === 'INPUT_OBJECT' ? false : null,
    fields: objectLike
      ? () => Object.entries(def.fields).map(([fieldName, field]) => ({
          __typename: '__Field',
          name: fieldName,
          description: field.description,
          args: () => Object.entries(field.args).map(([argName, arg]) => inputValue(argName, arg)),
          type: () => introspectRef(field.type),
          isDeprecated: false,
          deprecationReason: null,
        }))
      : null,
    interfaces: def.kind === 'OBJECT' || def.kind === 'INTERFACE' ? () => def.interfaces.map((iface) => introspectNamed(iface)) : null,
    possibleTypes: def.kind === 'INTERFACE' || def.kind === 'UNION' ? () => def.possibleTypes.map((possible) => introspectNamed(possible)) : null,
    enumValues: def.kind === 'ENUM' ? () => def.enumValues.map((value) => ({ __typename: '__EnumValue', name: value, description: null, isDeprecated: false, deprecationReason: null })) : null,
    inputFields: def.kind === 'INPUT_OBJECT' ? () => Object.entries(def.inputFields).map(([fieldName, field]) => inputValue(fieldName, field)) : null,
    ofType: null,
  };
}

function introspectSchema(): Record<string, unknown> {
  return {
    __typename: '__Schema',
    description: 'The forge GraphQL schema.',
    types: () => [...TYPES.keys()].map((name) => introspectNamed(name)),
    queryType: () => introspectNamed('Query'),
    mutationType: () => introspectNamed('Mutation'),
    subscriptionType: null,
    directives: () =>
      ['include', 'skip'].map((name) => ({
        __typename: '__Directive',
        name,
        description: `The @${name} directive.`,
        isRepeatable: false,
        locations: ['FIELD', 'FRAGMENT_SPREAD', 'INLINE_FRAGMENT'],
        args: () => [inputValue('if', { type: 'Boolean!' })],
      })),
  };
}

// ================================================================ validation and execution

function namedType(ref: string): string {
  return ref.replace(/[[\]!]/g, '');
}

/** The field definition a selection names on a type (including the meta fields). */
function fieldOn(typeName: string, name: string): FieldDef | null {
  if (name === '__typename') return { type: 'String!', args: {}, description: 'The type name.' };
  if (typeName === 'Query' && name === '__type') return { type: '__Type', args: { name: { type: 'String!' } }, description: 'A type by name.' };
  if (typeName === 'Query' && name === '__schema') return { type: '__Schema!', args: {}, description: 'The schema.' };
  const def = TYPES.get(typeName);
  if (def === undefined) return null;
  return def.fields[name] ?? null;
}

function possible(typeName: string): string[] {
  const def = TYPES.get(typeName);
  if (def === undefined) return [];
  if (def.kind === 'OBJECT') return [typeName];
  return def.possibleTypes;
}

function validate(doc: Document, operation: Operation, errors: GqlError[]): void {
  const visitedFragments = new Set<string>();
  const walk = (typeName: string, selections: Selection[], path: string[]): void => {
    for (const selection of selections) {
      if (selection.kind === 'Field') {
        const def = fieldOn(typeName, selection.name);
        const fieldPath = [...path, selection.alias ?? selection.name];
        if (def === null) {
          errors.push({
            message: `Field '${selection.name}' doesn't exist on type '${typeName}'`,
            locations: [selection.loc],
            path: fieldPath,
            extensions: { code: 'undefinedField', typeName, fieldName: selection.name },
          });
          continue;
        }
        for (const arg of selection.args) {
          if (def.args[arg.name] === undefined && selection.name !== '__typename') {
            errors.push({
              message: `Field '${selection.name}' doesn't accept argument '${arg.name}'`,
              locations: [selection.loc],
              path: fieldPath,
              extensions: { code: 'argumentNotAccepted', name: selection.name, typeName: 'Field', argumentName: arg.name },
            });
          }
        }
        const target = TYPES.get(namedType(def.type));
        const composite = target !== undefined && (target.kind === 'OBJECT' || target.kind === 'INTERFACE' || target.kind === 'UNION');
        if (composite && selection.selections === null) {
          errors.push({ message: `Field must have selections (field '${selection.name}' returns ${namedType(def.type)} but has no selections. Did you mean '${selection.name} { ... }'?)`, locations: [selection.loc], path: fieldPath, extensions: { code: 'selectionMismatch', nodeName: `field '${selection.name}'`, typeName: namedType(def.type) } });
        } else if (!composite && selection.selections !== null) {
          errors.push({ message: `Selections can't be made on scalars (field '${selection.name}' returns ${namedType(def.type)} but has selections [${selection.selections.map((s) => (s.kind === 'Field' ? s.name : '...')).join(', ')}])`, locations: [selection.loc], path: fieldPath, extensions: { code: 'selectionMismatch', nodeName: `field '${selection.name}'`, typeName: namedType(def.type) } });
        } else if (composite && selection.selections !== null) {
          walk(namedType(def.type), selection.selections, fieldPath);
        }
      } else if (selection.kind === 'InlineFragment') {
        const condition = selection.typeCondition ?? typeName;
        if (!TYPES.has(condition)) {
          errors.push({ message: `No such type ${condition}, so it can't be a fragment condition`, locations: [selection.loc], path: [...path, `... on ${condition}`], extensions: { code: 'undefinedType', typeName: condition } });
          continue;
        }
        walk(condition, selection.selections, [...path, `... on ${condition}`]);
      } else {
        const fragment = doc.fragments.get(selection.name);
        if (fragment === undefined) {
          errors.push({ message: `Fragment ${selection.name} was used, but not defined`, locations: [selection.loc], path: [...path, `... ${selection.name}`], extensions: { code: 'useAndDefineFragment', fragmentName: selection.name } });
          continue;
        }
        if (visitedFragments.has(`${typeName}:${fragment.name}`)) continue;
        visitedFragments.add(`${typeName}:${fragment.name}`);
        if (!TYPES.has(fragment.typeCondition)) {
          errors.push({ message: `No such type ${fragment.typeCondition}, so it can't be a fragment condition`, locations: [selection.loc], extensions: { code: 'undefinedType', typeName: fragment.typeCondition } });
          continue;
        }
        walk(fragment.typeCondition, fragment.selections, [...path, `... ${fragment.name}`]);
      }
    }
  };
  const root = operation.operation === 'mutation' ? 'Mutation' : 'Query';
  if (operation.operation === 'subscription') {
    errors.push({ message: 'Subscriptions are not supported', locations: [operation.loc] });
    return;
  }
  walk(root, operation.selections, [operation.operation === 'mutation' ? 'mutation' : 'query']);
}

function valueOf(node: ValueNode, ctx: Ctx): unknown {
  switch (node.kind) {
    case 'Variable':
      return ctx.variables[node.name];
    case 'Int':
      return Number.parseInt(node.value, 10);
    case 'Float':
      return Number.parseFloat(node.value);
    case 'String':
    case 'Enum':
      return node.value;
    case 'Boolean':
      return node.value;
    case 'Null':
      return null;
    case 'List':
      return node.values.map((value) => valueOf(value, ctx));
    case 'Object': {
      const out: Record<string, unknown> = {};
      for (const field of node.fields) out[field.name] = valueOf(field.value, ctx);
      return out;
    }
  }
}

function argsOf(def: FieldDef, field: FieldNode, ctx: Ctx): Record<string, any> {
  const out: Record<string, any> = {};
  for (const [name, arg] of Object.entries(def.args)) if (arg.defaultValue !== undefined) out[name] = arg.defaultValue;
  for (const arg of field.args) {
    const value = valueOf(arg.value, ctx);
    if (value !== undefined) out[arg.name] = value;
  }
  // A list argument given a single value is coerced to a one-element list.
  for (const [name, arg] of Object.entries(def.args)) {
    if (arg.type.startsWith('[') && out[name] !== undefined && out[name] !== null && !Array.isArray(out[name])) out[name] = [out[name]];
  }
  return out;
}

function skipped(directives: Directive[], ctx: Ctx): boolean {
  for (const directive of directives) {
    const arg = directive.args.find((candidate) => candidate.name === 'if');
    const value = arg === undefined ? undefined : valueOf(arg.value, ctx);
    if (directive.name === 'skip' && value === true) return true;
    if (directive.name === 'include' && value === false) return true;
  }
  return false;
}

function typeMatches(condition: string | null, runtime: string): boolean {
  if (condition === null || condition === runtime) return true;
  const def = TYPES.get(condition);
  return def !== undefined && (def.kind === 'INTERFACE' || def.kind === 'UNION') && def.possibleTypes.includes(runtime);
}

function collect(ctx: Ctx, runtime: string, selections: Selection[], into: Map<string, FieldNode[]>): void {
  for (const selection of selections) {
    if (skipped(selection.directives, ctx)) continue;
    if (selection.kind === 'Field') {
      const key = selection.alias ?? selection.name;
      const list = into.get(key);
      if (list === undefined) into.set(key, [selection]);
      else list.push(selection);
    } else if (selection.kind === 'InlineFragment') {
      if (typeMatches(selection.typeCondition, runtime)) collect(ctx, runtime, selection.selections, into);
    } else {
      const fragment = ctx.fragments.get(selection.name);
      if (fragment !== undefined && typeMatches(fragment.typeCondition, runtime)) collect(ctx, runtime, fragment.selections, into);
    }
  }
}

function executeSelections(ctx: Ctx, typeName: string, src: any, selections: Selection[], path: Array<string | number>): Record<string, unknown> {
  const grouped = new Map<string, FieldNode[]>();
  collect(ctx, typeName, selections, grouped);
  const out: Record<string, unknown> = {};
  for (const [key, nodes] of grouped) out[key] = executeField(ctx, typeName, src, nodes, [...path, key]);
  return out;
}

function executeField(ctx: Ctx, typeName: string, src: any, nodes: FieldNode[], path: Array<string | number>): unknown {
  const field = nodes[0];
  if (field.name === '__typename') return typeName;
  const def = fieldOn(typeName, field.name);
  if (def === null) return null;
  let value: unknown;
  try {
    const args = argsOf(def, field, ctx);
    if (typeName === 'Query' && field.name === '__type') value = introspectNamed(String(args.name));
    else if (typeName === 'Query' && field.name === '__schema') value = introspectSchema();
    else if (def.resolve !== undefined) value = def.resolve(src, args, ctx);
    else {
      const raw = src === null || src === undefined ? undefined : src[field.name];
      value = typeof raw === 'function' ? raw(args) : raw;
    }
  } catch (err) {
    if (err instanceof FieldError) {
      ctx.errors.push({ type: err.type, path, locations: [field.loc], message: err.message });
      return null;
    }
    throw err;
  }
  return complete(ctx, def.type, nodes, value, path);
}

function complete(ctx: Ctx, ref: string, nodes: FieldNode[], value: unknown, path: Array<string | number>): unknown {
  if (ref.endsWith('!')) return complete(ctx, ref.slice(0, -1), nodes, value, path);
  if (value === null || value === undefined) return null;
  if (ref.startsWith('[')) {
    if (!Array.isArray(value)) return null;
    const inner = ref.slice(1, -1);
    return value.map((item, index) => complete(ctx, inner, nodes, item, [...path, index]));
  }
  const def = TYPES.get(ref);
  if (def === undefined || def.kind === 'SCALAR' || def.kind === 'ENUM') return value;
  const runtime = def.kind === 'OBJECT' ? ref : String((value as { __typename?: string }).__typename ?? ref);
  const selections: Selection[] = [];
  for (const node of nodes) if (node.selections !== null) selections.push(...node.selections);
  return executeSelections(ctx, runtime, value, selections, path);
}

/** Coerce the request's variables against the operation's definitions (defaults and required ones). */
function coerceVariables(operation: Operation, provided: Record<string, unknown>, errors: GqlError[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const definition of operation.variables) {
    const given = Object.prototype.hasOwnProperty.call(provided, definition.name) ? provided[definition.name] : undefined;
    if (given !== undefined) out[definition.name] = given;
    else if (definition.defaultValue !== null) out[definition.name] = valueOf(definition.defaultValue, { variables: {} } as unknown as Ctx);
    if (definition.type.kind === 'NonNull' && (out[definition.name] === undefined || out[definition.name] === null)) {
      errors.push({ message: `Variable $${definition.name} of type ${typeText(definition.type)} was provided invalid value`, locations: [operation.loc], extensions: { value: null, problems: [{ path: [], explanation: 'Expected value to not be null' }] } });
    }
  }
  return out;
}

function typeText(type: TypeNode): string {
  return type.kind === 'Named' ? type.name : type.kind === 'List' ? `[${typeText(type.of)}]` : `${typeText(type.of)}!`;
}

/** Run one GraphQL request object ({query, variables, operationName}). */
function runOne(forge: Forge, origin: string, request: unknown): Record<string, unknown> {
  if (typeof request !== 'object' || request === null || Array.isArray(request)) {
    return { errors: [{ message: 'A query attribute must be specified and must be a string.' }] };
  }
  const { query, variables, operationName } = request as { query?: unknown; variables?: unknown; operationName?: unknown };
  if (typeof query !== 'string' || query.trim() === '') return { errors: [{ message: 'A query attribute must be specified and must be a string.' }] };
  let doc: Document;
  try {
    doc = new Parser(query).document();
  } catch (err) {
    if (err instanceof GraphQLSyntaxError) return { errors: [{ message: `Parse error on ${err.message}`, locations: [err.loc] }] };
    throw err;
  }
  let operation: Operation | undefined;
  if (typeof operationName === 'string' && operationName !== '') operation = doc.operations.find((candidate) => candidate.name === operationName);
  else if (doc.operations.length === 1) operation = doc.operations[0];
  if (operation === undefined) {
    return { errors: [{ message: doc.operations.length === 0 ? 'No operations in query document' : typeof operationName === 'string' && operationName !== '' ? `No operation named "${operationName}"` : 'An operation name is required' }] };
  }
  const errors: GqlError[] = [];
  validate(doc, operation, errors);
  const vars = coerceVariables(operation, typeof variables === 'object' && variables !== null && !Array.isArray(variables) ? (variables as Record<string, unknown>) : {}, errors);
  if (errors.length > 0) return { errors };
  const ctx: Ctx = { forge, origin, variables: vars, fragments: doc.fragments, errors: [] };
  const data = executeSelections(ctx, operation.operation === 'mutation' ? 'Mutation' : 'Query', {}, operation.selections, []);
  return ctx.errors.length > 0 ? { data, errors: ctx.errors } : { data };
}

export function handleGraphQL(forge: Forge, payload: unknown, origin: string): { status: number; body: unknown } {
  if (Array.isArray(payload)) return { status: 200, body: payload.map((request) => runOne(forge, origin, request)) };
  return { status: 200, body: runOne(forge, origin, payload) };
}

// Keep the type tables referenced so tooling never tree-shakes them.
void Node;
void Actor;
void RepositoryOwner;
void GitObject;
void UserType;
void RepositoryType;
void IssueType;
void PullType;
void Query;
void Mutation;
void __Schema;
void __Type;
void resolveLabels;
