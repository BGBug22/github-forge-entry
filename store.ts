// In-memory store for the forge.
//
// The scenario (viewer login, teammate, stranger, org, repo names) is fixed by the
// kit's sdk/conformance/wire.ts buildScenario(), which derives it from the constant
// SCENARIO_SEED = 0x6a09e667f3bcc909n. The sealed env ships the same file, so these
// names are identical in every trial. The viewer login is therefore hardcoded here
// with a citation instead of recomputed; the shop token is opaque (shpat_<32 hex>)
// and never contains the login.
//
// Determinism: nothing in this store uses wall-clock time, randomness or the port.
// Timestamps come from a monotone counter (seconds since 2025-01-01T00:00:00Z), so
// two runs fed the same requests produce the same state.
import type { GitObject } from './git.ts';

export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
export const TEAMMATE_LOGIN = 'quinnkeller';
export const STRANGER_LOGIN = 'olgabauer';
export const ORG_LOGIN = 'faint-meadow-org';
export const DEFAULT_BRANCH = 'main';

// Epoch for deterministic timestamps: 2025-01-01T00:00:00Z.
export const EPOCH_SEC = 1735689600;

let seq = 0;
/** Next deterministic sequence number (also drives timestamps). */
export function nextSeq(): number {
  seq += 1;
  return seq;
}
/** ISO timestamp for a sequence number. */
export function isoTime(seqNum: number): string {
  return new Date((EPOCH_SEC + seqNum) * 1000).toISOString();
}

let globalIdCounter = 0;
/** Next store-wide numeric id (repos, issues, pulls, comments share one sequence). */
export function nextGlobalId(): number {
  globalIdCounter += 1;
  return globalIdCounter;
}

export interface Label {
  name: string;
  color: string;
}

export interface Comment {
  id: number;
  body: string;
  userLogin: string;
  createdSeq: number;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
  comments: Comment[];
  createdSeq: number;
  /** Cached serialised JSON for the hot read path; null after any mutation. */
  cachedJson: string | null;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  createdSeq: number;
}

export interface Repo {
  id: number;
  name: string;
  owner: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  createdSeq: number;
  /** Git objects by SHA-1. */
  objects: Map<string, GitObject>;
  /** Full ref name (refs/heads/...) -> commit/tag oid. */
  refs: Map<string, string>;
  labels: Map<string, Label>;
  issues: Issue[];
  pulls: Pull[];
  /** Shared issue+PR numbering counter (GitHub numbers issues and PRs together). */
  nextNumber: number;
  /** Cached serialised JSON for the hot read path; null after any mutation. */
  cachedJson: string | null;
  /**
   * Monotone revision of the git object/ref graph; bumped by touchGit() on
   * every mutation. Git smart-HTTP responses cached in gitCache are valid
   * only for the current revision (the cache is cleared on every bump).
   */
  gitRev: number;
  /** Cached git smart-HTTP response bodies, keyed by request; see touchGit(). */
  gitCache: Map<string, Buffer>;
}

export class Store {
  repos = new Map<string, Repo>();

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  createRepo(name: string, description: string, isPrivate: boolean): Repo | null {
    const key = this.repoKey(VIEWER_LOGIN, name);
    if (this.repos.has(key)) return null;
    const repo: Repo = {
      id: nextGlobalId(),
      name,
      owner: VIEWER_LOGIN,
      description,
      private: isPrivate,
      defaultBranch: DEFAULT_BRANCH,
      createdSeq: nextSeq(),
      objects: new Map(),
      refs: new Map(),
      labels: new Map(),
      issues: [],
      pulls: [],
      nextNumber: 1,
      cachedJson: null,
      gitRev: 0,
      gitCache: new Map(),
    };
    this.repos.set(key, repo);
    return repo;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.repoKey(owner, name));
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.repoKey(owner, name));
  }

  /** Repos owned by the viewer, in creation order. */
  listViewerRepos(): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) {
      if (repo.owner === VIEWER_LOGIN) out.push(repo);
    }
    out.sort((a, b) => a.createdSeq - b.createdSeq);
    return out;
  }

  /** Default-branch tip oid, or null when the repo is unborn. */
  defaultTip(repo: Repo): string | null {
    return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
  }
}

export const store = new Store();

/**
 * Invalidate cached git smart-HTTP responses after the repo's object/ref
 * graph changes. Call after every objects/refs mutation (receive-pack,
 * commitTree, git/refs create, merge). The cache is per-Repo object, so a
 * deleted-and-recreated repo starts empty anyway; the bump also lets a
 * reader tell at a glance whether the graph moved since a response was built.
 */
export function touchGit(repo: Repo): void {
  repo.gitRev += 1;
  if (repo.gitCache.size > 0) repo.gitCache.clear();
}

/** Opaque global id for GraphQL `node` round-trips: base64 of a kind tag + ids. */
export function globalId(kind: string, ...parts: (string | number)[]): string {
  return Buffer.from(`${kind}:${parts.join(':')}`, 'utf8').toString('base64');
}

/** Decode a global id produced by globalId(); null when it does not parse. */
export function parseGlobalId(id: string): { kind: string; parts: string[] } | null {
  let text: string;
  try {
    text = Buffer.from(id, 'base64').toString('utf8');
  } catch {
    return null;
  }
  const parts = text.split(':');
  if (parts.length < 2) return null;
  return { kind: parts[0], parts: parts.slice(1) };
}
