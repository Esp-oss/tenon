// 测试用假宿主：起一个**真实**的 node:http 服务，按脚本回包，并记录收到的每一次请求。
//
// 用真 HTTP 而不是打桩 fetch，是因为本轮要断言的正是「线上字节」：头有没有带上
// （`Tenon-Version` / `Idempotency-Key`）、请求体是不是那份规范 JSON、审计两段是否共用同一个
// `auditId`。打桩 fetch 会把这些都让给被测代码自己解释，测不出东西。
import { createServer, type Server } from 'node:http';

/** 一次被记录下来的请求。`body` 是原始文本（摘要断言要的正是它）。 */
export interface FakeRequest {
  method: string;
  path: string;
  headers: Record<string, string | string[] | undefined>;
  body: string;
}

/** 脚本化的一次回包。`body` 是对象时按 JSON 序列化，是字符串时原样发出。 */
export interface FakeResponse {
  status: number;
  body: unknown;
}

/** 回包脚本。返回 undefined 表示「这个请求不该发生」，会回 500 让用例自己暴露出来。 */
export type FakeHandler = (request: FakeRequest, origin: string) => FakeResponse | undefined;

export interface FakeHost {
  /** 服务监听地址（`http://127.0.0.1:<port>`），模块的 baseURL 也指向它。 */
  origin: string;
  requests: FakeRequest[];
  /** 请求序列，形如 `POST /tenon/audit`。 */
  sequence(): string[];
  /** 取某个路径下的全部请求体（已解析）。 */
  bodies(path: string): unknown[];
  close(): Promise<void>;
}

/**
 * 起一个假宿主。
 * @param handler 回包脚本；可用第二参数 `origin` 让 `GET /tenon/modules` 指向自己。
 * @returns 已监听的假宿主。
 */
export async function startFakeHost(handler: FakeHandler): Promise<FakeHost> {
  const requests: FakeRequest[] = [];
  // 先声明后赋值：回包脚本需要知道「自己是谁」（模块的 baseURL 指向本服务），而端口在
  // listen 之后才有。请求只会在 listen 之后到达，因此脚本读到的一定是已赋值的值。
  let origin = '';
  const server: Server = createServer((incoming, response) => {
    const chunks: Buffer[] = [];
    incoming.on('data', (chunk: Buffer) => chunks.push(chunk));
    incoming.on('end', () => {
      const request: FakeRequest = {
        method: incoming.method ?? '',
        path: incoming.url ?? '',
        headers: incoming.headers,
        body: Buffer.concat(chunks).toString('utf8'),
      };
      requests.push(request);
      const reply = handler(request, origin) ?? {
        status: 500,
        body: {
          tenonError: {
            class: 'contract_violation',
            code: 'UNSTUBBED_REQUEST',
            message: `用例没有为 ${request.method} ${request.path} 准备回包`,
            retryable: false,
          },
        },
      };
      const text =
        typeof reply.body === 'string' ? reply.body : JSON.stringify(reply.body ?? null);
      response.writeHead(reply.status, { 'content-type': 'application/json; charset=utf-8' });
      response.end(text);
    });
  });

  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('假宿主没有拿到监听端口');
  }
  origin = `http://127.0.0.1:${address.port}`;

  return {
    origin,
    requests,
    sequence: () => requests.map((request) => `${request.method} ${request.path}`),
    bodies: (path: string) =>
      requests.filter((request) => request.path === path).map((request) => JSON.parse(request.body)),
    close: () =>
      new Promise<void>((resolve, reject) => {
        // 先掐掉 keep-alive 连接，否则 server.close 会等连接自然超时，用例白等好几秒。
        server.closeAllConnections();
        server.close((error) => (error ? reject(error) : resolve()));
      }),
  };
}
