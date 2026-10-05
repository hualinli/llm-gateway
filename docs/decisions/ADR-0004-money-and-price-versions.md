# ADR-0004：金额精度、价格版本与资金账本（T05）

> 状态：Accepted（2026-10-01）
> 关联任务：T05（价格版本、精确金额、账户与流水）
> 上游：[ADR-0002](ADR-0002-iam-identity-and-organizations.md)（用户/组织）、[ADR-0003](ADR-0003-enterprise-rbac-keys-and-policy.md)（RBAC/Key）
> 下游：T06（admission/reserve/receipt）、T07（settlement/对账）、T08（三视角 UI）
> 实现：`migrations/000008_*`、`internal/billing`、`internal/console`、`cmd/console`

## 1. 背景与现状

- 旧计费是 demo 计价：`metering_events.cost_cents` 在写入时由“1 token = 1 分”算出，没有价格版本、没有账户、没有流水，也无法表达“这次请求用了哪一版价格”。
- 金额曾经只能用 `int64` 分表达，而真实单价远小于 1 分（每百万 token 几元），按分计价会系统性丢失精度。
- 没有账户行、没有 append-only 账本，因此没有可重算的投影，也没有可审计的调整路径。
- T06 需要“固定价格版本 + 原子预占”，T07 需要“幂等结算 + 分页对账”，两者都要求 T05 先把金额单位、价格身份和账本形状冻结。

## 2. 决策

### 2.1 单位：整数微元（10^-6 CNY），int64，禁止 float64
- 账本单位是**微元**：`1 CNY = 1_000_000 micro`。所有金额字段（价格、余额、流水、预占）都是 `BIGINT`。
- Go 侧金额一律 `int64`，本包不出现 `float64` 金额；`decimal`/`numeric`/“分”都不作为存储单位。
- 展示到“分”是格式化，不是精度截断：内部永远保留微元，只有 API 把整数微元序列化成**十进制字符串**（`"1500000"`），因为 JSON number 在 JavaScript 里是 float64，会把 int64 静默改值。
- 首版只有 CNY：`currency` 在 DB 层用 `CHECK (currency = 'CNY')` 固定，混币账本不可能出现；引入第二种货币需要新迁移 + 新 ADR。

### 2.2 价格：每百万 token 多少微元，输入输出独立
```text
price = (input_price_micro_per_million, output_price_micro_per_million)
charge_micro = ceil((input_tokens * input_price + output_tokens * output_price) / 1_000_000)
```
- **只舍入一次，舍入对象是加权和**：逐 token 或分输入/输出分别向上取整都会把误差累加给客户。例：3 个输入 token、单价 1 微元/百万，正确结果是 1 微元；逐 token 舍入会得到 3 微元。
- 溢出的判定是精确的：乘积用 128 位计算，先做 `ceil` 的加法，再**先证明商能放进 int64** 才转换。超界返回 `ErrAmountOverflow`，**绝不回绕、绝不饱和**。
- 金额维度上不变量是 `available = posted_balance_micro - reserved_micro`（§2.4），消费是 `debit` 流水。

### 2.3 价格版本不可变，按 effective_at 生效，admission 时固定
- `price_versions(id, model, currency, input/output_price_micro_per_million, provider_input/output_cost_micro_per_million, effective_at, created_by, created_at)`，`UNIQUE (model, effective_at)`。
- **改价 = 插入新行**，没有 UPDATE 路径：接口上没有 Update/Delete，数据库上 `BEFORE UPDATE OR DELETE` 触发器直接 `RAISE`（23001）。
- 取价语义是“`model` 在时刻 `t` 生效的版本”= `effective_at <= t` 中最大的那一行；同一时刻一个模型只有一套价格，答案唯一。
- **T06 在 admission 时把 `price_version.id` 固定进 `billing_executions`**；receipt/outbox 重放、结算、迟到 usage 全部用那一版价格，价格中途更新不影响在途请求。
- `provider_*_cost` 是**供应商成本**，可空表示“未知”。未知成本不等于 0：把 NULL 当 0 会凭空造出毛利。
- `list price / customer charge / provider cost` 三者分离：失败的 attempt 可能产生供应商成本但不向客户收费；T05 只提供三个数，收费策略由 T06/T07 的失败状态机决定。

### 2.4 账户是投影，流水是事实，二者同事务更新
```text
billing_accounts(org_id PK, currency, posted_balance_micro, reserved_micro, status, version, created_at, updated_at)
available = posted_balance_micro - reserved_micro
```
- `posted_balance_micro` 允许为负 = 显式应收/欠款；`reserved_micro >= 0` 由 CHECK 兜底。
- `status ∈ {active, suspended}` 只表达运营状态，不参与算术；暂停账户不能发起新请求（T06 的 admission 判断），但不阻止平台调整资金。
- 账户行是性能投影，**不是事实**：任何变动都必须在同一个 PG 事务里写流水 + 更新投影；`RecalculateProjection(org_id)` 用流水重算并比对，`Matches=false` 即告警。

### 2.5 账本 append-only，`direction` 是唯一符号
```text
billing_entries(id, org_id, kind, amount_micro, direction, business_id, payload_digest,
                reason, operator_user_id, request_id, posted_at, created_at)
UNIQUE (business_id, kind)
```
- `amount_micro > 0` 恒成立（DB CHECK），符号只由 `direction` 表达：同一条流水不能有两种写法。
- `direction` 是**该条目对其所属桶的符号**：`credit` = 桶增加，`debit` = 桶减少。
  - `credit` / `debit` / `adjustment` → 作用于 `posted_balance_micro`；
  - `reservation`（`direction=credit`）→ `reserved_micro` 增加；
  - `release`（`direction=debit`）→ `reserved_micro` 减少。
- 原表金额不可改：纠正走**反向调整流水**，`BEFORE UPDATE OR DELETE` 触发器保证任何原位改账（包括手写 SQL）都失败。`billing_accounts` 是投影，刻意不加触发器。
- T05 只写 `adjustment`；`reservation`/`release` 建表但不写，留给 T06，`RecalculateProjection` 从第一天就认识它们。

### 2.6 幂等：`(business_id, kind)` + `payload_digest`
- `business_id` 是业务事实的身份。平台调整接口把它取为 `Idempotency-Key` 请求头；T06/T07 的结算用 execution/receipt 的业务 id（因此 `AdjustParams` 同时保留 `business_id` 与 `idempotency_key` 两个字段，二者同时给出时必须相等）。
- `payload_digest` = 该 payload 的 SHA-256（长度前缀编码，避免分隔符歧义）。同键**同 digest** → 复用已存结果（`Replayed=true`，资金只动一次）；同键**异 digest** → `ErrIdempotencyConflict`，**不改资金**。
- 该唯一键是全局的（不按 org 分区）。查重命中**别的组织**的行时一律判冲突，绝不把别家流水返回给调用方。
- 事务顺序固定：① 保证账户行存在并 `SELECT ... FOR UPDATE` ② 查重 ③ 插入流水 ④ 更新投影 ⑤ 写 `console_audit_events`。五步同事务提交：资金动了但没审计记录的状态不存在。

### 2.7 legacy `CostCents` 保持原价，不用新价格重算
- `metering_events.cost_cents` 继续是 demo 价（1 token = 1 分）的累计消费，语义是**历史累计消费**，不是余额、不是账单金额。
- 新价格体系不回填、不重算历史行：历史已计费数据按当时的价格来源解释。用量 API 用 `cost_kind: demo_consumption` 明确标注。

## 3. 跨组织边界

`internal/billing` 的每个方法都接收**已授权**的 org id，并且每条 SQL 都是 `WHERE org_id = $1`：

- 没有“列出所有流水”的方法，只有 `ListEntries(org_id, ...)`；
- `GetEntry(org_id, id)` 查不到别家的行，返回 `ErrEntryNotFound`（与“不存在”不可区分）；
- 唯一全局的 `(business_id, kind)` 命中别家时判冲突而不是返回；
- HTTP 层先过权限矩阵（`view_org_usage`：owner/admin/billing_viewer），非成员一律 404，不泄露组织存在性。

## 4. API 与错误

- `GET/POST /api/v1/admin/prices`：平台管理员；`POST` 需要 CSRF；创建冲突返回 409。
- `GET /api/v1/orgs/{org}/billing/account`、`GET /api/v1/orgs/{org}/billing/entries`：组织读权限（矩阵 `view_org_usage`），分页游标是 `(posted_at, id)` 元组；没有账户行时 `account` 返回 404 而不是伪造 0 余额。
- `POST /api/v1/admin/billing/accounts/{org}/adjustments`：平台管理员 + CSRF + `Idempotency-Key` 头 + `reason`；`amount_micro` 是十进制字符串；重复幂等键同 payload 返回 200 + `replayed: true`，异 payload 返回 409。
- 所有金额（余额、流水、价格、成本）在 API 里都是字符串。

## 5. T06 依赖的形状（本任务只定义，不实现）

- `LatestPriceVersion(model, at)` / `GetPriceVersion(id)` → 在 admission 事务里读一次，把 `price_version_id` 写进 `billing_executions`，之后所有重放都用这一版。
- `PriceVersion.ChargeMicro(in, out)` 给出预占上界（配合平台强制的 `max_tokens` 上界）；`ProviderCostMicro` 单独计算且可 unknown。
- `Account.AvailableMicro()` 是 admission 的资金判据；原子预占 = `billing_accounts` 行锁 + `kind=reservation, direction=credit` 流水 + `reserved_micro += amount`，与 execution 行同事务。
- `RecalculateProjection` 已按 `reservation`/`release` 分类，T06/T07 不会绕过它。
- 结算 = `kind=debit/credit` 的消费流水 + `kind=release` 的释放流水，`business_id` 取 receipt/execution 业务 id。

## 6. 替代方案与否决理由

- **float64 / `numeric` / 分**：float64 不可精确表示金额；`numeric` 与 Go 侧 `int64` 之间的转换点太多；按分计价在每百万 token 单价下直接丢精度。
- **可 UPDATE 的价目表**：一旦允许原地改价，历史账单就无法复现，“当时是多少钱”变成不可回答问题。
- **用 `UPDATE billing_entries` 做冲正**：审计链路会丢事实；反向流水保留完整历史且可重算。
- **`ON CONFLICT DO NOTHING` 静默幂等**：同键异 payload 会被吞掉，调用方以为新 payload 生效；必须显式冲突。
- **用 Redis 作为余额事实源**：Redis 不是持久账本，重启/裁剪会丢资金事实；PG 事务 + 行锁才是首版答案。

## 计费请求的输入/输出边界（review 修复）

PG 计费模式当前只接受单份输出：省略 `n` 或显式传整数 `n=1`；其他值在预占和上游调用前返回 400 `invalid_request`。`max_tokens` 是每份输出的上限，不能用它为多份输出的合计费用预占。

输入只支持文本字符串或纯 `type=text` 内容数组。图片 URL、内联图片、音频、文件、tool/function 内容和未知 message 字段均拒绝；媒体的 token 成本不能由 URL/JSON 字节数推导。纯文本仍按原始 JSON 大小加模板开销保守预占。后续支持多份输出或媒体前，必须实现相应的合计/模型计费上界。
