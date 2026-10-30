// 插件配置（Schemastery）。
//
// 身份字段的定位是本节最容易被误读的地方：`agentID` 与 `onBehalfOf` **只写进审计的 actor**，
// 不是鉴权凭据——M0.5 的 host 闸门端点没有 token（设计 §2），审计里的身份就是全部的身份事实。
// 因此这两项只能来自部署配置，**绝不从模型参数取**：工具面的 parameters 里没有、也不会有
// 这两个名字（见 index.ts 的 parameters 与它的单测）。
import z from '@deepseek-ai/schemastery';

/** 部署侧配置。由 profile 的插件配置提供，模型不可见、不可写。 */
export const Config = z.object({
  hostURL: z
    .string()
    .required()
    .description('Tenon host 根地址，形如 http://127.0.0.1:8094；不带尾部斜杠也可'),
  agentID: z
    .string()
    .required()
    .description('审计 actor.id（约定 ag_<name>）。只进审计，不是鉴权凭据'),
  onBehalfOf: z
    .string()
    .required()
    .description('代行委托人（约定 user:<账号>）。只进审计 actor.onBehalfOf，不从模型参数取'),
  timeoutMs: z
    .number()
    .min(100)
    .default(15000)
    .description('单次 HTTP 调用的超时毫秒数（含发现、策略、预演、审计与执行）'),
  operatorRoles: z
    .array(z.string())
    .default([])
    .description(
      '本会话操作员实际持有的角色（例 role:warehouse_manager）。**这是授权输入，不只是展示**：' +
        '与策略给出的 requiredRoles 无交集时，approve 一律停在待批、不会向任何人提问。' +
        '留空即「本会话谁都批不了」。DSH 不 attest 身份，因此它是部署声明，是 Q2 定下来之前的临时绑定',
    ),
  stateDir: z
    .string()
    .default('out/module-host-state')
    .description(
      '待批计划的落盘目录（`<stateDir>/pending/<planId>.json`）。默认与 Go agent 的 out/agent-state 分开：' +
        '两份实现不共用一份盘上状态，免得互相踩',
    ),
  pendingTTLMinutes: z
    .number()
    .min(1)
    .default(15)
    .description('待批计划的有效期（分钟）。过期后只写结局、绝不执行（与 Go 侧 15 分钟缺省一致）'),
});

/** 与 {@link Config} 同形的普通接口，供纯函数与单测直接构造。 */
export interface ModuleHostConfig {
  hostURL: string;
  agentID: string;
  onBehalfOf: string;
  timeoutMs?: number;
  /** 本会话操作员持有的角色（授权输入，见 {@link Config} 里的说明）。 */
  operatorRoles?: string[];
  /** 待批计划落盘目录。 */
  stateDir?: string;
  /** 待批有效期（分钟）。 */
  pendingTTLMinutes?: number;
}

/** 默认超时（`timeoutMs` 省略时的取值，与 Schemastery 的 default 保持一致）。 */
export const DEFAULT_TIMEOUT_MS = 15000;

/** 默认状态目录（与 Schemastery 的 default 一致）。 */
export const DEFAULT_STATE_DIR = 'out/module-host-state';

/** 默认待批有效期（分钟，与 Schemastery 的 default 一致）。 */
export const DEFAULT_PENDING_TTL_MINUTES = 15;
