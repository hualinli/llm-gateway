# T12 · 5 分钟发布候选 Demo（成员调用 → 企业查费 → 余额拒绝 → 撤销传播 → 故障恢复 → 平台审计）

> 目标：一个没读过这个仓库的人，按本文在 5 分钟内跑完一条真实的垂直闭环，并且知道
> **每一步应该看到什么**。全部是真实进程、真实 PostgreSQL/Redis、真实会话与真实记账；
> 没有任何 mock 的业务逻辑（只有上游模型是本地假 Provider）。

契约与证据：`docs/verification/2026-10-01/t12-release-candidate/README.md`。
一键复现整条验收：`scripts/release-candidate.sh run`。

---

## 0. 准备（一次性，约 2–3 分钟；不计入 5 分钟）

```bash
cd <repo root>
export DOCKER_CONFIG=/path/to/writable/docker-config   # 受限环境下必须（见下）
export GOCACHE=/path/to/gocache                         # 可选
scripts/release-candidate.sh up
```

`up` 会：构建镜像 → 起 PostgreSQL/Redis/mocks → 跑 migration → 启动
Control/Console/Gateway/Metering → bootstrap 平台管理员 + 两家企业 → 提交受控
Provider/路由 → 建价格版本、给企业 A 充值、建成员与 billing_viewer。

输出末尾会打印：

```text
[..] ==> 栈已就绪并保留运行。
[..]     Console  http://127.0.0.1:18092
[..]     Gateway  http://127.0.0.1:18090
[..]     Metering http://127.0.0.1:18091
[..]     work dir <repo>/data/release-candidate/<run-id>
```

**所有口令与 Key 只存在于 `<work dir>/secrets/`（目录 0700，文件 0600）**：

| 文件 | 内容 |
|---|---|
| `secrets/admin-password` | 平台管理员密码（每次运行随机生成） |
| `secrets/control-admin-token` | Control 管理面 token（随机） |
| `secrets/provider-credential-master-key` | 本次运行专属的 Provider 凭据主密钥 |
| `secrets/identities.json` | 成员 / billing_viewer 的邮箱与口令 |
| `secrets/state.env` | 端口、compose project、compose 文件列表 |

下面用几个 shell 变量把路径和端口固定下来（真实的 Demo 里直接照抄）：

```bash
W="$(readlink -f data/release-candidate/current)"     # 也可以写死 work dir
C="http://127.0.0.1:18092"; G="http://127.0.0.1:18090"
ADMIN_EMAIL=rc-admin@example.com
ORG_A="$(python3 -c "import json;print(json.load(open('$W/secrets/identities.json'))['org_a']['id'])")"
MEMBER_A="$(python3 -c "import json;d=json.load(open('$W/secrets/identities.json'));print(d['users']['member_a']['email'])")"
MEMBER_A_PW="$(python3 -c "import json;d=json.load(open('$W/secrets/identities.json'));print(d['users']['member_a']['password'])")"
VIEWER_A="$(python3 -c "import json;d=json.load(open('$W/secrets/identities.json'));print(d['users']['viewer_a']['email'])")"
VIEWER_A_PW="$(python3 -c "import json;d=json.load(open('$W/secrets/identities.json'));print(d['users']['viewer_a']['password'])")"
```

> 也可以什么都不敲，直接跑脚本化的同一套演示：
> `scripts/release-candidate.sh demo` —— 它会打印下面每一条命令并实时执行，
> 输出与本文的"应该看到"逐条对应（实测 5–7 秒走完，见 runs/rc-demo-01/demo-transcript.txt）。

---

## 1. 成员登录 → 建 Key → 流式调用（约 60 秒）

```bash
# 1.1 成员登录（拿到会话 cookie + CSRF token）
LOGIN=$(curl -s -c "$W/member.jar" -X POST "$C/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$MEMBER_A\",\"password\":\"$MEMBER_A_PW\"}")
echo "$LOGIN" | python3 -m json.tool | head -20
CSRF=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['csrf_token'])" "$LOGIN")

# 1.2 成员给自己建一把 Key（secret 只出现这一次）
KEY=$(curl -s -b "$W/member.jar" -X POST "$C/api/v1/orgs/$ORG_A/keys" \
  -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -d '{}')
echo "$KEY" | python3 -c "import json,sys;d=json.load(sys.stdin);print('key_id',d['key']['key_id'],'prefix',d['key']['prefix'],'secret_returned_once',d['secret_returned_once'])"
SECRET=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['secret'])" "$KEY")

# 1.3 流式调用（模型 default 路由到本地 primary mock）
curl -sN -X POST "$G/v1/chat/completions" \
  -H "Authorization: Bearer $SECRET" -H 'Accept: text/event-stream' \
  -H 'Content-Type: application/json' \
  -d '{"model":"default","stream":true,"messages":[{"role":"user","content":"hello"}]}' | tail -5
```

**应该看到**

* 登录 `200`，响应里 `organizations` 含 `demo-enterprise-a` 且 `role=member`，
  并带 `csrf_token`；`Set-Cookie` 是 `gw_session=…; HttpOnly; SameSite=Lax`
  （demo 走明文 http，所以**没有** `Secure`；prod profile 会因
  `CONSOLE_SECURE_COOKIES!=true` 拒绝启动）。
* 建 Key `201`，`secret_returned_once=true`，`key.status=active`。
* 调用 `200`，SSE 以 `data: [DONE]` 结束，内容里带 `primary`
  （这是 mock 的名字，用来证明真的打到了上游）。
* Key 从创建到 Gateway 接受通常 **< 1 秒**（Console 2 秒一轮发布策略快照）。

**一句话解释**：Key 的归属（org/user/key row）与余额都在 PostgreSQL 里，Gateway 在
每一次上游调用前会用 `GATEWAY_ACCESS_GUARD=pg` + `GATEWAY_BILLING=pg` 校验并预占资金。

---

## 2. 企业视角看到这笔收费（约 30 秒）

```bash
# 2.1 billing_viewer 登录（只读角色）
VLOGIN=$(curl -s -c "$W/viewer.jar" -X POST "$C/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$VIEWER_A\",\"password\":\"$VIEWER_A_PW\"}")

# 2.2 账户（可用余额 = 入账 - 预占）
curl -s -b "$W/viewer.jar" "$C/api/v1/orgs/$ORG_A/billing/account" | python3 -m json.tool

# 2.3 账本流水（结算扣费 + 预占释放）
curl -s -b "$W/viewer.jar" "$C/api/v1/orgs/$ORG_A/billing/entries?limit=5" \
  | python3 -c "import json,sys;[print(e['kind'],e['direction'],e['amount_micro'],e['business_id']) for e in json.load(sys.stdin)['items']]"

# 2.4 计量与用量
curl -s -b "$W/viewer.jar" "$C/api/v1/orgs/$ORG_A/usage?limit=5" \
  | python3 -c "import json,sys;d=json.load(sys.stdin);u=d['items'][0];print('model',u['model'],'provider',u['provider'],'tokens',u['total_tokens'],'key',u['key_id'],'exec',u['execution_id']);print('totals',d['totals'])"
```

**应该看到**

* `account.available_micro` **比调用前小**，`reserved_micro` 在结算后回到 0。
* 流水里有 `kind=debit`、`business_id=billing_receipt:debit:exec_…` 的**结算扣费**行，
  以及 `kind=release` 的预占释放行（预占是上界，结算按真实 token 数扣）。
* 用量行带 `organization_id / user_id / key_id / execution_id / model / provider / tokens`。
* 结算由 dispatcher **异步**完成，通常 < 1 秒；`S3` 验收步骤会轮询到可见为止并记录时延。

**一句话解释**：`available_micro = posted_balance_micro - reserved_micro`；
预占（reservation）保证并发下不会超额放行，结算（settlement）把预占换成真实扣费。

**负例（顺手做掉）**：同一个 `member` 读账户会被 403。

```bash
curl -s -o /dev/null -w '%{http_code}\n' -b "$W/member.jar" "$C/api/v1/orgs/$ORG_A/billing/account"   # 403
```

---

## 3. 余额不足：在触达上游之前被拒（约 30 秒）

企业 B 从未充值，且 `rc-refuse-probe` 这个模型路由到一个**故意不可达**的 Provider
（`http://127.0.0.1:1`）。因此：

* 如果请求真的被转发 → 只可能是 `502 upstream_unavailable`；
* 拿到 `402 insufficient_balance` → admission 在调用上游之前就拒绝了。

```bash
ORG_B="$(python3 -c "import json;print(json.load(open('$W/secrets/identities.json'))['org_b']['id'])")"
B_EMAIL="$(python3 -c "import json;d=json.load(open('$W/secrets/identities.json'));print(d['users']['member_b']['email'])")"
B_PW="$(python3 -c "import json;d=json.load(open('$W/secrets/identities.json'));print(d['users']['member_b']['password'])")"
BLOGIN=$(curl -s -c "$W/memberb.jar" -X POST "$C/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$B_EMAIL\",\"password\":\"$B_PW\"}")
BCSRF=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['csrf_token'])" "$BLOGIN")
BKEY=$(curl -s -b "$W/memberb.jar" -X POST "$C/api/v1/orgs/$ORG_B/keys" \
  -H "X-CSRF-Token: $BCSRF" -H 'Content-Type: application/json' -d '{}')
BSECRET=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['secret'])" "$BKEY")

# 3.1 企业 B（余额 0）-> 402
curl -s -w '\nHTTP %{http_code}\n' -X POST "$G/v1/chat/completions" \
  -H "Authorization: Bearer $BSECRET" -H 'Content-Type: application/json' \
  -d '{"model":"rc-refuse-probe","stream":true,"messages":[{"role":"user","content":"hello"}]}'

# 3.2 对照：企业 A（已充值）打同一个模型 -> 502（证明 Provider 真的不可达）
curl -s -o /dev/null -w 'control HTTP %{http_code}\n' -X POST "$G/v1/chat/completions" \
  -H "Authorization: Bearer $SECRET" -H 'Content-Type: application/json' \
  -d '{"model":"rc-refuse-probe","stream":true,"messages":[{"role":"user","content":"hello"}]}'
```

**应该看到**

```json
{"error":{"code":"insufficient_balance","message":"the organization's available balance cannot cover the maximum cost of this request", ...}}
```
HTTP `402`；对照是 `502 upstream_unavailable`；企业 B 的 `GET /usage` 里**没有**这一行
（被拒的请求不产生计量事实）。

**一句话解释**：余额不足是 **fail-closed 的准入拒绝**，不是"先调用再记账"；
响应里不会泄露余额数字。

---

## 4. 撤销传播（约 20 秒）

```bash
curl -s -b "$W/member.jar" -X POST "$C/api/v1/orgs/$ORG_A/keys/<key_id>/revoke" \
  -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -d '{}'
sleep 1
curl -s -w '\nHTTP %{http_code}\n' -X POST "$G/v1/chat/completions" \
  -H "Authorization: Bearer $SECRET" -H 'Content-Type: application/json' \
  -d '{"model":"default","stream":true,"messages":[{"role":"user","content":"hello"}]}'
```

**应该看到**：撤销 `200` 且 `key.status=revoked`；随后调用 `401 unauthorized`。
实测传播 **< 1 秒**（本候选运行 `revocation_propagation_s=0.001`）。

顺带一个相邻事实（验收步骤 S6 覆盖）：**被移出组织的成员**，其 Key 在数据面得到
`403 forbidden`，他自己的组织读接口变成 `404 not_found`。也就是说"撤销 Key"和
"移除成员"是两条独立收敛的路径。

**一句话解释**：Key 状态与成员关系都是 PostgreSQL 事实；Gateway 每请求校验一次，
旧快照不会把已撤销的凭据"恢复"回来。

---

## 5. 故障恢复：Redis 停机 fail-closed → 恢复（约 40 秒）

```bash
# state.env 里存着本次运行的 project 名与全部 compose 文件路径。
# 最简单的做法：让脚本把这条命令原样打印出来（`scripts/release-candidate.sh demo`
# 的第 5 步就是它）；要手敲就按下面两行拼：
COMPOSE="docker compose --project-name $(grep '^RC_PROJECT=' $W/state.env | cut -d= -f2)"
for p in $(grep '^COMPOSE_PATHS=' $W/state.env | cut -d= -f2- | tr -d "'"); do
  COMPOSE="$COMPOSE -f $p"
done

$COMPOSE stop redis          # 拿掉限流/传输依赖
sleep 2
curl -s -w '\nHTTP %{http_code}\n' -X POST "$G/v1/chat/completions" \
  -H "Authorization: Bearer $SECRET_FRESH" -H 'Content-Type: application/json' \
  -d '{"model":"default","stream":true,"messages":[{"role":"user","content":"hello"}]}'

$COMPOSE start redis
sleep 3
# 同样再发一次 -> 200
```

（`$SECRET_FRESH` 用第 1 步的方法再建一把 Key；演示脚本会自动做这件事。）

**应该看到**

| 阶段 | 观测 |
|---|---|
| 停机前 | `200` + `data: [DONE]` |
| Redis 停机 | `503 unavailable`（`LIMITER_ON_REDIS_ERROR=closed`：拒绝，而不是无额度放行） |
| Redis 恢复 | `200` + `data: [DONE]`，实测 < 1 秒 |

也可以换成杀 Gateway：`$COMPOSE restart gateway`，等 `/readyz` 变 200（实测 < 10 秒），
再用新 Key 调用成功 —— 验收步骤 S9 覆盖这一条。

**一句话解释**：限流状态在 Redis，Redis 不可用时**拒绝**而不是放行；资金事实在
PostgreSQL，所以进程/传输层重启不会丢账。

---

## 6. 平台管理员的异常与审计面（约 30 秒）

```bash
ALOGIN=$(curl -s -c "$W/admin.jar" -X POST "$C/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$(cat $W/secrets/admin-password)\"}")
ACSRF=$(python3 -c "import json,sys;print(json.loads(sys.argv[1])['csrf_token'])" "$ALOGIN")

# 6.1 资金异常队列（durable PG 事实；没有异常就是空队列）
curl -s -b "$W/admin.jar" "$C/api/v1/orgs/$ORG_A/billing/exceptions" | python3 -m json.tool

# 6.2 审计（谁能看到什么、谁改了什么，含失败）
curl -s -b "$W/admin.jar" "$C/api/v1/orgs/$ORG_A/audit?limit=20" \
  | python3 -c "import json,sys;d=json.load(sys.stdin);[print(i['action'],i['outcome'],(i.get('detail') or {}).get('model','')) for i in d['items'][:10]]"

# 6.3 月度账单
curl -s -b "$W/admin.jar" "$C/api/v1/orgs/$ORG_A/billing/statements" \
  | python3 -c "import json,sys;d=json.load(sys.stdin);print(d['month'],d['totals'])"
```

**应该看到**

* `/billing/exceptions` → `200`，带 `totals{open,resolved,dismissed}` 与 `note`
  （说明异常是 PostgreSQL 里的终态，不是日志行）。本次演示没有制造异常，所以是空队列
  ——**空队列是真实事实，不是假数据**。
* `/audit` → `200`，能看到刚才这一串动作：
  `org.key.create`、`billing.execution.reserved`、`billing.receipt.recorded`、
  `billing.settlement.settled`、`org.key.revoke`……
* `/billing/statements` → 当月 `credit_micro / debit_micro / net_micro`。

**一句话解释**：三视角（成员 / 企业 / 平台）用同一套权限矩阵和同一批 PostgreSQL 事实；
平台管理员是唯一能改价格与做额度调整的角色。

---

## 7. 收尾

```bash
scripts/release-candidate.sh down      # 只回收本次运行的 compose project（含卷）
```

---

## 附：受限网络下的两个已知环境要求

1. **`DOCKER_CONFIG` 必须指向可写目录**（例如仓库外的临时目录）。Docker CLI 需要在
   其中写 buildx 状态；不可写时 `docker compose build` 会以
   `mkdir …/.docker/buildx: operation not permitted` 失败。
2. **`proxy.golang.org` 不可达时**，`scripts/release-candidate.sh` 会自动读取
   `go env GOPROXY`（或 `--goproxy`）并生成一份只在本次运行使用的 Dockerfile 覆盖，
   把 `GOPROXY` 注入构建阶段；仓库里的 `Dockerfile` 不被修改。`goproxy.cn` 之类的
   镜像可用时构建即通过。

## 附：常见问题

| 现象 | 原因 / 处理 |
|---|---|
| 端口被占用 | 脚本自动改选空闲端口并打印 WARNING；要固定端口就先释放 18090/18092/15051/15052/25433/26379 |
| `console bootstrap` 报 `--demo-data is refused under the prod profile` | 说明 `GATEWAY_PROFILE` 被设成了 prod；Demo 需要 demo profile |
| 调用返回 `503 billing_unavailable` | 该模型没有生效的价格版本，或 PostgreSQL 不可达（fail-closed） |
| 调用返回 `503 unavailable` | Redis 不可达且 `LIMITER_ON_REDIS_ERROR=closed`（见第 5 步） |
| 新 Key 立刻调用返回 `401` | 策略快照还没收敛，等 1–2 秒；验收里是轮询等待并记录时延 |
