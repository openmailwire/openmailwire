// A local HTTP server that records every request and answers from a
// handler, so the SDK is tested without any real network service.
import http from "node:http";

/**
 * @param {(req: {method: string, path: string, query: URLSearchParams, headers: http.IncomingHttpHeaders, body: string, json: any}) =>
 *   {status?: number, body?: unknown, headers?: Record<string, string>} | undefined} handler
 */
export async function startMock(handler) {
  const requests = [];
  const server = http.createServer(async (req, res) => {
    const chunks = [];
    for await (const c of req) chunks.push(c);
    const body = Buffer.concat(chunks).toString("utf8");
    const url = new URL(req.url, "http://mock");
    let json;
    try {
      json = body ? JSON.parse(body) : undefined;
    } catch {
      json = undefined;
    }
    const rec = { method: req.method, path: url.pathname, query: url.searchParams, headers: req.headers, body, json };
    requests.push(rec);
    const out = (await handler(rec)) ?? {
      status: 404,
      body: { statusCode: 404, name: "not_found", message: "no such endpoint" },
    };
    const isRaw = typeof out.body === "string" || out.body instanceof Uint8Array;
    res.writeHead(out.status ?? 200, {
      "content-type": isRaw ? "application/octet-stream" : "application/json",
      ...out.headers,
    });
    res.end(isRaw ? out.body : JSON.stringify(out.body ?? {}));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();
  return {
    url: `http://127.0.0.1:${port}`,
    requests,
    close: () =>
      new Promise((resolve) => {
        server.closeAllConnections();
        server.close(resolve);
      }),
  };
}
