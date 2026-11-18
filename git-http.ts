// Git smart HTTP: info/refs advertisements (v2 upload-pack, v0 both services),
// protocol v2 ls-refs and fetch, and protocol v0 receive-pack (push).
//
// Wire facts (from sdk/conformance/git-client.ts + checks-git.ts):
// - v2 advertisement: first data pkt-line exactly `version 2`, then capability
//   lines, then flush. No `# service=` line.
// - v0 advertisement: `# service=<svc>` line, flush, then ref lines with the
//   capability list after NUL on the first line only. Empty repo:
//   `0000...0000 capabilities^{}\0<caps>`.
// - v2 fetch response sections, in order: `acknowledgments` (NAK, or ACK +
//   ready), `shallow-info` (`shallow`/`unshallow`), `packfile` (sideband band
//   1 = pack bytes, band 2 = progress, band 3 = error), flush at the end. The
//   packfile section must carry >= 1 sideband packet even when empty.
// - receive-pack report: pkt-lined `unpack ok` + per-ref `ok`/`ng` lines,
//   carried inside sideband band 1, then flush.
// - A push's pack is unpacked even when a ref update is refused.
import { store, touchGit, VIEWER_LOGIN, DEFAULT_BRANCH, type Repo } from './store.ts';
import {
  ZERO_OID,
  decodePktLines,
  pktLine,
  sidebandPacket,
  PKT_FLUSH,
  readPack,
  writePack,
  writeEmptyPack,
  computeFetch,
  isAncestor,
  peelToCommit,
  type PktItem,
} from './git.ts';
import { sendBuffer, sendError, type Ctx } from './http-util.ts';

const AGENT = 'makefun-github-forge/1';

const UPLOAD_V0_CAPS = `symref=HEAD:refs/heads/${DEFAULT_BRANCH} side-band-64k thin-pack ofs-delta multi_ack shallow agent=${AGENT}`;
const RECEIVE_V0_CAPS = `report-status side-band-64k delete-refs ofs-delta agent=${AGENT}`;

const UPLOAD_PACK_AD = 'application/x-git-upload-pack-advertisement';
const RECEIVE_PACK_AD = 'application/x-git-receive-pack-advertisement';
const UPLOAD_PACK_RESULT = 'application/x-git-upload-pack-result';
const RECEIVE_PACK_RESULT = 'application/x-git-receive-pack-result';

interface RefLine {
  oid: string;
  name: string;
  peeled?: string;
}

/** All refs of a repo, sorted by name, with peeled targets for annotated tags. */
function refLines(repo: Repo): RefLine[] {
  const names = [...repo.refs.keys()].sort();
  const lines: RefLine[] = [];
  for (const name of names) {
    const oid = repo.refs.get(name)!;
    const line: RefLine = { oid, name };
    const obj = repo.objects.get(oid);
    if (obj && obj.type === 'tag') {
      const target = peelToCommit(repo.objects, oid);
      if (target) line.peeled = target;
    }
    lines.push(line);
  }
  return lines;
}

function headTip(repo: Repo): string {
  return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? ZERO_OID;
}

/** v0 advertisement body for a service. */
function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps = service === 'git-upload-pack' ? UPLOAD_V0_CAPS : RECEIVE_V0_CAPS;
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), PKT_FLUSH];
  const refs = refLines(repo);
  if (refs.length === 0) {
    parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    const tip = headTip(repo);
    parts.push(pktLine(`${tip} HEAD\0${caps}\n`));
    for (const ref of refs) {
      parts.push(pktLine(`${ref.oid} ${ref.name}\n`));
      if (ref.peeled) parts.push(pktLine(`${ref.peeled} ${ref.name}^{}\n`));
    }
  }
  return Buffer.concat(parts);
}

/** v2 advertisement body (upload-pack only). Constant: it names no repo state. */
const V2_ADVERTISEMENT: Buffer = Buffer.concat([
  pktLine('version 2\n'),
  pktLine(`agent=${AGENT}\n`),
  pktLine('ls-refs=unborn\n'),
  pktLine('fetch=shallow\n'),
  pktLine('object-format=sha1\n'),
  PKT_FLUSH,
]);

/**
 * Clone-workload cache: 50 concurrent shallow clones of one tip ask for the
 * same advertisement, the same ls-refs list and the same fetch pack. Rebuilding
 * those bytes per request is pure repeated CPU on the server's single thread,
 * so identical responses are cached per repo and dropped by touchGit() the
 * moment the object/ref graph changes. A cached body is a pure function of
 * (repo graph, request args), so the bytes served are identical to a fresh
 * build and the stored state is untouched.
 */
/** Upper bound for one cached git response body (a depth-1 clone pack is < 1 KiB). */
const GIT_CACHE_MAX_BODY = 1 << 20;
/** Upper bound for cached responses per repo; the map is cleared when full. */
const GIT_CACHE_MAX_ENTRIES = 128;

/** Store a git smart-HTTP response body for an identical later request. */
function cacheGitResponse(repo: Repo, key: string, body: Buffer): void {
  if (body.length > GIT_CACHE_MAX_BODY) return;
  if (repo.gitCache.size >= GIT_CACHE_MAX_ENTRIES) repo.gitCache.clear();
  repo.gitCache.set(key, body);
}

function handleInfoRefs(ctx: Ctx, repo: Repo | undefined): boolean {
  const service = ctx.query.get('service');
  if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
    sendError(ctx, 400, 'service parameter is required');
    return true;
  }
  if (!repo) {
    sendError(ctx, 404, 'Not Found');
    return true;
  }
  const wantsV2 = service === 'git-upload-pack' && ctx.req.headers['git-protocol'] === 'version=2';
  if (wantsV2) {
    sendBuffer(ctx, 200, V2_ADVERTISEMENT, UPLOAD_PACK_AD);
  } else {
    const cacheKey = `v0-ad|${service}`;
    const cached = repo.gitCache.get(cacheKey);
    if (cached !== undefined) {
      sendBuffer(
        ctx,
        200,
        cached,
        service === 'git-upload-pack' ? UPLOAD_PACK_AD : RECEIVE_PACK_AD,
      );
    } else {
      const body = v0Advertisement(repo, service);
      cacheGitResponse(repo, cacheKey, body);
      sendBuffer(
        ctx,
        200,
        body,
        service === 'git-upload-pack' ? UPLOAD_PACK_AD : RECEIVE_PACK_AD,
      );
    }
  }
  return true;
}

interface LsRefsArgs {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
}

function handleLsRefs(ctx: Ctx, repo: Repo, args: LsRefsArgs): boolean {
  const cacheKey =
    `ls-refs|${args.peel ? 1 : 0}${args.symrefs ? 1 : 0}${args.unborn ? 1 : 0}|` +
    args.prefixes.join(',');
  const cached = repo.gitCache.get(cacheKey);
  if (cached !== undefined) {
    sendBuffer(ctx, 200, cached, UPLOAD_PACK_RESULT);
    return true;
  }
  const lines: Buffer[] = [];
  const refs = refLines(repo);
  const matches = (name: string): boolean =>
    args.prefixes.length === 0 || args.prefixes.some((p) => name.startsWith(p));
  if (refs.length === 0) {
    if (args.unborn && matches('HEAD')) {
      lines.push(pktLine(`unborn HEAD symref-target:refs/heads/${DEFAULT_BRANCH}\n`));
    }
  } else {
    const tip = headTip(repo);
    if (matches('HEAD')) {
      const sym = args.symrefs ? ` symref-target:refs/heads/${DEFAULT_BRANCH}` : '';
      lines.push(pktLine(`${tip} HEAD${sym}\n`));
    }
    for (const ref of refs) {
      if (!matches(ref.name)) continue;
      const peeled = args.peel && ref.peeled ? ` peeled:${ref.peeled}` : '';
      lines.push(pktLine(`${ref.oid} ${ref.name}${peeled}\n`));
    }
  }
  lines.push(PKT_FLUSH);
  const body = Buffer.concat(lines);
  cacheGitResponse(repo, cacheKey, body);
  sendBuffer(ctx, 200, body, UPLOAD_PACK_RESULT);
  return true;
}

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth?: number;
}

function handleFetch(ctx: Ctx, repo: Repo, args: FetchArgs): boolean {
  // The whole response is a pure function of (repo graph, wants, haves,
  // shallow boundary, depth): 50 concurrent shallow clones of one tip ask
  // for byte-identical bytes, so serve the cached body when it exists.
  const cacheKey =
    `fetch|${args.wants.join(',')}|${args.haves.join(',')}|${args.shallow.join(',')}|` +
    (args.depth ?? '');
  const cached = repo.gitCache.get(cacheKey);
  if (cached !== undefined) {
    sendBuffer(ctx, 200, cached, UPLOAD_PACK_RESULT);
    return true;
  }
  // A want that is not in the store is a protocol error, reported on band 3.
  const missing = args.wants.find((want) => !repo.objects.has(want));
  const sections: Buffer[] = [];
  if (missing) {
    sections.push(pktLine('acknowledgments\n'), pktLine('NAK\n'), Buffer.from('0001', 'ascii'));
    sections.push(pktLine('packfile\n'));
    sections.push(
      sidebandPacket(3, Buffer.from(`ERR unknown object ${missing}\n`, 'utf8')),
      PKT_FLUSH,
    );
    const body = Buffer.concat(sections);
    cacheGitResponse(repo, cacheKey, body);
    sendBuffer(ctx, 200, body, UPLOAD_PACK_RESULT);
    return true;
  }
  const held = args.haves.filter((have) => repo.objects.has(have));
  const { toSend, shallow, unshallow } = computeFetch(
    repo.objects,
    args.wants,
    held,
    args.shallow.filter((s) => repo.objects.has(s)),
    args.depth,
  );
  const ackLines: Buffer[] = [pktLine('acknowledgments\n')];
  if (held.length === 0) {
    ackLines.push(pktLine('NAK\n'));
  } else {
    for (const have of held) ackLines.push(pktLine(`ACK ${have}\n`));
    ackLines.push(pktLine('ready\n'));
  }
  sections.push(...ackLines, Buffer.from('0001', 'ascii'));
  if (shallow.length > 0 || unshallow.length > 0) {
    sections.push(pktLine('shallow-info\n'));
    for (const oid of shallow) sections.push(pktLine(`shallow ${oid}\n`));
    for (const oid of unshallow) sections.push(pktLine(`unshallow ${oid}\n`));
    sections.push(Buffer.from('0001', 'ascii'));
  }
  sections.push(pktLine('packfile\n'));
  const pack = toSend.size > 0 ? writePack([...toSend.values()]) : writeEmptyPack();
  // Chunk the pack into sideband packets (each pkt-line <= 65520 bytes).
  const CHUNK = 65000;
  for (let offset = 0; offset < pack.length; offset += CHUNK) {
    sections.push(sidebandPacket(1, pack.subarray(offset, Math.min(pack.length, offset + CHUNK))));
  }
  sections.push(PKT_FLUSH);
  const body = Buffer.concat(sections);
  cacheGitResponse(repo, cacheKey, body);
  sendBuffer(ctx, 200, body, UPLOAD_PACK_RESULT);
  return true;
}

/** Parse a v2 upload-pack request body into ls-refs / fetch arguments. */
function parseV2UploadPack(body: Buffer): { kind: 'ls-refs'; args: LsRefsArgs } | { kind: 'fetch'; args: FetchArgs } | null {
  let items: PktItem[];
  try {
    items = decodePktLines(body);
  } catch {
    return null;
  }
  let command: string | null = null;
  let inArgs = false;
  const argLines: string[] = [];
  for (const item of items) {
    if (item.kind === 'flush') break;
    if (item.kind === 'delim') {
      inArgs = true;
      continue;
    }
    if (item.kind !== 'data') continue;
    const line = item.payload.toString('utf8');
    if (!inArgs) {
      if (line.startsWith('command=')) command = line.slice('command='.length).replace(/\n$/, '');
    } else {
      argLines.push(line.replace(/\n$/, ''));
    }
  }
  if (command === 'ls-refs') {
    const args: LsRefsArgs = { peel: false, symrefs: false, unborn: false, prefixes: [] };
    for (const line of argLines) {
      if (line === 'peel') args.peel = true;
      else if (line === 'symrefs') args.symrefs = true;
      else if (line === 'unborn') args.unborn = true;
      else if (line.startsWith('ref-prefix ')) args.prefixes.push(line.slice('ref-prefix '.length));
    }
    return { kind: 'ls-refs', args };
  }
  if (command === 'fetch') {
    const args: FetchArgs = { wants: [], haves: [], shallow: [] };
    for (const line of argLines) {
      if (line.startsWith('want ')) args.wants.push(line.slice(5).trim());
      else if (line.startsWith('have ')) args.haves.push(line.slice(5).trim());
      else if (line.startsWith('shallow ')) args.shallow.push(line.slice(8).trim());
      else if (line.startsWith('deepen ')) {
        const n = Number.parseInt(line.slice(7).trim(), 10);
        if (Number.isFinite(n) && n > 0) args.depth = n;
      }
    }
    return { kind: 'fetch', args };
  }
  return null;
}

function handleUploadPack(ctx: Ctx, repo: Repo): boolean {
  const parsed = parseV2UploadPack(ctx.body);
  if (!parsed) {
    sendError(ctx, 400, 'malformed upload-pack request');
    return true;
  }
  if (parsed.kind === 'ls-refs') return handleLsRefs(ctx, repo, parsed.args);
  return handleFetch(ctx, repo, parsed.args);
}

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

function handleReceivePack(ctx: Ctx, repo: Repo): boolean {
  // Split the body: command pkt-lines up to the first flush, then the pack.
  let flushEnd = -1;
  let offset = 0;
  const commands: PushCommand[] = [];
  let malformed = false;
  try {
    while (offset < ctx.body.length) {
      if (offset + 4 > ctx.body.length) throw new Error('truncated');
      const len = Number.parseInt(ctx.body.subarray(offset, offset + 4).toString('ascii'), 16);
      if (Number.isNaN(len)) throw new Error('bad length');
      if (len === 0) {
        flushEnd = offset + 4;
        break;
      }
      if (len < 4 || offset + len > ctx.body.length) throw new Error('truncated');
      const payload = ctx.body.subarray(offset + 4, offset + len).toString('utf8');
      const nul = payload.indexOf('\0');
      const line = (nul >= 0 ? payload.slice(0, nul) : payload).replace(/\n$/, '');
      const parts = line.split(' ');
      if (parts.length < 3) throw new Error('bad command line');
      commands.push({ oldOid: parts[0], newOid: parts[1], ref: parts.slice(2).join(' ') });
      offset += len;
    }
  } catch {
    malformed = true;
  }
  const report = (unpackOk: boolean, unpackDetail: string, results: Array<{ ref: string; ok: boolean; reason: string }>): Buffer => {
    const inner: Buffer[] = [pktLine(`unpack ${unpackDetail}\n`)];
    for (const result of results) {
      inner.push(
        result.ok
          ? pktLine(`ok ${result.ref}\n`)
          : pktLine(`ng ${result.ref} ${result.reason}\n`),
      );
    }
    return Buffer.concat([sidebandPacket(1, Buffer.concat(inner)), PKT_FLUSH]);
  };
  if (malformed || flushEnd < 0 || commands.length === 0) {
    sendBuffer(ctx, 200, report(false, 'malformed request', commands.map((c) => ({ ref: c.ref, ok: false, reason: 'malformed request' }))), RECEIVE_PACK_RESULT);
    return true;
  }
  const updatesRef = commands.some((c) => c.newOid !== ZERO_OID);
  const packBytes = ctx.body.subarray(flushEnd);
  let unpacked = new Map<string, { type: 'commit' | 'tree' | 'blob' | 'tag'; data: Buffer }>();
  let unpackError: string | null = null;
  if (updatesRef) {
    if (packBytes.length === 0) {
      unpackError = 'missing pack';
    } else {
      try {
        unpacked = readPack(packBytes).objects;
      } catch (err) {
        unpackError = err instanceof Error ? err.message : 'unpack error';
      }
    }
  }
  if (unpackError) {
    sendBuffer(
      ctx,
      200,
      report(false, unpackError, commands.map((c) => ({ ref: c.ref, ok: false, reason: 'unpacker error' }))),
      RECEIVE_PACK_RESULT,
    );
    return true;
  }
  // Unpack first: objects land in the store even when a ref is refused.
  for (const [oid, obj] of unpacked) repo.objects.set(oid, obj);
  const results: Array<{ ref: string; ok: boolean; reason: string }> = [];
  for (const command of commands) {
    const current = repo.refs.get(command.ref);
    if (command.newOid === ZERO_OID) {
      // Delete.
      if (current === undefined) {
        results.push({ ref: command.ref, ok: false, reason: 'reference does not exist' });
      } else if (command.oldOid !== ZERO_OID && command.oldOid !== current) {
        results.push({ ref: command.ref, ok: false, reason: 'stale old value' });
      } else {
        repo.refs.delete(command.ref);
        results.push({ ref: command.ref, ok: true, reason: '' });
      }
      continue;
    }
    const newObj = repo.objects.get(command.newOid);
    if (!newObj || (newObj.type !== 'commit' && newObj.type !== 'tag')) {
      results.push({ ref: command.ref, ok: false, reason: 'object not found' });
      continue;
    }
    if (current === undefined) {
      if (command.oldOid !== ZERO_OID) {
        results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
        continue;
      }
      repo.refs.set(command.ref, command.newOid);
      results.push({ ref: command.ref, ok: true, reason: '' });
      continue;
    }
    if (command.oldOid !== current) {
      results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    const newCommit = peelToCommit(repo.objects, command.newOid);
    const oldCommit = peelToCommit(repo.objects, current);
    if (!newCommit || !oldCommit || !isAncestor(repo.objects, oldCommit, newCommit)) {
      results.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    repo.refs.set(command.ref, command.newOid);
    results.push({ ref: command.ref, ok: true, reason: '' });
  }
  // The unpacked objects landed in the store even when a ref was refused, so
  // the graph moved either way: drop every cached git response for this repo.
  touchGit(repo);
  sendBuffer(ctx, 200, report(true, 'ok', results), RECEIVE_PACK_RESULT);
  return true;
}

/** Route git smart-HTTP paths. Returns true when the path was a git path. */
export function handleGit(ctx: Ctx): boolean {
  const match = ctx.pathname.match(/^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/);
  if (!match) return false;
  const [, owner, repoName, service] = match;
  if (owner !== VIEWER_LOGIN) {
    sendError(ctx, 404, 'Not Found');
    return true;
  }
  const repo = store.getRepo(owner, repoName);
  if (service === 'info/refs') return handleInfoRefs(ctx, repo);
  if (!repo) {
    sendError(ctx, 404, 'Not Found');
    return true;
  }
  if (service === 'git-upload-pack') return handleUploadPack(ctx, repo);
  return handleReceivePack(ctx, repo);
}
