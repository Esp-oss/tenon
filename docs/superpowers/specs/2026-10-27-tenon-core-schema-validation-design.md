# tenon-core schema 校验：选型与设计

- 日期：2026-10-27
- 状态：待复核
- 关联：[ADR-0001](../../adr/0001-driver-dsh-vs-go.md) §6 第 1 步（`tenon-core` 真实现）；[`packages/tenon-core/README.md`](../../../packages/tenon-core/README.md)
- 本次要解决：M0 唯一未落地的部分——**JSON Schema 的运行时校验**（`src/types.ts` 已有类型骨架，其余工具已落地）

---

## 1. 背景（实测事实）

| 事实 | 数据 |
|---|---|
| 契约真源 | `spec/0.1/schemas/*.json`，10 份，draft **2020-12**，单份 3–14 KB |
| Go 侧校验器 | `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3`，报错形如 `at '/actions/0': additional properties 'requiredArgs' not allowed`（JSON Pointer） |
| 仓库既有假设 | `scripts/validate-spec.mjs` 的注释写着「完整实例校验（**ajv**）属于 conformance 套件的职责」 |
| 现状 | TS 侧没有任何实例校验；Go 侧有，且**按 `additionalProperties: false` 拒绝未知字段** |

## 2. 选型（决策一：ajv）

同一份真实 schema（`module-manifest.schema.json`）实测：

| 维度 | ajv | zod |
|---|---|---|
| 能否直接吃 `spec/*.json` | ✅ 10 份全注册、`strict: true` 下零警告 | ❌ 必须手写等价声明 |
| 漂移风险 | 零（校验的就是规范文件） | 高（实测：声明里没有 `requiredArgs` 概念，规范加字段它不报错） |
| 安装体积 | **1.0 MB**（+ `ajv-formats` 55 KB） | **3.5 MB** |
| 编译 10 份 schema | **65–90 ms** | 不适用 |
| 错误路径 | JSON Pointer（`instancePath: "/actions"`） | 数组 `["actions"]`，需转换 |
| 与 Go 对齐 | 同一种 JSON Pointer | 需额外映射层 |

**决定：用 `ajv@8`（2020-12 入口 `ajv/dist/2020.js`）。** 理由：这个包的职责就是「校验规范本身」，任何把规范翻译第二遍的方案都会引入漂移；自研最小校验器在 `$ref` 与组合关键字上风险过高。

> **执行期更正（2026-10-27）**：初版写「必须 `ajv-formats`」，实测不成立——全仓 schema 只用到一个 `format`（`date-time`），而 `ajv-formats` 是 55 KB 的 CJS 包，在 NodeNext 下还会带来默认导入不可调用的互操作问题。改为**自己注册一个 RFC 3339 校验器**（正则与 `common.schema.json` 的 `Iso8601Timestamp.pattern` 同源，再加 `Date.parse` 兜「结构对但日期不存在」）。未注册的 format 在编译期会报错，这反而是好事（fail-closed：新 format 必须显式声明）。

## 3. 未知字段（决策二：生产严、消费宽）

### 撞到的矛盾

- 三份 schema 里共 14 处 `additionalProperties: false`；
- 但 [`versioning.md`](../../../spec/0.1/versioning.md) §3.2 要求「**未知字段必须忽略**」——这是「0.1 消费方能处理 0.2 报文」的前提。

若接收侧照 schema 严格拒绝，**加一个字段就是破坏性变更**，与「只增不删」自相矛盾。2026-10-27 加 `requiredArgs` 时，Go 的 `additional properties 'requiredArgs' not allowed` 就是这条矛盾的现场。

### 裁决

**`additionalProperties: false` 约束写入方，§3.2 约束读取方。** 同一份 schema 编译**两个视图**：

| 视图 | ajv 配置 | 用途 | 多余字段的处理 |
|---|---|---|---|
| `lenient`（接收侧） | `removeAdditional: 'all'` | 宿主/适配器校验收到的报文 | **剥离并放行** |
| `strict`（一致性侧） | `removeAdditional: false` | conformance 套件校验**自产**报文 | 视为错误 |

两者共享同一批 `addSchema`，**不存在第二份真源**。

> **执行期更正（2026-10-27）**：初版让一致性视图开 ajv 的 `strict: true`（schema lint）。实测**三处误伤**——`action#/allOf/0/then` 的 `required` 不在本分支 `properties`（`strictRequired`）、`action#/properties/inputSchema` 的 union type、`audit-event#/allOf/0/if/properties/decision` 缺 `type: object`。这些都是**合法 JSON Schema**，lint 检查的是「schema 该怎么写」而不是「实例合不合法」，而本仓库 schema 已由 Go 校验器与 `validate-spec.mjs` 把关。因此两个视图**都关 lint**（`strict: false`），唯一差别是 `removeAdditional`——严格性只体现在实例上。

**附带动作（文档）**：给 `versioning.md` §3.2 补一句「`additionalProperties: false` 约束的是写入方；读取方必须按本节忽略未知字段」。不补的话，下一个人还会踩同一个坑。

**已知缺口（本轮只记不改）**：Go host 现在按 schema 拒绝接收报文，与 §3.2 不一致；记入 README「还没做的」。

## 4. API（决策三：每类一个函数 + 动态入口）

```ts
// 每类**有根实例契约**的 schema 一个函数，返回校验后的值（lenient 视图已剥离未知字段）。
// 名字与各 schema 的根 title 对齐；根是 oneOf 的（frames）也在同一函数里分派。
export function validateModuleManifest(value: unknown, opts?: ValidateOptions): ModuleManifest;
export function validateAction(value: unknown, opts?: ValidateOptions): Action;
export function validatePlan(value: unknown, opts?: ValidateOptions): Plan;
export function validatePolicyRule(value: unknown, opts?: ValidateOptions): PolicyRule;
export function validateGrant(value: unknown, opts?: ValidateOptions): Grant;
export function validateAuditEvent(value: unknown, opts?: ValidateOptions): AuditEvent;
export function validateDomainEvent(value: unknown, opts?: ValidateOptions): DomainEvent;
export function validateFrame(value: unknown, opts?: ValidateOptions): Frame;
export function validateError(value: unknown, opts?: ValidateOptions): ErrorEnvelope; // 根 required: tenonError（信封形态）

// wire 上按契约名分派时用（frame 的 `type` 字段就是契约名）
export function validateByContract(contract: ContractName, value: unknown, opts?: ValidateOptions): unknown;
```

- **通过返回校验后的值**，失败抛 `ValidationError`（带 `contract`、`issues[]`）——不给 `true/false`，否则调用方还要再 `as` 一次。
- **视图由 `opts.strict` 选择**：默认 lenient（接收侧）；`{ strict: true }` 走一致性视图。
- **`common.schema.json` 不导出校验函数**：它根级只有 `type: object` 与 `$defs`（`ModuleId`、`EntityRef`、`CorrelationId`、`Iso8601Timestamp`…），没有「一个实例」的概念——它只被其它 schema `$ref`。实体寻址的校验用 `entity-ref.ts` 的 `toEntityRef`，不重复造。

## 5. 错误模型

```ts
interface ValidationIssue {
  path: string;      // JSON Pointer，根为 "/"；与 Go 的 at '/actions/0' 同形
  keyword: string;   // ajv 的 keyword（type / required / additionalProperties / oneOf …），供程序判断
  message: string;   // 人能读的一句话
}

class ValidationError extends Error {
  readonly contract: ContractName;
  readonly issues: ValidationIssue[];
}
```

**`message` 不追求与 Go 逐字相同**：两个库措辞必然不同，强行对齐是负资产；对齐的是 **path 格式** 与 **keyword 可编程判断**。

## 6. schema 的打包

包不能 `import '../../spec/...'`（发布后路径会断）。做法：

1. `pnpm --filter @tenon/core run sync:schemas`：把 `spec/0.1/schemas/*.json` 复制到 `packages/tenon-core/schemas/`（**git 跟踪**）；
2. 同一脚本的 `--check` 模式：逐文件比内容哈希，不一致则退出非零；
3. 根 `pnpm check` 追加 `sync:schemas --check`——**规范改了没同步就红**。

## 7. 加载策略

- **懒编译 + 缓存**：首次用到某契约时 `getSchema`/`compile`，结果缓存；
- 全量预热实测 65–90 ms，**不做**导入即编译（会拖慢每一个只用到一类契约的调用方）；
- 暴露 `warmUp()` 供服务启动时可选预热。

## 8. 验收口径（机械可验）

1. `pnpm check` 包含 TS 校验测试（现有 `validate + typecheck + test` 继续成立）；
2. **9 类契约 × 3 个用例**（合法 / 类型错 / 多余字段）——9 = 10 份 schema 减去 `common`（只提供 `$defs`，无实例契约）：
   - 合法 → 通过且**未知字段被剥离**（lenient）；
   - 类型错 → `issues[0].path` 是 JSON Pointer；
   - 多余字段 → lenient **通过**、`strict` **失败**，且用例名写明这一点；
3. `sync:schemas --check` 在 CI 通过；**故意改一个副本文件会让它失败**（用一个测试或脚本自证）；
4. 错误 `path` 根为 `/`，嵌套为 `/actions/0`；
5. 包内不出现指向 `../../spec` 的运行时路径（用 grep 断言）。

## 9. 风险

| 风险 | 处理 |
|---|---|
| `ajv/dist/2020.js` 的 ESM/CJS 互操作（`new Ajv2020()` 默认导出） | 已实测可行；tsconfig `module: NodeNext` 下用 default import |
| `removeAdditional: 'all'` 会**原地修改**传入对象 | 文档写明「返回校验后的值，不要继续用入参」；实现里先 `structuredClone` 再校验，避免污染调用方 |
| `ajv-formats` 的 `date-time` 与 Go 的 RFC3339 判定不完全等价 | 不在校验器里比较两侧措辞；`Iso8601Timestamp` 的边界由 conformance 用例覆盖 |
| 体积（1 MB）对浏览器端 UI 插件偏重 | 本轮只服务 Node 侧（宿主/适配器）；浏览器打包优化留到真正需要时 |

## 10. 明确不做

- 不做 JSON Schema → zod 代码生成（决策一已否）；
- 不引入 `ajv`/`ajv-formats` 之外的校验依赖；
- 不改任何现有 schema 的结构（只改 `versioning.md` 的一句澄清）；
- 不改 Go host 的接收侧行为（记为缺口）。
