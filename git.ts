// Pure git primitives: object ids, object construction/parsing, packfiles,
// pkt-lines and reachability. No store dependency; no I/O. All functions are
// deterministic (deflate is deterministic for a fixed zlib version and level).
import { createHash } from 'node:crypto';
import { inflateSync as zlibInflate, deflateSync as zlibDeflate } from 'node:zlib';

export const ZERO_OID = '0000000000000000000000000000000000000000';

export type ObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  type: ObjectType;
  data: Buffer;
}

const TYPE_CODE: Record<ObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, ObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

/** Git object id: SHA-1 over "<type> <size>\0<bytes>". */
export function objectId(type: ObjectType, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return sha1Hex(Buffer.concat([header, data]));
}

export function makeObject(type: ObjectType, data: Buffer): { oid: string; object: GitObject } {
  return { oid: objectId(type, data), object: { type, data } };
}

export interface TreeEntry {
  mode: string; // e.g. '100644', '40000'
  name: string;
  oid: string;
  isDir: boolean;
}

/** Git tree entry sort: directory names compare as if they ended in '/'. */
function treeSortKey(entry: TreeEntry): string {
  return entry.isDir ? `${entry.name}/` : entry.name;
}

export function buildTree(entries: TreeEntry[]): { oid: string; object: GitObject } {
  const sorted = [...entries].sort((a, b) =>
    Buffer.compare(Buffer.from(treeSortKey(a)), Buffer.from(treeSortKey(b))),
  );
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return makeObject('tree', Buffer.concat(parts));
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    const nul = data.indexOf(0, space);
    const mode = data.subarray(offset, space).toString('utf8');
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid, isDir: mode === '40000' });
    offset = nul + 21;
  }
  return entries;
}

export interface CommitInfo {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function buildCommit(
  tree: string,
  parents: string[],
  message: string,
  author: string,
  committer: string,
): { oid: string; object: GitObject } {
  const lines: string[] = [`tree ${tree}`];
  for (const parent of parents) lines.push(`parent ${parent}`);
  lines.push(`author ${author}`, `committer ${committer}`, '', message);
  return makeObject('commit', Buffer.from(lines.join('\n'), 'utf8'));
}

export function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const headEnd = text.indexOf('\n\n');
  const head = headEnd >= 0 ? text.slice(0, headEnd) : text;
  const message = headEnd >= 0 ? text.slice(headEnd + 2) : '';
  const info: CommitInfo = { tree: '', parents: [], author: '', committer: '', message };
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) info.tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) info.parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) info.author = line.slice(7);
    else if (line.startsWith('committer ')) info.committer = line.slice(10);
  }
  return info;
}

export interface TagInfo {
  object: string;
  type: ObjectType;
  tag: string;
  tagger: string;
  message: string;
}

export function buildTag(
  object: string,
  type: ObjectType,
  tag: string,
  tagger: string,
  message: string,
): { oid: string; object: GitObject } {
  const lines = [`object ${object}`, `type ${type}`, `tag ${tag}`, `tagger ${tagger}`, '', message];
  return makeObject('tag', Buffer.from(lines.join('\n'), 'utf8'));
}

export function parseTag(data: Buffer): TagInfo {
  const text = data.toString('utf8');
  const headEnd = text.indexOf('\n\n');
  const head = headEnd >= 0 ? text.slice(0, headEnd) : text;
  const message = headEnd >= 0 ? text.slice(headEnd + 2) : '';
  const info: TagInfo = { object: '', type: 'commit', tag: '', tagger: '', message };
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) info.object = line.slice(7).trim();
    else if (line.startsWith('type ')) info.type = line.slice(5).trim() as ObjectType;
    else if (line.startsWith('tag ')) info.tag = line.slice(4);
    else if (line.startsWith('tagger ')) info.tagger = line.slice(7);
  }
  return info;
}

// ---------------------------------------------------------------------------
// pkt-lines
// ---------------------------------------------------------------------------

export type PktItem =
  | { kind: 'data'; payload: Buffer }
  | { kind: 'flush' }
  | { kind: 'delim' }
  | { kind: 'end' };

/** Encode one pkt-line: 4 hex digits of (payload length + 4), then the payload. */
export function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const header = Buffer.from((body.length + 4).toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([header, body]);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');
export const PKT_END = Buffer.from('0002', 'ascii');

/** Decode a whole buffer of pkt-lines. Throws on malformed framing. */
export function decodePktLines(buf: Buffer): PktItem[] {
  const items: PktItem[] = [];
  let offset = 0;
  while (offset < buf.length) {
    if (offset + 4 > buf.length) throw new Error('truncated pkt-line header');
    const lenHex = buf.subarray(offset, offset + 4).toString('ascii');
    const len = Number.parseInt(lenHex, 16);
    if (Number.isNaN(len)) throw new Error(`bad pkt-line length ${JSON.stringify(lenHex)}`);
    if (len === 0) {
      items.push({ kind: 'flush' });
      offset += 4;
    } else if (len === 1) {
      items.push({ kind: 'delim' });
      offset += 4;
    } else if (len === 2) {
      items.push({ kind: 'end' });
      offset += 4;
    } else {
      if (offset + len > buf.length) throw new Error('truncated pkt-line payload');
      items.push({ kind: 'data', payload: buf.subarray(offset + 4, offset + len) });
      offset += len;
    }
  }
  return items;
}

/** A sideband packet: band byte + payload, framed as one pkt-line (<= 65520). */
export function sidebandPacket(band: 1 | 2 | 3, payload: Buffer): Buffer {
  const body = Buffer.concat([Buffer.from([band]), payload]);
  return pktLine(body);
}

// ---------------------------------------------------------------------------
// Packfiles (plain objects only; the suite never sends or needs deltas)
// ---------------------------------------------------------------------------

/** Encode one object entry: type/size varint + zlib stream. */
function packObjectEntry(type: ObjectType, data: Buffer): Buffer {
  const code = TYPE_CODE[type];
  const compressed = zlibDeflate(data);
  // varint: first byte holds type (3 bits) + low 4 size bits + continuation bit.
  const sizeBytes: number[] = [];
  let size = data.length;
  let first = (code << 4) | (size & 0x0f);
  size >>= 4;
  if (size > 0) first |= 0x80;
  sizeBytes.push(first);
  while (size > 0) {
    let byte = size & 0x7f;
    size >>= 7;
    if (size > 0) byte |= 0x80;
    sizeBytes.push(byte);
  }
  return Buffer.concat([Buffer.from(sizeBytes), compressed]);
}

/** Write a pack: PACK, version 2, count, entries, 20-byte SHA-1 trailer. */
export function writePack(objects: GitObject[]): Buffer {
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  const entries = objects.map((obj) => packObjectEntry(obj.type, obj.data));
  const body = Buffer.concat([header, ...entries]);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

/** An empty pack: header + count 0 + trailer (32 bytes). */
export function writeEmptyPack(): Buffer {
  return writePack([]);
}

export interface PackReadResult {
  objects: Map<string, GitObject>;
  deltaCount: number;
}

/**
 * Read a pack, verifying the trailer checksum and that every object recomputes
 * to its id. Delta entries are rejected (the suite never sends them).
 */
export function readPack(buf: Buffer): PackReadResult {
  if (buf.length < 32) throw new Error('pack too short');
  if (buf.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('bad pack magic');
  if (buf.readUInt32BE(4) !== 2) throw new Error('unsupported pack version');
  const count = buf.readUInt32BE(8);
  const trailerStart = buf.length - 20;
  const expected = buf.subarray(trailerStart).toString('hex');
  const actual = sha1Hex(buf.subarray(0, trailerStart));
  if (expected !== actual) throw new Error('pack trailer checksum mismatch');
  const objects = new Map<string, GitObject>();
  let offset = 12;
  let deltaCount = 0;
  for (let i = 0; i < count; i += 1) {
    // varint header: first byte holds type (3 bits) + low 4 size bits.
    let byte = buf[offset];
    offset += 1;
    const typeCode = (byte >> 4) & 0x07;
    let size = byte & 0x0f;
    let shift = 4;
    while (byte & 0x80) {
      byte = buf[offset];
      offset += 1;
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    if (typeCode === 6 || typeCode === 7) {
      throw new Error('delta pack entries are not supported');
    }
    const type = CODE_TYPE[typeCode];
    if (!type) throw new Error(`unknown pack object type ${typeCode}`);
    // inflateSync ignores trailing bytes, so inflating the whole remainder
    // yields this object's data; the exact consumed length comes from the
    // minimal-prefix search below.
    const data = zlibInflate(buf.subarray(offset, trailerStart));
    if (data.length !== size) throw new Error('inflate size mismatch');
    offset += findZlibEnd(buf, offset, trailerStart - offset);
    const oid = objectId(type, data);
    objects.set(oid, { type, data });
  }
  if (offset !== trailerStart) throw new Error('pack has trailing bytes before the trailer');
  if (objects.size !== count) throw new Error('pack object count mismatch');
  return { objects, deltaCount };
}

/**
 * Find how many bytes a zlib stream starting at `offset` occupies, by
 * inflating progressively longer prefixes until the output length matches
 * the stream's own end. Uses the raw deflate block end: inflateSync throws
 * on a truncated stream, so binary-search the smallest prefix that inflates
 * cleanly to a full stream.
 */
function findZlibEnd(buf: Buffer, offset: number, maxLen: number): number {
  // The stream must end at or before offset+maxLen. Binary search the
  // smallest prefix that inflates without error.
  let lo = 1;
  let hi = maxLen;
  // First, the guess that worked in readPack is `guess`; the end is within it.
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    try {
      zlibInflate(buf.subarray(offset, offset + mid));
      hi = mid;
    } catch {
      lo = mid + 1;
    }
  }
  return lo;
}

// ---------------------------------------------------------------------------
// Reachability over a repo's object map
// ---------------------------------------------------------------------------

export type ObjectMap = Map<string, GitObject>;

/** Peel a tag (recursively) to the commit it points at. */
export function peelToCommit(objects: ObjectMap, oid: string): string | null {
  let current = oid;
  for (let i = 0; i < 10; i += 1) {
    const obj = objects.get(current);
    if (!obj) return null;
    if (obj.type === 'commit') return current;
    if (obj.type === 'tag') {
      current = parseTag(obj.data).object;
      continue;
    }
    return null;
  }
  return null;
}

/**
 * All commits reachable from `tips` (following parents), in BFS order.
 * With maxDepth, only generations < maxDepth are included (depth 1 = tips only).
 */
export function reachableCommits(
  objects: ObjectMap,
  tips: string[],
  maxDepth?: number,
): Map<string, number> {
  const seen = new Map<string, number>();
  const queue: Array<{ oid: string; depth: number }> = [];
  for (const tip of tips) {
    const commit = peelToCommit(objects, tip);
    if (commit && !seen.has(commit)) {
      seen.set(commit, 0);
      queue.push({ oid: commit, depth: 0 });
    }
  }
  while (queue.length) {
    const { oid, depth } = queue.shift()!;
    if (maxDepth !== undefined && depth + 1 >= maxDepth) continue;
    const obj = objects.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) {
      if (!seen.has(parent)) {
        seen.set(parent, depth + 1);
        queue.push({ oid: parent, depth: depth + 1 });
      }
    }
  }
  return seen;
}

/** Full closure (trees + blobs) of a set of commits. */
export function closureOfCommits(objects: ObjectMap, commits: Iterable<string>): Set<string> {
  const out = new Set<string>();
  const stack: string[] = [...commits];
  while (stack.length) {
    const oid = stack.pop()!;
    if (out.has(oid)) continue;
    const obj = objects.get(oid);
    if (!obj) continue;
    out.add(oid);
    if (obj.type === 'commit') {
      const info = parseCommit(obj.data);
      if (info.tree) stack.push(info.tree);
      for (const parent of info.parents) stack.push(parent);
    } else if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) stack.push(entry.oid);
    } else if (obj.type === 'tag') {
      stack.push(parseTag(obj.data).object);
    }
  }
  return out;
}

/**
 * The objects of one commit without following parents: the commit, its tree
 * (recursively) and its blobs. Used for fetch object sets, where parents are
 * only included when they are themselves in the to-send set.
 */
export function treeClosure(objects: ObjectMap, commitOid: string): Set<string> {
  const out = new Set<string>();
  const obj = objects.get(commitOid);
  if (!obj || obj.type !== 'commit') return out;
  out.add(commitOid);
  const stack: string[] = [parseCommit(obj.data).tree];
  while (stack.length) {
    const oid = stack.pop()!;
    if (!oid || out.has(oid)) continue;
    const entry = objects.get(oid);
    if (!entry) continue;
    out.add(oid);
    if (entry.type === 'tree') {
      for (const child of parseTree(entry.data)) stack.push(child.oid);
    }
  }
  return out;
}

/** True when `ancestor` is reachable from `tip` (or equal). */
export function isAncestor(objects: ObjectMap, ancestor: string, tip: string): boolean {
  if (ancestor === tip) return true;
  const seen = new Set<string>([tip]);
  const queue = [tip];
  while (queue.length) {
    const oid = queue.shift()!;
    const obj = objects.get(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) {
      if (parent === ancestor) return true;
      if (!seen.has(parent)) {
        seen.add(parent);
        queue.push(parent);
      }
    }
  }
  return false;
}

/**
 * Compute the object set for a v2 fetch.
 * - wants: oids the client wants (commits or tags).
 * - haves: oids the client already holds.
 * - shallowHaves: haves the client holds as shallow (their ancestry is unknown
 *   to the client, so they do not exclude ancestors).
 * - depth: when set, only generations < depth from the wants are included.
 * Returns the objects to send, the new shallow boundary commits, and the
 * commits that stop being shallow (their parents were just delivered).
 */
export function computeFetch(
  objects: ObjectMap,
  wants: string[],
  haves: string[],
  shallowHaves: string[],
  depth?: number,
): { toSend: Map<string, GitObject>; shallow: string[]; unshallow: string[] } {
  const shallowHaveSet = new Set(shallowHaves);
  // Commits the client already holds in full (haves minus shallow haves).
  const heldFull = new Set<string>();
  for (const have of haves) {
    if (shallowHaveSet.has(have)) continue;
    for (const commit of reachableCommits(objects, [have]).keys()) heldFull.add(commit);
  }
  // Shallow haves themselves are held (but not their ancestors).
  const heldShallow = new Set<string>();
  for (const have of shallowHaves) {
    const commit = peelToCommit(objects, have);
    if (commit) heldShallow.add(commit);
  }
  // Commits within depth of the wants.
  const wantCommits = reachableCommits(objects, wants, depth);
  const toSendCommits = new Set<string>();
  for (const commit of wantCommits.keys()) {
    if (!heldFull.has(commit) && !heldShallow.has(commit)) toSendCommits.add(commit);
  }
  // Boundary: the deepest included commits (at depth-1 generations) whose
  // parents were cut off by the depth limit. With no depth limit there is no
  // boundary (the whole history is delivered).
  const shallow: string[] = [];
  if (depth !== undefined) {
    for (const [commit, gen] of wantCommits) {
      if (gen === depth - 1 && toSendCommits.has(commit)) shallow.push(commit);
    }
  }
  // Unshallow: shallow haves whose parents are now delivered (i.e. the have
  // is an ancestor of a delivered commit, or the have itself is within the
  // delivered range and its parents are included).
  const unshallow: string[] = [];
  if (depth !== undefined) {
    for (const have of shallowHaves) {
      const commit = peelToCommit(objects, have);
      if (!commit) continue;
      // The have stops being shallow when its parents are delivered: that is
      // when the have is itself sent (it was not held) or when a delivered
      // commit is a child of the have... precisely: the have's parents are in
      // the delivered set. Parents are delivered when the have is within the
      // depth window (gen < depth-1) and not held... Simpler exact rule: the
      // have is unshallowed iff it is an ancestor of a to-send commit (its
      // parent chain is now known) OR it is itself in toSendCommits and its
      // parents are also delivered.
      const parentsDelivered = (() => {
        const obj = objects.get(commit);
        if (!obj || obj.type !== 'commit') return false;
        const parents = parseCommit(obj.data).parents;
        if (parents.length === 0) return true; // root: nothing unknown
        return parents.every((p) => toSendCommits.has(p) || heldFull.has(p));
      })();
      const isAncestorOfSent = [...toSendCommits].some((c) => isAncestor(objects, commit, c));
      if (parentsDelivered && (isAncestorOfSent || toSendCommits.has(commit))) {
        unshallow.push(commit);
      }
    }
  }
  // Objects: the tree-closure of the to-send commits (parents are NOT followed;
  // a parent is only included when it is itself in the to-send set), minus
  // anything the client already holds (full closure of held commits, plus the
  // tree-closure of shallow-held commits).
  const toSend = new Map<string, GitObject>();
  const toSendOids = new Set<string>();
  for (const commit of toSendCommits) {
    for (const oid of treeClosure(objects, commit)) toSendOids.add(oid);
  }
  const heldObjects = closureOfCommits(objects, heldFull);
  for (const commit of heldShallow) {
    for (const oid of treeClosure(objects, commit)) heldObjects.add(oid);
  }
  for (const oid of toSendOids) {
    if (heldObjects.has(oid)) continue;
    const obj = objects.get(oid);
    if (obj) toSend.set(oid, obj);
  }
  // Tag wants themselves are always sent (the client wants the tag object).
  for (const want of wants) {
    const obj = objects.get(want);
    if (obj && obj.type === 'tag' && !heldFull.has(want)) toSend.set(want, obj);
  }
  // Non-commit wants (blob/tree) are sent directly.
  for (const want of wants) {
    const obj = objects.get(want);
    if (obj && (obj.type === 'blob' || obj.type === 'tree') && !heldObjects.has(want)) {
      toSend.set(want, obj);
    }
  }
  return { toSend, shallow, unshallow };
}
