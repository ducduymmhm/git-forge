# git-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/ducduymmhm/git-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/ducduymmhm/git-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, GitHub-compatible git forge in one Node 22 process, with no dependencies and no git binary.
It serves its own in-memory store over four surfaces:

- **Git smart HTTP** at `/{owner}/{repo}.git`: upload-pack protocol v2 (`ls-refs`, `fetch` with shallow,
  deepen and thin haves) and v0, plus receive-pack v0 with `report-status`, `side-band-64k`,
  `delete-refs` and per-ref fast-forward checks. Packfiles, including ofs- and ref-deltas, are read and written in-process.
- **REST v3** under `/api/v3`: repositories, contents, branches, commits, git refs, trees and blobs,
  labels, issues, comments, pull requests (files, commits, squash or merge-commit merges with three-way
  conflict detection) and search, in GitHub's JSON shapes with `Link` pagination.
- **GraphQL v4** at `/api/graphql`: its own parser, validator and executor, with introspection,
  batching, Relay global ids and connections, and the queries and mutations `gh` sends.
- **MCP** at `/mcp`: JSON-RPC over Streamable HTTP (revision 2026-07-28 `server/discover`, and the
  `initialize` handshake), with github-mcp-server-style tools over the same store.

Every request needs `Authorization: Bearer <token>`. The token is `MAKEFUN_SHOP_TOKEN` when it is set;
otherwise any token is accepted.

## Run it

```sh
PORT=8000 node main.ts
```

Under the make.fun harness (`MAKEFUN_ENV_URL` set), it binds an ephemeral port, registers it, and exits
once the suite reports that it is done.

## Layout

| file | what |
| --- | --- |
| `main.ts` | the lifecycle: listen, register, wait for done |
| `src/server.ts` | HTTP routing, auth, response headers |
| `src/git.ts` | objects, trees, commits, pkt-lines, packfiles, deltas |
| `src/store.ts` | repositories, refs, issues, pulls; merges and diffs |
| `src/smart-http.ts` | upload-pack and receive-pack |
| `src/rest.ts` | REST v3 |
| `src/graphql.ts` | the GraphQL engine and schema |
| `src/mcp.ts` | the MCP endpoint and tools |

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
