// Shared HTTP plumbing: the request context, body reading, JSON replies.
// Every response carries a non-empty X-GitHub-Request-Id (the REST protocol
// checks require it on every route, including errors and 404s).
import type { IncomingMessage, ServerResponse } from 'node:http';

export interface Ctx {
  req: IncomingMessage;
  res: ServerResponse;
  method: string;
  pathname: string;
  query: URLSearchParams;
  body: Buffer;
  requestId: string;
}

let requestCounter = 0;
export function nextRequestId(): string {
  requestCounter += 1;
  return `req-${requestCounter}`;
}

export function readBody(req: IncomingMessage, maxBytes = 64 * 1024 * 1024): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > maxBytes) {
        reject(new Error('request body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

export function sendBuffer(
  ctx: Ctx,
  status: number,
  body: Buffer,
  contentType: string,
  extra?: Record<string, string>,
): void {
  ctx.res.setHeader('X-GitHub-Request-Id', ctx.requestId);
  ctx.res.setHeader('Content-Type', contentType);
  if (extra) for (const [key, value] of Object.entries(extra)) ctx.res.setHeader(key, value);
  ctx.res.writeHead(status);
  ctx.res.end(body);
}

export function sendJson(
  ctx: Ctx,
  status: number,
  obj: unknown,
  extra?: Record<string, string>,
): void {
  const body = typeof obj === 'string' ? obj : JSON.stringify(obj);
  sendBuffer(ctx, status, Buffer.from(body, 'utf8'), 'application/json; charset=utf-8', extra);
}

/** 204 No Content, still carrying the request id. */
export function sendNoContent(ctx: Ctx): void {
  ctx.res.setHeader('X-GitHub-Request-Id', ctx.requestId);
  ctx.res.writeHead(204);
  ctx.res.end();
}

export function sendError(ctx: Ctx, status: number, message: string): void {
  sendJson(ctx, status, { message });
}

/**
 * Parse a JSON request body. An empty body counts as `{}`; malformed JSON
 * returns null (the caller answers 400).
 */
export function parseJsonBody(ctx: Ctx): unknown | null {
  if (ctx.body.length === 0) return {};
  try {
    return JSON.parse(ctx.body.toString('utf8'));
  } catch {
    return null;
  }
}

export function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
