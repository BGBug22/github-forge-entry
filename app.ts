// The HTTP server: routing, auth and the boot probe.
//
// - GET /healthz answers 200 {"ok":true} and takes no token (the env's boot
//   probe). Everything else requires Authorization: Bearer $MAKEFUN_SHOP_TOKEN
//   (missing or wrong -> 401 JSON with a message).
// - Dispatch order: git smart HTTP, MCP, GraphQL, REST v3, then a JSON 404.
// - A handler that throws answers 500 JSON; the server never crashes on a
//   bad request.
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { nextRequestId, readBody, sendJson, sendError, type Ctx } from './http-util.ts';
import { handleGit } from './git-http.ts';
import { handleRest } from './rest.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';

const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

function authorized(req: IncomingMessage): boolean {
  const header = req.headers.authorization ?? '';
  if (!header.startsWith('Bearer ')) return false;
  const token = header.slice('Bearer '.length);
  if (TOKEN === '') return token.length > 0;
  return token === TOKEN;
}

function needsAuth(pathname: string): boolean {
  return (
    pathname.startsWith('/api/') ||
    pathname === '/mcp' ||
    /\.git\//.test(pathname)
  );
}

export function createApp(): ReturnType<typeof createServer> {
  const server = createServer(async (req: IncomingMessage, res: ServerResponse) => {
    const requestId = nextRequestId();
    try {
      const url = new URL(req.url ?? '/', `http://${req.headers.host ?? '127.0.0.1'}`);
      const ctx: Ctx = {
        req,
        res,
        method: req.method ?? 'GET',
        pathname: url.pathname,
        query: url.searchParams,
        body: Buffer.alloc(0),
        requestId,
      };
      if (ctx.pathname === '/healthz') {
        sendJson(ctx, 200, { ok: true });
        return;
      }
      if (needsAuth(ctx.pathname) && !authorized(req)) {
        sendError(ctx, 401, 'Bad credentials');
        return;
      }
      ctx.body = await readBody(req);
      if (handleGit(ctx)) return;
      if (ctx.pathname === '/mcp' && handleMcp(ctx)) return;
      if (ctx.pathname === '/api/graphql' && handleGraphQL(ctx)) return;
      if (ctx.pathname.startsWith('/api/v3') && handleRest(ctx)) return;
      sendError(ctx, 404, 'Not Found');
    } catch (err) {
      // A body that exceeds the read cap is the client's fault (413); anything
      // else is a server fault (500). Either way the server keeps serving.
      const tooLarge = err instanceof Error && err.message === 'request body too large';
      try {
        res.setHeader('X-GitHub-Request-Id', requestId);
        res.setHeader('Content-Type', 'application/json; charset=utf-8');
        res.writeHead(tooLarge ? 413 : 500);
        res.end(JSON.stringify({ message: tooLarge ? 'request body too large' : err instanceof Error ? err.message : 'internal error' }));
      } catch {
        // The socket is already gone; nothing more to do.
      }
    }
  });
  return server;
}
