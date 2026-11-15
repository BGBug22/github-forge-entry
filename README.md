# github-forge-01 — a GitHub-compatible git forge

A single Node 22 process (stdlib only, no dependencies, no git binary, no
outside service) that serves:

- **Git smart HTTP** at `/{owner}/{repo}.git`: upload-pack protocol v2
  (advertisement, ls-refs, fetch with shallow/deepen) and receive-pack v0
  (fast-forward enforcement, per-ref `ok`/`ng` report-status).
- **REST v3** at `/api/v3`: repositories, branches, commits, contents,
  labels, issues, comments, pull requests, files and squash merges.
- **GraphQL v4** at `/api/graphql`: the documents `gh` and
  `github-mcp-server` send, including introspection, cursor pagination and
  the issue/PR mutations.
- **MCP** at `/mcp`: JSON-RPC 2.0 streamable HTTP, modern revision
  2026-07-28 (`server/discover`) with the legacy `initialize` handshake as
  a fallback, and six tools (`create_repo`, `get_repo`, `create_issue`,
  `list_issues`, `create_pull_request`, `merge_pull_request`) that share the
  same in-memory store as REST.

Everything is in memory; state is deterministic (no wall-clock time, no
randomness, no port in any export), so two runs fed the same requests
produce the same state.

## Run it

```sh
MAKEFUN_SHOP_TOKEN=shpat_<token> node --experimental-strip-types main.ts
```

`main.ts` is the harness lifecycle (bind an ephemeral port, register with
`$MAKEFUN_ENV_URL`, poll `/done`, exit 0). `app.ts` builds the server.

## Layout

| File | What it is |
| --- | --- |
| `main.ts` | Lifecycle (kept as the starter's). |
| `app.ts` | HTTP server: routing, Bearer auth, `/healthz`. |
| `http-util.ts` | Request context, body reading, JSON replies. |
| `store.ts` | In-memory store, scenario constants, deterministic clock. |
| `git.ts` | Git objects, packfiles, pkt-lines, reachability. |
| `git-http.ts` | Smart-HTTP handlers (advertisements, ls-refs, fetch, push). |
| `rest.ts` | REST v3 handlers + the squash-merge engine. |
| `graphql.ts` | GraphQL parser, schema and executor. |
| `mcp.ts` | MCP endpoint (discover, initialize, tools). |

## Test locally

Start the server on a fixed port without the harness lifecycle, then run
the kit's conformance runner against it:

```sh
MAKEFUN_SHOP_TOKEN=shpat_<your-test-token> \
  node --experimental-strip-types ../test-server.ts
node --experimental-strip-types \
  ../../github-core-clone-01-kit/sdk/run.ts \
  --url http://127.0.0.1:18123 --token shpat_<your-test-token>
```

Add `--perf` to also run the four benchmark workloads.
