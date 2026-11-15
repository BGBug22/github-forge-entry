// REST v3: repositories, branches, commits, contents, labels, issues,
// comments, pull requests, files and merges.
//
// Wire facts (sdk/conformance/checks-rest.ts):
// - every reply is JSON with a non-empty X-GitHub-Request-Id; errors are
//   {message: string}; unknown routes are 404; malformed JSON bodies are 400.
// - GET /api/v3/ answers 200 + X-OAuth-Scopes: repo, read:org.
// - GET /api/v3/meta answers 200 with a non-empty installed_version.
// - GET /api/v3/user/repos is paginated (default 30/page) with a Link header
//   rel="next" (absolute, same origin) while more pages exist.
// - contents PUT: 201 create / 200 update / 409 stale sha; content.sha is the
//   git blob SHA-1; GET file returns {type:"file", encoding:"base64", content,
//   sha, size}; GET directory returns an array of {type, name, path, sha}.
// - issues may only carry labels that exist, else 422; issue numbers increment
//   by 1 per repo and share the counter with pull requests (GitHub behaviour).
// - pulls: POST 201 with head/base {ref, sha} at the branch tips; duplicate open
//   PR from the same head+base is 422; merge is a squash commit on the default
//   branch (200 {sha, merged:true, message}); unknown PR 404, conflicted PR 405.
// - a repo under another owner is 404, never 403.
import {
  store,
  VIEWER_LOGIN,
  VIEWER_NAME,
  VIEWER_EMAIL,
  DEFAULT_BRANCH,
  nextSeq,
  nextGlobalId,
  isoTime,
  type Repo,
  type Issue,
  type Pull,
  type Label,
} from './store.ts';
import {
  buildTree,
  buildCommit,
  parseTree,
  parseCommit,
  makeObject,
  type TreeEntry,
} from './git.ts';
import {
  sendJson,
  sendNoContent,
  sendError,
  parseJsonBody,
  isPlainObject,
  type Ctx,
} from './http-util.ts';

// ---------------------------------------------------------------------------
// Serialisers (GitHub-shaped; the suite reads the fields it asserts and
// ignores the rest, so supersets are safe).
// ---------------------------------------------------------------------------

export function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: repo.id, type: 'User' },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: isoTime(repo.createdSeq),
    updated_at: isoTime(repo.createdSeq),
    html_url: `https://example.invalid/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    has_issues: true,
  };
}

export function labelJson(label: Label): Record<string, unknown> {
  return { name: label.name, color: label.color, description: '' };
}

export function issueJson(issue: Issue, repo: Repo): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map(labelJson),
    user: { login: VIEWER_LOGIN, id: repo.id, type: 'User' },
    created_at: isoTime(issue.createdSeq),
    updated_at: isoTime(issue.createdSeq),
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    comments: issue.comments.length,
  };
}

export function commentJson(comment: Issue['comments'][number], repo: Repo): Record<string, unknown> {
  return {
    id: comment.id,
    body: comment.body,
    user: { login: comment.userLogin, id: repo.id, type: 'User' },
    created_at: isoTime(comment.createdSeq),
    updated_at: isoTime(comment.createdSeq),
  };
}

export function pullJson(pull: Pull, repo: Repo): Record<string, unknown> {
  return {
    id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    head: { ref: pull.headRef, sha: pull.headSha, label: `${repo.owner}:${pull.headRef}` },
    base: { ref: pull.baseRef, sha: pull.baseSha, label: `${repo.owner}:${pull.baseRef}` },
    user: { login: VIEWER_LOGIN, id: repo.id, type: 'User' },
    created_at: isoTime(pull.createdSeq),
    updated_at: isoTime(pull.createdSeq),
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/pull/${pull.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
  };
}

// ---------------------------------------------------------------------------
// Git helpers for the contents / commits surface
// ---------------------------------------------------------------------------

/** Resolve a ref (branch name, commit sha, or undefined = default branch). */
export function resolveRefToCommit(repo: Repo, ref: string | undefined): string | null {
  if (!ref) return store.defaultTip(repo);
  if (repo.objects.has(ref)) {
    const obj = repo.objects.get(ref)!;
    if (obj.type === 'commit') return ref;
    if (obj.type === 'tag') {
      // Peel one level (tags in this suite point at commits).
      const target = parseTagText(obj.data);
      if (target && repo.objects.get(target)?.type === 'commit') return target;
    }
    return null;
  }
  return repo.refs.get(`refs/heads/${ref}`) ?? null;
}

function parseTagText(data: Buffer): string | null {
  const text = data.toString('utf8');
  const match = text.match(/^object ([0-9a-f]{40})$/m);
  return match ? match[1] : null;
}

/** Flatten a tree (recursively) into path -> {mode, oid}. */
function flattenTree(repo: Repo, treeOid: string, prefix: string, out: Map<string, { mode: string; oid: string }>): void {
  const obj = repo.objects.get(treeOid);
  if (!obj || obj.type !== 'tree') return;
  for (const entry of parseTree(obj.data)) {
    const path = prefix ? `${prefix}/${entry.name}` : entry.name;
    if (entry.isDir) flattenTree(repo, entry.oid, path, out);
    else out.set(path, { mode: entry.mode, oid: entry.oid });
  }
}

/** Build a tree from a flat path map, storing every intermediate tree. */
function buildTreeFromFlat(repo: Repo, flat: Map<string, { mode: string; oid: string }>): string {
  const build = (prefix: string): string => {
    const entries: TreeEntry[] = [];
    const dirs = new Map<string, string>();
    for (const path of flat.keys()) {
      if (prefix !== '' && !path.startsWith(`${prefix}/`)) continue;
      const rest = prefix === '' ? path : path.slice(prefix.length + 1);
      const slash = rest.indexOf('/');
      if (slash < 0) {
        const entry = flat.get(path)!;
        entries.push({ mode: entry.mode, name: rest, oid: entry.oid, isDir: false });
      } else {
        dirs.set(rest.slice(0, slash), '');
      }
    }
    for (const dir of [...dirs.keys()].sort()) {
      const subPrefix = prefix === '' ? dir : `${prefix}/${dir}`;
      const subOid = build(subPrefix);
      entries.push({ mode: '40000', name: dir, oid: subOid, isDir: true });
    }
    const { oid, object } = buildTree(entries);
    repo.objects.set(oid, object);
    return oid;
  };
  return build('');
}

/** Walk a tree to a path; returns the entry (file or directory) or null. */
function lookupPath(
  repo: Repo,
  treeOid: string,
  segments: string[],
): { type: 'file' | 'dir'; oid: string; mode: string } | null {
  let current = treeOid;
  for (let i = 0; i < segments.length; i += 1) {
    const obj = repo.objects.get(current);
    if (!obj || obj.type !== 'tree') return null;
    const entry = parseTree(obj.data).find((e) => e.name === segments[i]);
    if (!entry) return null;
    if (i === segments.length - 1) {
      return { type: entry.isDir ? 'dir' : 'file', oid: entry.oid, mode: entry.mode };
    }
    if (!entry.isDir) return null;
    current = entry.oid;
  }
  return { type: 'dir', oid: current, mode: '40000' };
}

/** Create a commit on a branch from a flat file map (empty map = empty tree). */
function commitTree(
  repo: Repo,
  message: string,
  flat: Map<string, { mode: string; oid: string }>,
  parent: string | null,
  branch: string,
): string {
  const treeOid = buildTreeFromFlat(repo, flat);
  const seq = nextSeq();
  const stamp = `${1735689600 + seq} +0000`;
  const ident = `${VIEWER_NAME} <${VIEWER_EMAIL}> ${stamp}`;
  const { oid, object } = buildCommit(treeOid, parent ? [parent] : [], message, ident, ident);
  repo.objects.set(oid, object);
  repo.refs.set(`refs/heads/${branch}`, oid);
  repo.cachedJson = null;
  return oid;
}

/** Flat path map of the tree at a commit. Exported for the GraphQL executor. */
export function flatTreeAtCommit(repo: Repo, sha: string): Map<string, { mode: string; oid: string }> {
  const out = new Map<string, { mode: string; oid: string }>();
  const obj = repo.objects.get(sha);
  if (obj?.type === 'commit') flattenTree(repo, parseCommit(obj.data).tree, '', out);
  return out;
}

/**
 * Three-way merge conflict test for a PR (shared by the REST merge and the
 * GraphQL `mergeable` field): a file changed on the PR side that was also
 * changed between the PR's base commit and the current base tip, with
 * different resulting content, is a conflict.
 */
export function mergeConflict(repo: Repo, pull: Pull): boolean {
  const base0 = flatTreeAtCommit(repo, pull.baseSha);
  const head = flatTreeAtCommit(repo, pull.headSha);
  const baseTipSha = repo.refs.get(`refs/heads/${pull.baseRef}`);
  if (!baseTipSha) return false;
  const tip = flatTreeAtCommit(repo, baseTipSha);
  for (const path of new Set([...base0.keys(), ...head.keys()])) {
    const base = base0.get(path)?.oid ?? null;
    const headOid = head.get(path)?.oid ?? null;
    if (base === headOid) continue;
    const tipOid = tip.get(path)?.oid ?? null;
    if (tipOid !== base && tipOid !== headOid) return true;
  }
  return false;
}
function listCommits(repo: Repo, tip: string, limit = 100): Array<{ sha: string; commit: { message: string } }> {
  const out: Array<{ sha: string; commit: { message: string } }> = [];
  let current: string | null = tip;
  const seen = new Set<string>();
  while (current && out.length < limit && !seen.has(current)) {
    seen.add(current);
    const obj = repo.objects.get(current);
    if (!obj || obj.type !== 'commit') break;
    const info = parseCommit(obj.data);
    out.push({ sha: current, commit: { message: info.message } });
    current = info.parents[0] ?? null;
  }
  return out;
}

/** Line counts for a diff between two blob contents. */
function lineDiff(oldBytes: Buffer | null, newBytes: Buffer | null): { additions: number; deletions: number } {
  const countLines = (bytes: Buffer | null): string[] => {
    if (!bytes || bytes.length === 0) return [];
    const text = bytes.toString('utf8');
    const lines = text.split('\n');
    if (text.endsWith('\n')) lines.pop();
    return lines;
  };
  const oldLines = countLines(oldBytes);
  const newLines = countLines(newBytes);
  const oldCount = new Map<string, number>();
  for (const line of oldLines) oldCount.set(line, (oldCount.get(line) ?? 0) + 1);
  let additions = 0;
  for (const line of newLines) {
    const remaining = oldCount.get(line) ?? 0;
    if (remaining > 0) oldCount.set(line, remaining - 1);
    else additions += 1;
  }
  let deletions = 0;
  for (const remaining of oldCount.values()) deletions += remaining;
  return { additions, deletions };
}

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------

function paginate<T>(ctx: Ctx, items: T[], defaultPerPage = 30): { page: T[]; link: string | null } {
  const perPageRaw = Number.parseInt(ctx.query.get('per_page') ?? '', 10);
  const pageRaw = Number.parseInt(ctx.query.get('page') ?? '', 10);
  const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(perPageRaw, 100) : defaultPerPage;
  const page = Number.isFinite(pageRaw) && pageRaw > 0 ? pageRaw : 1;
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  let link: string | null = null;
  if (start + perPage < items.length) {
    const host = ctx.req.headers.host ?? '127.0.0.1';
    const url = new URL(ctx.req.url ?? '/', `http://${host}`);
    url.searchParams.set('per_page', String(perPage));
    url.searchParams.set('page', String(page + 1));
    link = `<${url.toString()}>; rel="next"`;
  }
  return { page: slice, link };
}

function sendPage<T>(ctx: Ctx, items: T[]): void {
  const { page, link } = paginate(ctx, items);
  sendJson(ctx, 200, page, link ? { Link: link } : undefined);
}

// ---------------------------------------------------------------------------
// Route helpers
// ---------------------------------------------------------------------------

function decodeSegment(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

/** Match /api/v3/repos/{owner}/{repo}... and load the repo (404 otherwise). */
function matchRepo(ctx: Ctx, rest: string): { repo: Repo; tail: string } | null {
  const parts = rest.split('/').map(decodeSegment);
  if (parts.length < 2) return null;
  const [owner, name, ...tail] = parts;
  if (owner !== VIEWER_LOGIN) return null;
  const repo = store.getRepo(owner, name);
  if (!repo) return null;
  return { repo, tail: tail.join('/') };
}

function repoNotFound(ctx: Ctx): boolean {
  sendError(ctx, 404, 'Not Found');
  return true;
}

// ---------------------------------------------------------------------------
// The router
// ---------------------------------------------------------------------------

export function handleRest(ctx: Ctx): boolean {
  if (!ctx.pathname.startsWith('/api/v3')) return false;
  const path = ctx.pathname === '/api/v3' ? '/api/v3/' : ctx.pathname;
  const method = ctx.method;

  // API root and meta.
  if (path === '/api/v3/') {
    if (method !== 'GET') return sendError(ctx, 404, 'Not Found'), true;
    return sendJson(ctx, 200, {
      current_user_url: '/api/v3/user',
      rate_limit_url: '/api/v3/rate_limit',
      repository_url: '/api/v3/repos/{owner}/{repo}',
    }, { 'X-OAuth-Scopes': 'repo, read:org' }), true;
  }
  if (path === '/api/v3/meta') {
    if (method !== 'GET') return sendError(ctx, 404, 'Not Found'), true;
    return sendJson(ctx, 200, { installed_version: '1.0.0', verifiable_password_authentication: true }), true;
  }
  if (path === '/api/v3/user') {
    if (method !== 'GET') return sendError(ctx, 404, 'Not Found'), true;
    return sendJson(ctx, 200, {
      login: VIEWER_LOGIN,
      id: 1,
      name: VIEWER_NAME,
      email: VIEWER_EMAIL,
      type: 'User',
    }), true;
  }
  if (path === '/api/v3/user/repos') {
    if (method === 'GET') {
      return sendPage(ctx, store.listViewerRepos().map(repoJson)), true;
    }
    if (method === 'POST') {
      const body = parseJsonBody(ctx);
      if (body === null) return sendError(ctx, 400, 'malformed JSON body'), true;
      if (!isPlainObject(body) || typeof body.name !== 'string' || body.name === '') {
        return sendError(ctx, 422, 'name is required'), true;
      }
      const repo = store.createRepo(
        body.name,
        typeof body.description === 'string' ? body.description : '',
        body.private === true,
      );
      if (!repo) return sendError(ctx, 422, 'name already exists on this account'), true;
      return sendJson(ctx, 201, repoJson(repo)), true;
    }
    return sendError(ctx, 404, 'Not Found'), true;
  }

  // /api/v3/repos/{owner}/{repo}/...
  if (path.startsWith('/api/v3/repos/')) {
    const rest = path.slice('/api/v3/repos/'.length);
    const matched = matchRepo(ctx, rest);
    if (!matched) return repoNotFound(ctx);
    return handleRepoRoute(ctx, matched.repo, matched.tail);
  }

  return sendError(ctx, 404, 'Not Found'), true;
}

function handleRepoRoute(ctx: Ctx, repo: Repo, tail: string): boolean {
  const method = ctx.method;
  const segments = tail === '' ? [] : tail.split('/');

  // The repo itself: GET / PATCH / DELETE.
  if (segments.length === 0) {
    if (method === 'GET') {
      if (repo.cachedJson) {
        sendJson(ctx, 200, repo.cachedJson);
        return true;
      }
      const json = JSON.stringify(repoJson(repo));
      repo.cachedJson = json;
      sendJson(ctx, 200, json);
      return true;
    }
    if (method === 'PATCH') {
      const body = parseJsonBody(ctx);
      if (body === null) return sendError(ctx, 400, 'malformed JSON body'), true;
      if (isPlainObject(body) && typeof body.description === 'string') {
        repo.description = body.description;
        repo.cachedJson = null;
      }
      return sendJson(ctx, 200, repoJson(repo)), true;
    }
    if (method === 'DELETE') {
      store.deleteRepo(repo.owner, repo.name);
      return sendNoContent(ctx), true;
    }
    return sendError(ctx, 404, 'Not Found'), true;
  }

  const head = segments[0];

  // Branches.
  if (head === 'branches') {
    if (method === 'GET' && segments.length === 1) {
      const branches = [...repo.refs.keys()]
        .filter((name) => name.startsWith('refs/heads/'))
        .sort()
        .map((name) => ({ name: name.slice('refs/heads/'.length), commit: { sha: repo.refs.get(name)! } }));
      return sendJson(ctx, 200, branches), true;
    }
    if (method === 'GET' && segments.length === 2) {
      const tip = repo.refs.get(`refs/heads/${decodeSegment(segments[1])}`);
      if (!tip) return repoNotFound(ctx);
      return sendJson(ctx, 200, { name: decodeSegment(segments[1]), commit: { sha: tip } }), true;
    }
    return sendError(ctx, 404, 'Not Found'), true;
  }

  // Commits.
  if (head === 'commits') {
    if (method === 'GET' && segments.length === 1) {
      const tip = resolveRefToCommit(repo, ctx.query.get('sha') ?? undefined);
      if (!tip) return repoNotFound(ctx);
      return sendJson(ctx, 200, listCommits(repo, tip)), true;
    }
    return sendError(ctx, 404, 'Not Found'), true;
  }

  // git/refs.
  if (head === 'git' && segments[1] === 'refs') {
    if (method === 'POST' && segments.length === 2) {
      const body = parseJsonBody(ctx);
      if (body === null) return sendError(ctx, 400, 'malformed JSON body'), true;
      if (!isPlainObject(body) || typeof body.ref !== 'string' || typeof body.sha !== 'string') {
        return sendError(ctx, 422, 'ref and sha are required'), true;
      }
      if (!repo.objects.has(body.sha)) return sendError(ctx, 422, 'object does not exist'), true;
      if (repo.refs.has(body.ref)) return sendError(ctx, 422, 'reference already exists'), true;
      repo.refs.set(body.ref, body.sha);
      return sendJson(ctx, 201, { ref: body.ref, object: { sha: body.sha } }), true;
    }
    if (method === 'GET' && segments.length >= 4 && segments[2] === 'heads') {
      const branch = segments.slice(3).join('/');
      const tip = repo.refs.get(`refs/heads/${branch}`);
      if (!tip) return repoNotFound(ctx);
      return sendJson(ctx, 200, { ref: `refs/heads/${branch}`, object: { sha: tip } }), true;
    }
    return sendError(ctx, 404, 'Not Found'), true;
  }

  // Contents.
  if (head === 'contents') {
    const filePath = segments.slice(1).map(decodeSegment).join('/');
    return handleContents(ctx, repo, filePath);
  }

  // Labels.
  if (head === 'labels') {
    if (method === 'GET' && segments.length === 1) {
      return sendJson(ctx, 200, [...repo.labels.values()].map(labelJson)), true;
    }
    if (method === 'POST' && segments.length === 1) {
      const body = parseJsonBody(ctx);
      if (body === null) return sendError(ctx, 400, 'malformed JSON body'), true;
      if (!isPlainObject(body) || typeof body.name !== 'string' || body.name === '') {
        return sendError(ctx, 422, 'name is required'), true;
      }
      if (repo.labels.has(body.name)) return sendError(ctx, 422, 'label already exists'), true;
      const label: Label = {
        name: body.name,
        color: typeof body.color === 'string' ? body.color : 'ededed',
      };
      repo.labels.set(label.name, label);
      return sendJson(ctx, 201, labelJson(label)), true;
    }
    return sendError(ctx, 404, 'Not Found'), true;
  }

  // Issues.
  if (head === 'issues') {
    return handleIssues(ctx, repo, segments.slice(1));
  }

  // Pull requests.
  if (head === 'pulls') {
    return handlePulls(ctx, repo, segments.slice(1));
  }

  return sendError(ctx, 404, 'Not Found'), true;
}

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

function handleContents(ctx: Ctx, repo: Repo, filePath: string): boolean {
  const method = ctx.method;
  const refParam = ctx.query.get('ref') ?? undefined;

  if (method === 'GET') {
    const tip = resolveRefToCommit(repo, refParam);
    if (!tip) return repoNotFound(ctx);
    const commit = parseCommit(repo.objects.get(tip)!.data);
    const segments = filePath === '' ? [] : filePath.split('/');
    if (segments.length === 0) {
      // Root listing.
      return sendJson(ctx, 200, listDirectory(repo, commit.tree, '')), true;
    }
    const found = lookupPath(repo, commit.tree, segments);
    if (!found) return repoNotFound(ctx);
    if (found.type === 'dir') {
      return sendJson(ctx, 200, listDirectory(repo, found.oid, filePath)), true;
    }
    const blob = repo.objects.get(found.oid)!;
    return sendJson(ctx, 200, {
      type: 'file',
      encoding: 'base64',
      size: blob.data.length,
      name: segments[segments.length - 1],
      path: filePath,
      content: blob.data.toString('base64'),
      sha: found.oid,
    }), true;
  }

  if (method === 'PUT') {
    const body = parseJsonBody(ctx);
    if (body === null) return sendError(ctx, 400, 'malformed JSON body'), true;
    if (!isPlainObject(body) || typeof body.message !== 'string' || typeof body.content !== 'string') {
      return sendError(ctx, 422, 'message and content are required'), true;
    }
    const branch = typeof body.branch === 'string' ? body.branch : repo.defaultBranch;
    const tip = repo.refs.get(`refs/heads/${branch}`) ?? null;
    const flat = new Map<string, { mode: string; oid: string }>();
    if (tip) {
      const commit = parseCommit(repo.objects.get(tip)!.data);
      flattenTree(repo, commit.tree, '', flat);
    }
    const existing = flat.get(filePath);
    if (existing) {
      if (typeof body.sha !== 'string') return sendError(ctx, 422, 'sha is required to update a file'), true;
      if (body.sha !== existing.oid) return sendError(ctx, 409, 'sha does not match the current file'), true;
    } else if (typeof body.sha === 'string') {
      return sendError(ctx, 422, 'sha must not be provided when creating a file'), true;
    }
    let contentBytes: Buffer;
    try {
      contentBytes = Buffer.from(body.content, 'base64');
    } catch {
      return sendError(ctx, 422, 'content is not valid base64'), true;
    }
    const { oid: blobOid, object: blob } = makeObject('blob', contentBytes);
    repo.objects.set(blobOid, blob);
    flat.set(filePath, { mode: '100644', oid: blobOid });
    const commitOid = commitTree(repo, body.message, flat, tip, branch);
    const status = existing ? 200 : 201;
    return sendJson(ctx, status, {
      content: {
        name: filePath.split('/').pop(),
        path: filePath,
        sha: blobOid,
        size: contentBytes.length,
        type: 'file',
        encoding: 'base64',
        content: body.content,
      },
      commit: { sha: commitOid, message: body.message },
    }), true;
  }

  if (method === 'DELETE') {
    const body = parseJsonBody(ctx);
    if (body === null) return sendError(ctx, 400, 'malformed JSON body'), true;
    if (!isPlainObject(body) || typeof body.message !== 'string') {
      return sendError(ctx, 422, 'message is required'), true;
    }
    const branch = typeof body.branch === 'string' ? body.branch : repo.defaultBranch;
    const tip = repo.refs.get(`refs/heads/${branch}`) ?? null;
    if (!tip) return repoNotFound(ctx);
    const commit = parseCommit(repo.objects.get(tip)!.data);
    const flat = new Map<string, { mode: string; oid: string }>();
    flattenTree(repo, commit.tree, '', flat);
    const existing = flat.get(filePath);
    if (!existing) return repoNotFound(ctx);
    if (typeof body.sha === 'string' && body.sha !== existing.oid) {
      return sendError(ctx, 409, 'sha does not match the current file'), true;
    }
    flat.delete(filePath);
    const commitOid = commitTree(repo, body.message, flat, tip, branch);
    return sendJson(ctx, 200, { commit: { sha: commitOid, message: body.message } }), true;
  }

  return sendError(ctx, 404, 'Not Found'), true;
}

function listDirectory(repo: Repo, treeOid: string, prefix: string): Array<Record<string, unknown>> {
  const obj = repo.objects.get(treeOid);
  if (!obj || obj.type !== 'tree') return [];
  return parseTree(obj.data)
    .sort((a, b) => a.name.localeCompare(b.name))
    .map((entry) => ({
      type: entry.isDir ? 'dir' : 'file',
      name: entry.name,
      path: prefix ? `${prefix}/${entry.name}` : entry.name,
      sha: entry.oid,
      size: entry.isDir ? 0 : repo.objects.get(entry.oid)?.data.length ?? 0,
    }));
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

function handleIssues(ctx: Ctx, repo: Repo, tail: string[]): boolean {
  const method = ctx.method;

  // /issues (list / create)
  if (tail.length === 0) {
    if (method === 'GET') {
      const state = ctx.query.get('state') ?? 'open';
      const direction = ctx.query.get('direction') ?? 'desc';
      let issues = repo.issues.filter((issue) =>
        state === 'all' ? true : issue.state === state,
      );
      issues = [...issues].sort((a, b) =>
        direction === 'asc' ? a.number - b.number : b.number - a.number,
      );
      return sendPage(ctx, issues.map((issue) => issueJson(issue, repo))), true;
    }
    if (method === 'POST') {
      const body = parseJsonBody(ctx);
      if (body === null) return sendError(ctx, 400, 'malformed JSON body'), true;
      if (!isPlainObject(body) || typeof body.title !== 'string' || body.title === '') {
        return sendError(ctx, 422, 'title is required'), true;
      }
      const labelNames: string[] = Array.isArray(body.labels)
        ? body.labels.filter((l): l is string => typeof l === 'string')
        : [];
      const labels: Label[] = [];
      for (const name of labelNames) {
        const label = repo.labels.get(name);
        if (!label) return sendError(ctx, 422, `label does not exist: ${name}`), true;
        labels.push(label);
      }
      const issue: Issue = {
        id: nextGlobalId(),
        number: repo.nextNumber,
        title: body.title,
        body: typeof body.body === 'string' ? body.body : '',
        state: 'open',
        labels,
        comments: [],
        createdSeq: nextSeq(),
        cachedJson: null,
      };
      repo.nextNumber += 1;
      repo.issues.push(issue);
      return sendJson(ctx, 201, issueJson(issue, repo)), true;
    }
    return sendError(ctx, 404, 'Not Found'), true;
  }

  // /issues/{n}
  const number = Number.parseInt(tail[0], 10);
  if (!Number.isInteger(number)) return repoNotFound(ctx);
  const issue = repo.issues.find((entry) => entry.number === number);
  if (!issue) return repoNotFound(ctx);

  // /issues/{n}/comments
  if (tail.length === 2 && tail[1] === 'comments') {
    if (method === 'GET') {
      return sendJson(ctx, 200, issue.comments.map((c) => commentJson(c, repo))), true;
    }
    if (method === 'POST') {
      const body = parseJsonBody(ctx);
      if (body === null) return sendError(ctx, 400, 'malformed JSON body'), true;
      if (!isPlainObject(body) || typeof body.body !== 'string' || body.body === '') {
        return sendError(ctx, 422, 'body is required'), true;
      }
      const comment = {
        id: nextGlobalId(),
        body: body.body,
        userLogin: VIEWER_LOGIN,
        createdSeq: nextSeq(),
      };
      issue.comments.push(comment);
      issue.cachedJson = null;
      return sendJson(ctx, 201, commentJson(comment, repo)), true;
    }
    return sendError(ctx, 404, 'Not Found'), true;
  }

  if (tail.length === 1) {
    if (method === 'GET') {
      // Hot path (the 2000-read workload): serve the cached serialisation.
      if (issue.cachedJson) {
        sendJson(ctx, 200, issue.cachedJson);
        return true;
      }
      const json = JSON.stringify(issueJson(issue, repo));
      issue.cachedJson = json;
      sendJson(ctx, 200, json);
      return true;
    }
    if (method === 'PATCH') {
      const body = parseJsonBody(ctx);
      if (body === null) return sendError(ctx, 400, 'malformed JSON body'), true;
      if (isPlainObject(body)) {
        if (body.state === 'open' || body.state === 'closed') issue.state = body.state;
        if (typeof body.title === 'string') issue.title = body.title;
        if (typeof body.body === 'string') issue.body = body.body;
        issue.cachedJson = null;
      }
      return sendJson(ctx, 200, issueJson(issue, repo)), true;
    }
    return sendError(ctx, 404, 'Not Found'), true;
  }

  return sendError(ctx, 404, 'Not Found'), true;
}

// ---------------------------------------------------------------------------
// Pull requests
// ---------------------------------------------------------------------------

function handlePulls(ctx: Ctx, repo: Repo, tail: string[]): boolean {
  const method = ctx.method;

  // /pulls (list / create)
  if (tail.length === 0) {
    if (method === 'GET') {
      const state = ctx.query.get('state') ?? 'open';
      const pulls = repo.pulls.filter((pull) =>
        state === 'all' ? true : pull.state === state,
      );
      return sendPage(ctx, pulls.map((pull) => pullJson(pull, repo))), true;
    }
    if (method === 'POST') {
      const body = parseJsonBody(ctx);
      if (body === null) return sendError(ctx, 400, 'malformed JSON body'), true;
      if (
        !isPlainObject(body) ||
        typeof body.title !== 'string' ||
        typeof body.head !== 'string' ||
        typeof body.base !== 'string'
      ) {
        return sendError(ctx, 422, 'title, head and base are required'), true;
      }
      const headSha = repo.refs.get(`refs/heads/${body.head}`);
      const baseSha = repo.refs.get(`refs/heads/${body.base}`);
      if (!headSha || !baseSha) return sendError(ctx, 422, 'head or base branch does not exist'), true;
      const duplicate = repo.pulls.find(
        (pull) => pull.state === 'open' && pull.headRef === body.head && pull.baseRef === body.base,
      );
      if (duplicate) return sendError(ctx, 422, 'a pull request already exists for these branches'), true;
      const pull: Pull = {
        id: nextGlobalId(),
        number: repo.nextNumber,
        title: body.title,
        body: typeof body.body === 'string' ? body.body : '',
        state: 'open',
        merged: false,
        mergedAt: null,
        mergeCommitSha: null,
        headRef: body.head,
        baseRef: body.base,
        headSha,
        baseSha,
        createdSeq: nextSeq(),
      };
      repo.nextNumber += 1;
      repo.pulls.push(pull);
      return sendJson(ctx, 201, pullJson(pull, repo)), true;
    }
    return sendError(ctx, 404, 'Not Found'), true;
  }

  const number = Number.parseInt(tail[0], 10);
  if (!Number.isInteger(number)) return repoNotFound(ctx);
  const pull = repo.pulls.find((entry) => entry.number === number);
  if (!pull) return repoNotFound(ctx);

  // /pulls/{n}/files
  if (tail.length === 2 && tail[1] === 'files') {
    if (method !== 'GET') return sendError(ctx, 404, 'Not Found'), true;
    return sendJson(ctx, 200, pullFiles(repo, pull)), true;
  }

  // /pulls/{n}/merge
  if (tail.length === 2 && tail[1] === 'merge') {
    if (method !== 'PUT') return sendError(ctx, 404, 'Not Found'), true;
    const body = parseJsonBody(ctx);
    if (body === null) return sendError(ctx, 400, 'malformed JSON body'), true;
    return mergePull(ctx, repo, pull);
  }

  if (tail.length === 1) {
    if (method === 'GET') return sendJson(ctx, 200, pullJson(pull, repo)), true;
    return sendError(ctx, 404, 'Not Found'), true;
  }

  return sendError(ctx, 404, 'Not Found'), true;
}

/** Files changed between a PR's base and head commits. */
export function pullFiles(repo: Repo, pull: Pull): Array<Record<string, unknown>> {
  const baseFlat = flatTreeAtCommit(repo, pull.baseSha);
  const headFlat = flatTreeAtCommit(repo, pull.headSha);
  const paths = new Set([...baseFlat.keys(), ...headFlat.keys()]);
  const files: Array<Record<string, unknown>> = [];
  for (const path of [...paths].sort()) {
    const base = baseFlat.get(path);
    const head = headFlat.get(path);
    if (base && head && base.oid === head.oid) continue;
    if (!base && head) {
      const blob = repo.objects.get(head.oid);
      const { additions } = lineDiff(null, blob?.data ?? null);
      files.push({ filename: path, status: 'added', additions: Math.max(additions, 1), deletions: 0, changes: Math.max(additions, 1) });
    } else if (base && !head) {
      const blob = repo.objects.get(base.oid);
      const { deletions } = lineDiff(blob?.data ?? null, null);
      files.push({ filename: path, status: 'removed', additions: 0, deletions, changes: deletions });
    } else if (base && head) {
      const { additions, deletions } = lineDiff(
        repo.objects.get(base.oid)?.data ?? null,
        repo.objects.get(head.oid)?.data ?? null,
      );
      files.push({ filename: path, status: 'modified', additions, deletions, changes: additions + deletions });
    }
  }
  return files;
}

/**
 * Squash-merge a PR onto its base branch (shared by the REST merge endpoint
 * and the GraphQL mergePullRequest mutation). Returns the merge commit sha on
 * success, or a reason string on failure.
 */
export function squashMerge(repo: Repo, pull: Pull): { ok: boolean; sha?: string; reason?: string } {
  if (pull.state !== 'open') return { ok: false, reason: 'pull request is not open' };
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
  if (!baseTip) return { ok: false, reason: 'base branch not found' };
  if (mergeConflict(repo, pull)) return { ok: false, reason: 'merge conflict' };
  const baseTipFlat = flatTreeAtCommit(repo, baseTip);
  const base0Flat = flatTreeAtCommit(repo, pull.baseSha);
  const headFlat = flatTreeAtCommit(repo, pull.headSha);
  const merged = new Map(baseTipFlat);
  for (const path of new Set([...base0Flat.keys(), ...headFlat.keys()])) {
    const base0 = base0Flat.get(path);
    const head = headFlat.get(path);
    if (base0?.oid === head?.oid) continue; // unchanged on the PR side
    if (head) merged.set(path, head);
    else merged.delete(path);
  }
  const seq = nextSeq();
  const stamp = `${1735689600 + seq} +0000`;
  const ident = `${VIEWER_NAME} <${VIEWER_EMAIL}> ${stamp}`;
  const treeOid = buildTreeFromFlat(repo, merged);
  const message = `Merge pull request #${pull.number} from ${repo.owner}/${pull.headRef}\n\n${pull.title}`;
  const { oid: mergeSha, object } = buildCommit(treeOid, [baseTip], message, ident, ident);
  repo.objects.set(mergeSha, object);
  repo.refs.set(`refs/heads/${pull.baseRef}`, mergeSha);
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = isoTime(seq);
  pull.mergeCommitSha = mergeSha;
  repo.cachedJson = null;
  return { ok: true, sha: mergeSha };
}

function mergePull(ctx: Ctx, repo: Repo, pull: Pull): boolean {
  const result = squashMerge(repo, pull);
  if (!result.ok) {
    if (result.reason === 'base branch not found') return repoNotFound(ctx);
    return sendError(ctx, 405, result.reason ?? 'merge conflict'), true;
  }
  return sendJson(ctx, 200, {
    sha: result.sha,
    merged: true,
    message: 'Pull request successfully merged',
  }), true;
}
