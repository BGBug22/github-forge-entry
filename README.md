# github-forge-entry — GitHub-compatible git forge (Go rebuild)

![make.fun](https://make.fun/badge/github-core-clone-01.svg)
![rank](https://make.fun/badge/github-core-clone-01/BGBug22/github-forge-entry/rank.svg)
![reward](https://make.fun/badge/github-core-clone-01/BGBug22/github-forge-entry/reward.svg)

Entry for [Build a GitHub-compatible git forge](https://make.fun/bounty/github-core-clone-01)
on [make.fun](https://make.fun): a GitHub-compatible forge — Git Smart HTTP
(upload-pack v2 + receive-pack v0), REST v3, GraphQL, and MCP — implemented as
**one static Go binary** for speed, with a thin Node 22 launcher (`main.ts`).

## How it runs

```sh
node main.ts
```

`main.ts` picks an ephemeral port, spawns `./forge` (the static Go binary,
built with `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`), registers the
port at `$MAKEFUN_ENV_URL/register`, then long-polls `$MAKEFUN_ENV_URL/done`
and exits 0 when the suite finishes.

## The forge

- **One in-memory store** serves all four surfaces, so a repo, issue, or pull
  request created over MCP is the same object over REST and GraphQL.
- **Git Smart HTTP**: upload-pack v2 (`ls-refs`, `fetch` with shallow/deepen),
  receive-pack v0 (`report-status`, `side-band-64k`, `delete-refs`), packfiles
  written plain (no deltas) and read with OFS/REF delta support; objects are
  stored verbatim so clones round-trip byte-exact.
- **REST v3** (`/api/v3`): repos, branches, commits, contents (each PUT is one
  commit), labels, issues, comments, pull requests with squash merge and
  conflict detection (405 on conflict).
- **GraphQL** (`/api/graphql`): viewer, repository, issues/pullRequests
  connections with cursor pagination, `__type` introspection, mutations, and
  batched operations.
- **MCP** (`/mcp`): JSON-RPC 2.0 Streamable HTTP, revision 2026-07-28 —
  `server/discover`, `initialize`, and the six tools `create_repo`, `get_repo`,
  `create_issue`, `list_issues`, `create_pull_request`, `merge_pull_request`.
- **Determinism**: no wall clock anywhere in state — every timestamp comes
  from `MAKEFUN_TRIAL_NOW` plus a monotonic counter, and all IDs come from
  creation counters, so repeated runs export byte-identical state.
- **Performance**: issues are served from pre-serialized JSON, depth-1 clone
  packs are memoized per head commit, and all hot paths are O(1) appends or
  hash lookups — no disk I/O, no external git binary, no network.

## Layout

- `main.ts` — Node 22 launcher (entrypoint).
- `forge` — static linux/amd64 Go binary (the whole forge).
- `go-src/` — Go source (`go.mod`, `main.go`, `store.go`, `git.go`,
  `gitproto.go`, `rest.go`, `graphql.go`, `mcp.go`).
- `package.json` — marks the repo as an ES module for `node main.ts`.

## License

MIT — see [LICENSE](LICENSE).
