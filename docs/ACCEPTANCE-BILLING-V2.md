# Billing V2 端到端验收清单（P1）

目标：在任何预发/测试环境上可重复验证以下 5 条，不需要跑生产：

1. 同一 `model + usage` 分别走 relay 与 dslpha，扣费差 ≤ 1 credit；
2. 过期（`expired`）reservation 仍能结算；
3. 余额不足时 partial debit + `debt` 行；
4. reserve 后价格版本变化 → settle 返回 **409**；
5. 压缩（compression）产生正的 `net_gain_minor`，且扣费低于未压缩。

自动化入口：`apps/api/scripts/acceptance-billing-v2.sh`（只连测试库，不调用生产 HTTP）。

## 0. 自动化验收（推荐，先跑这个）

前置：一个**一次性**的 Postgres，应用 `migrations/` 全部迁移并 seed
`billing.price_book` / `billing.settings`（ver `2026-11-25.1` 或测试版本）。

```bash
cd console-main/apps/api

ROOST_TEST_DATABASE_URL='postgres://user:pass@127.0.0.1:5432/roost_test' \
ROOST_ACCEPTANCE_CONFIRM=YES \
  ./scripts/acceptance-billing-v2.sh
```

脚本执行：

- `go test ./internal/billing -run '^TestBillingV2Acceptance$' -v`：5 条都在同一
  store 事务路径上验证（`RelaySettleV2` vs `SettleByRunV2`、过期、partial+debt、
  `ErrPriceVersionMismatch`、compression）；
- `go test ./internal/httpapi -run 'TestRelaySettleV2ErrorCodes|...UUID...'`：HTTP 层
  的 409 / 400 映射；
- 可选 `ROOST_ACCEPTANCE_PRICE_BOOK=<file|url>`：价格书 canonical hash 校验
  （见 `docs/PRICING-HASH.md`）。

> 脚本会写测试库（创建 org/lease/ledger/debt），**不要**把
> `ROOST_TEST_DATABASE_URL` 指到生产。生产只允许只读审计 SQL（见
> `worktrees/audit-billing-v2-console/`）。

## 1. 手工 HTTP 清单（staging）

所有请求都带 service 认证。relay 与 dslpha 兼容接口接受 legacy token
（`X-Service-Token`）；market 的 `/internal/v1/*` 只接受 HMAC 签名
（`X-Roost-Service` + `X-Roost-Timestamp` + `X-Roost-Nonce` +
`X-Roost-Signature`，见 `docs/SERVICE-AUTH.md`）。以下用 legacy token 演示 relay /
dslpha，避免测试脚本手写 HMAC。

变量：

```bash
BASE=https://staging-console.example      # 不要填生产
RELAY_SECRET=...                          # staging relay service secret
DSLPHA_SECRET=...                         # staging dslpha service secret
USER_ID=<测试用户 uuid>                   # 有默认 org、余额充足
ORG_ID=<该用户 org uuid>
```

### 1.1 relay：reserve → settle

```bash
REQ="req-accept-$(date +%s)"

# reserve：hold 500 credits（amount_minor = 500000）
curl -sS -X POST "$BASE/v1/relay/reservations" \
  -H "Content-Type: application/json" -H "X-Service-Token: $RELAY_SECRET" \
  -d "{\"request_id\":\"$REQ\",\"subject_ref\":\"$ORG_ID\",
       \"amount_minor\":500000,\"currency\":\"CREDIT\",
       \"price_version\":\"2026-11-25.1\"}"
# 期望 200；记下 reservation_id，响应 amount_credits=500

RES_ID=<reservation_id>

# settle v2：claude-sonnet-5.5，150k input / 2k output
curl -sS -X POST "$BASE/v1/relay/reservations/$RES_ID/settle" \
  -H "Content-Type: application/json" -H "X-Service-Token: $RELAY_SECRET" \
  -d "{\"settlement_id\":\"stl-$REQ\",\"idempotency_key\":\"settle:$REQ\",
       \"model\":\"claude-sonnet-5.5\",\"price_version\":\"2026-11-25.1\",
       \"usage\":{\"input\":150000,\"output\":2000}}"
# 期望 200；amount_credits=180, amount_minor=179595（当前 golden 价）
```

### 1.2 dslpha：acquire → release（同一 model+usage）

```bash
RUN="run-accept-$(date +%s)"

curl -sS -X POST "$BASE/api/auth/billing/leases/acquire" \
  -H "Content-Type: application/json" -H "X-Service-Token: $DSLPHA_SECRET" \
  -d "{\"user_id\":\"$USER_ID\",\"run_ref\":\"$RUN\",\"spent_credits\":250}"
# 期望 200 granted=true

curl -sS -X POST "$BASE/api/auth/billing/leases/release" \
  -H "Content-Type: application/json" -H "X-Service-Token: $DSLPHA_SECRET" \
  -d "{\"user_id\":\"$USER_ID\",\"run_ref\":\"$RUN\",
       \"model_usage\":[{\"model\":\"claude-sonnet-5.5\",
                         \"input\":150000,\"output\":2000,\"calls\":1}]}"
# 期望 200；charged_credits 与 relay 的 amount_credits 差 ≤ 1
#（当前两路都来自同一 PriceCharge，应该完全相同：180）
```

比对 `charged_credits` 与 `amount_credits`：`abs(diff) <= 1`。若不等，先看两边
`price_version` 是否都锁定到同一个版本。

### 1.3 过期结算

先新建一个 reservation（不要复用 1.1 已经 settled 的），再把它置为过期
（模拟 `ExpireLeases`）：

```bash
REQ_EXP="req-expired-$(date +%s)"
curl -sS -X POST "$BASE/v1/relay/reservations" \
  -H "Content-Type: application/json" -H "X-Service-Token: $RELAY_SECRET" \
  -d "{\"request_id\":\"$REQ_EXP\",\"subject_ref\":\"$ORG_ID\",
       \"amount_minor\":50000,\"currency\":\"CREDIT\",
       \"price_version\":\"2026-11-25.1\"}"
RES_EXP=<reservation_id>
```

```sql
-- 只对测试库执行；模拟 ExpireLeases：先释放 hold，再标记 expired
update billing.wallets
   set reserved_credits = reserved_credits - 50
 where org_id = '<ORG_ID>';
update billing.usage_leases set status = 'expired'
 where id = '<RES_EXP>';
```

然后 settle 这个过期 reservation（新的 `settlement_id` / `idempotency_key`）：

```bash
curl -sS -X POST "$BASE/v1/relay/reservations/$RES_EXP/settle" \
  -H "Content-Type: application/json" -H "X-Service-Token: $RELAY_SECRET" \
  -d "{\"settlement_id\":\"stl-expired-$REQ_EXP\",\"idempotency_key\":\"settle:expired:$REQ_EXP\",
       \"model\":\"claude-sonnet-5.5\",\"price_version\":\"2026-11-25.1\",
       \"usage\":{\"input\":1000,\"output\":10}}"
# 期望 200；amount_credits=2, amount_minor=1179；reserved 不再被扣第二次
```

### 1.4 余额不足 → partial + debt

准备一个余额 150、reservation 150 的测试 org（可直接用 SQL 建，或用一个新用户），
settle 一笔应付 180 的 relay 请求：

```bash
# reserve 150，再 settle 150k/2k（应付 180）
# 期望 200；debts 数组恰好 1 行：amount_credits=30,
# reason=insufficient_balance；balance=0；ledger 只扣 150
```

`debts` 是非空数组；`billing.debts` 里能查到同一 `run_ref` 的 open 行。

### 1.5 改价 → 409

新建一个 reservation 时锁 `price_version=2026-11-25.1`，settle 时故意报另一个版本
（不要复用已 settled 的 reservation）：

```bash
REQ_PRICE="req-price-$(date +%s)"
# 先用 price_version=2026-11-25.1 reserve 200，记下 RES_PRICE
# curl ... /v1/relay/reservations -d '{"request_id":"...","subject_ref":"<ORG>",
#   "amount_minor":200000,"currency":"CREDIT","price_version":"2026-11-25.1"}'
RES_PRICE=<reservation_id>

curl -sS -o /tmp/settle.json -w '%{http_code}\n' \
  -X POST "$BASE/v1/relay/reservations/$RES_PRICE/settle" \
  -H "Content-Type: application/json" -H "X-Service-Token: $RELAY_SECRET" \
  -d "{\"settlement_id\":\"stl-price-$REQ_PRICE\",\"idempotency_key\":\"settle:price:$REQ_PRICE\",
       \"model\":\"claude-sonnet-5.5\",\"price_version\":\"changed-version\",
       \"usage\":{\"input\":1}}"
# 期望 409；error.code=price_version_mismatch；wallet/ledger 不变
```

同一路径下 `amount_minor` 与 Console 重算不一致也是 409
（`error.code=amount_mismatch`）。

### 1.6 压缩 net gain

再新建一个 reservation，带 `basis` settle：

```bash
REQ_CMP="req-cmp-$(date +%s)"
# 用 price_version=2026-11-25.1 reserve 500，记下 RES_CMP
RES_CMP=<reservation_id>

curl -sS -X POST "$BASE/v1/relay/reservations/$RES_CMP/settle" \
  -H "Content-Type: application/json" -H "X-Service-Token: $RELAY_SECRET" \
  -d "{\"settlement_id\":\"stl-cmp-$REQ_CMP\",\"idempotency_key\":\"settle:cmp:$REQ_CMP\",
       \"model\":\"claude-sonnet-5.5\",\"price_version\":\"2026-11-25.1\",
       \"usage\":{\"input\":150000,\"output\":2000},
       \"basis\":{\"mode\":\"original\",\"before_input_tokens\":150000,
                 \"after_input_tokens\":90000,\"summary_cost_minor\":20}}"
# 期望 200；compression.net_gain_minor > 0
#（当前 golden：166 credits / 165283 minor，net_gain_minor=16573）
```

`billing.usage_compression` 中该 lease 的 `net_gain_minor` 与响应一致。

## 2. 验收结果记录

| # | 检查 | 期望 | staging 结果 |
|---|---|---|---|
| 1 | relay vs dslpha 同 model+usage | 差 ≤ 1 credit（应为 0） | |
| 2 | expired reservation settle | 200，按 usage 扣费，reserved 不重复释放 | |
| 3 | 余额不足 | partial debit + 1 条 debt，ledger 不为负 | |
| 4 | settle 报错价格版本 | 409 `price_version_mismatch`，钱不动 | |
| 5 | 压缩 | `net_gain_minor > 0`，扣费 < 未压缩 | |

## 3. 未完成 / 边界

- 本清单不覆盖真实 relay / dslpha 代码的联调，只覆盖 Console 合同面；跨服务
  “同 usage” 还需在 staging 跑一次真实 relay 请求和 dslpha 一次 release，比对
  `reservation_id` / `run_ref` 对应的 ledger 行。
- 生产验收需要 staging 的 service secret 和一次性测试 org；本分支没有生产凭据，
  因此未执行。
- 任何调整余额/补账动作都不在本清单内；见审计报告 `worktrees/audit-billing-v2-console/`。
