// MCP over streamable HTTP (JSON-RPC 2.0, plain JSON answers).
//
// Wire facts (sdk/conformance/mcp-client.ts + checks-mcp.ts):
// - Modern era 2026-07-28: every request declares the revision in
//   _meta['io.modelcontextprotocol/protocolVersion'] and the
//   MCP-Protocol-Version header; server/discover answers supportedVersions
//   (non-empty YYYY-MM-DD list incl. 2026-07-28), capabilities (a record
//   including tools) and serverInfo (name + version, both as a loose field and
//   in _meta['io.modelcontextprotocol/serverInfo']).
// - Legacy: initialize (echo a legacy revision) + notifications/initialized
//   (202, body ignored).
// - tools/list returns a fixed array (byte-identical across calls); tools/call
//   answers {content:[{type:"text", text: JSON}], structuredContent, isError}.
// - Unknown tool -> -32602 or -32601 (never HTTP 500); missing required
//   argument -> isError:true or -32602; non-JSON body -> HTTP 400 with -32700;
//   no Authorization -> 401 (handled by the app's auth layer).
// - Tools share the store with REST: a repo/issue created over MCP reads back
//   over /api/v3 as the same object.
import {
  store,
  VIEWER_LOGIN,
  nextSeq,
  nextGlobalId,
  type Repo,
  type Issue,
  type Pull,
} from './store.ts';
import { repoJson, issueJson, pullJson, squashMerge } from './rest.ts';
import { sendJson, parseJsonBody, isPlainObject, type Ctx } from './http-util.ts';

const SERVER_NAME = 'makefun-github-forge';
const SERVER_VERSION = '1.0.0';
const MODERN_REVISION = '2026-07-28';
const LEGACY_REVISION = '2025-11-25';

const SUPPORTED_VERSIONS = [MODERN_REVISION, LEGACY_REVISION];

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, { type: string; description: string }>;
    required: string[];
  };
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The repository name.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues of a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        state: { type: 'string', description: 'open, closed or all (default open).' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch onto a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The pull request title.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
        body: { type: 'string', description: 'The pull request body.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request onto its base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        pull_number: { type: 'integer', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

const TOOL_MAP = new Map(TOOLS.map((tool) => [tool.name, tool]));

function toolResult(obj: Record<string, unknown>): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: JSON.stringify(obj) }],
    structuredContent: obj,
    isError: false,
  };
}

function toolError(message: string): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: message }],
    structuredContent: { error: message },
    isError: true,
  };
}

/** Find a required argument; returns the value or null (with the error set). */
function requiredArg(
  args: Record<string, unknown>,
  tool: ToolDef,
  name: string,
): { value: unknown } | { error: string } {
  const value = args[name];
  if (value === undefined || value === null || value === '') {
    return { error: `missing required argument: ${name}` };
  }
  return { value };
}

function getRepoOrError(args: Record<string, unknown>): { repo: Repo } | { error: string } {
  const owner = args.owner;
  const repoName = args.repo;
  if (typeof owner !== 'string' || typeof repoName !== 'string') {
    return { error: 'owner and repo are required' };
  }
  if (owner !== VIEWER_LOGIN) return { error: 'repository not found' };
  const repo = store.getRepo(owner, repoName);
  if (!repo) return { error: 'repository not found' };
  return { repo };
}

function callTool(name: string, args: Record<string, unknown>): Record<string, unknown> {
  const tool = TOOL_MAP.get(name);
  if (!tool) return toolError(`unknown tool: ${name}`);
  // Required-argument check (minimal lists, per the tool definitions).
  for (const required of tool.inputSchema.required) {
    const found = requiredArg(args, tool, required);
    if ('error' in found) return toolError(found.error);
  }

  if (name === 'create_repo') {
    const repoName = String(args.name);
    const repo = store.createRepo(
      repoName,
      typeof args.description === 'string' ? args.description : '',
      args.private === true,
    );
    if (!repo) return toolError('name already exists on this account');
    return toolResult(repoJson(repo) as Record<string, unknown>);
  }

  if (name === 'get_repo') {
    const found = getRepoOrError(args);
    if ('error' in found) return toolError(found.error);
    return toolResult(repoJson(found.repo) as Record<string, unknown>);
  }

  if (name === 'create_issue') {
    const found = getRepoOrError(args);
    if ('error' in found) return toolError(found.error);
    const repo = found.repo;
    const issue: Issue = {
      id: nextGlobalId(),
      number: repo.nextNumber,
      title: String(args.title),
      body: typeof args.body === 'string' ? args.body : '',
      state: 'open',
      labels: [],
      comments: [],
      createdSeq: nextSeq(),
      cachedJson: null,
    };
    repo.nextNumber += 1;
    repo.issues.push(issue);
    return toolResult(issueJson(issue, repo) as Record<string, unknown>);
  }

  if (name === 'list_issues') {
    const found = getRepoOrError(args);
    if ('error' in found) return toolError(found.error);
    const repo = found.repo;
    const state = typeof args.state === 'string' ? args.state : 'open';
    const issues = repo.issues
      .filter((issue) => (state === 'all' ? true : issue.state === state))
      .map((issue) => issueJson(issue, repo));
    return toolResult({ issues });
  }

  if (name === 'create_pull_request') {
    const found = getRepoOrError(args);
    if ('error' in found) return toolError(found.error);
    const repo = found.repo;
    const head = String(args.head);
    const base = String(args.base);
    const headSha = repo.refs.get(`refs/heads/${head}`);
    const baseSha = repo.refs.get(`refs/heads/${base}`);
    if (!headSha || !baseSha) return toolError('head or base branch does not exist');
    const duplicate = repo.pulls.find((p) => p.state === 'open' && p.headRef === head && p.baseRef === base);
    if (duplicate) return toolError('a pull request already exists for these branches');
    const pull: Pull = {
      id: nextGlobalId(),
      number: repo.nextNumber,
      title: String(args.title),
      body: typeof args.body === 'string' ? args.body : '',
      state: 'open',
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      headRef: head,
      baseRef: base,
      headSha,
      baseSha,
      createdSeq: nextSeq(),
    };
    repo.nextNumber += 1;
    repo.pulls.push(pull);
    return toolResult(pullJson(pull, repo) as Record<string, unknown>);
  }

  if (name === 'merge_pull_request') {
    const found = getRepoOrError(args);
    if ('error' in found) return toolError(found.error);
    const repo = found.repo;
    const number = Number(args.pull_number);
    const pull = repo.pulls.find((p) => p.number === number);
    if (!pull) return toolError('pull request not found');
    const result = squashMerge(repo, pull);
    if (!result.ok) return toolError(result.reason ?? 'merge failed');
    return toolResult({
      sha: result.sha,
      merged: true,
      number: pull.number,
      message: 'Pull request successfully merged',
    });
  }

  return toolError(`unknown tool: ${name}`);
}

const SERVER_INFO = { name: SERVER_NAME, version: SERVER_VERSION };

function handleRpc(body: Record<string, unknown>): { status: number; payload: unknown } | null {
  const id = 'id' in body ? body.id : undefined;
  const method = body.method;
  // Notifications (no id) are acknowledged with 202 and no body.
  if (id === undefined) {
    return { status: 202, payload: null };
  }
  const result = (value: unknown): { status: number; payload: unknown } => ({
    status: 200,
    payload: { jsonrpc: '2.0', id, result: value },
  });
  const error = (code: number, message: string): { status: number; payload: unknown } => ({
    status: 200,
    payload: { jsonrpc: '2.0', id, error: { code, message } },
  });
  if (typeof method !== 'string') return error(-32600, 'Invalid Request');

  if (method === 'server/discover') {
    return result({
      supportedVersions: [...SUPPORTED_VERSIONS],
      capabilities: { tools: {} },
      serverInfo: { ...SERVER_INFO },
      _meta: { 'io.modelcontextprotocol/serverInfo': { ...SERVER_INFO } },
    });
  }
  if (method === 'initialize') {
    const params = isPlainObject(body.params) ? (body.params as Record<string, unknown>) : {};
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : LEGACY_REVISION;
    const revision = /^\d{4}-\d{2}-\d{2}$/.test(requested) ? requested : LEGACY_REVISION;
    return result({
      protocolVersion: revision,
      capabilities: { tools: {} },
      serverInfo: { ...SERVER_INFO },
    });
  }
  if (method.startsWith('notifications/')) {
    return { status: 202, payload: null };
  }
  if (method === 'ping') return result({});
  if (method === 'tools/list') {
    return result({ tools: TOOLS });
  }
  if (method === 'tools/call') {
    const params = isPlainObject(body.params) ? (body.params as Record<string, unknown>) : {};
    const toolName = typeof params.name === 'string' ? params.name : '';
    const args = isPlainObject(params.arguments) ? (params.arguments as Record<string, unknown>) : {};
    if (!TOOL_MAP.has(toolName)) return error(-32602, `unknown tool: ${toolName}`);
    return result(callTool(toolName, args));
  }
  return error(-32601, `method not found: ${method}`);
}

export function handleMcp(ctx: Ctx): boolean {
  if (ctx.pathname !== '/mcp') return false;
  if (ctx.method !== 'POST') {
    sendJson(ctx, 404, { message: 'Not Found' });
    return true;
  }
  const body = parseJsonBody(ctx);
  if (body === null) {
    // JSON-RPC parse error: HTTP 400 with -32700.
    sendJson(ctx, 400, {
      jsonrpc: '2.0',
      id: null,
      error: { code: -32700, message: 'Parse error' },
    });
    return true;
  }
  if (!isPlainObject(body)) {
    sendJson(ctx, 400, {
      jsonrpc: '2.0',
      id: null,
      error: { code: -32600, message: 'Invalid Request' },
    });
    return true;
  }
  const outcome = handleRpc(body);
  if (outcome === null) {
    ctx.res.setHeader('X-GitHub-Request-Id', ctx.requestId);
    ctx.res.writeHead(202);
    ctx.res.end();
    return true;
  }
  if (outcome.status === 202) {
    ctx.res.setHeader('X-GitHub-Request-Id', ctx.requestId);
    ctx.res.writeHead(202);
    ctx.res.end();
    return true;
  }
  sendJson(ctx, outcome.status, outcome.payload, { 'MCP-Protocol-Version': MODERN_REVISION });
  return true;
}
