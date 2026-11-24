# Roost 统一账号、积分与支付系统设计

状态：Draft v1 · 2026-11-11  
范围：Roost Console（新仓库）+ dslpha + roostbot + market；causal-memory 后续接入。  
前提：**现在没有真实用户，不做历史迁移，不做兼容层。**

---

## 0. 一句话

**Supabase 是唯一身份源，Roost Console 是唯一控制面，Roost Credits 是唯一余额，虎皮椒是默认支付入口（可选的 USDT 通道见 §4.5）。**

```text
                     Supabase Auth
                   organizations
                         │
                         ▼
                  Roost Console API
          ┌──────────────┼──────────────┐
          │              │              │
      Identity        Billing        Entitlements
      org/members     wallet/ledger   products/caps
          │              │              │
          └──────────────┼──────────────┘
                         │
       ┌─────────────────┼─────────────────┐
       ▼                 ▼                 ▼
    dslpha            roostbot           market
    FastAPI           Node/TS            Go
       │                 │                 │
       └──────── usage / lease / settle ───┘
```

原则：

1. 不做第四套账号系统。
2. 不在产品侧保存权威余额；产品只保存缓存和操作配额。
3. 钱只在 Roost Console 的 `billing` schema 里。
4. 支付回调只由 Console API 处理，产品不接触支付。
5. 没有用户，所以可以直接删除旧的本地钱包、账号和登录表。

---

## 1. 新仓库与组件

新仓库：`roost-console`

```text
roost-console/
├── apps/
│   ├── web/                 # React 19 + Vite + TS 控制台
│   └── api/                 # Go 控制面 / Billing API / 支付回调
├── migrations/              # Supabase Postgres schema
├── api/openapi.yaml         # 控制台与内部 API 契约
├── docs/
└── deploy/                  # Nginx / Docker / compose
```

为什么是 Go API：

- market 已经是 Go，账本/冻结/租约的并发模型可以复用。
- 支付回调、租约结算、并发扣款是服务端事务问题，不适合前端或 Edge Function。
- Web 和控制面分离，后面可以独立扩容。

### 部署域名

```text
www.roostbots.com         官网（现有静态站）
console.roostbots.com     控制台（新）
api.roostbots.com         Console API（新）
pay.roostbots.com         可选的收银台域名；也可以并进 console
deepseek-alpha.com        dslpha（现有）
```

支付回调必须公网 HTTPS：

```text
POST https://api.roostbots.com/v1/payments/xunhupay/notify
POST https://api.roostbots.com/v1/payments/epusdt/notify
```

---

## 2. 身份模型

### 2.1 Supabase

使用 dslpha 现有 Supabase 项目 `veysvzfcyjxxbhvcfhuv`，不新建身份库。

`public` schema：

```sql
create table organizations (
  id uuid primary key default gen_random_uuid(),
  name text not null,
  slug text not null unique,
  owner_user_id uuid not null references auth.users(id),
  plan_code text not null default 'free',
  status text not null default 'active',
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table organization_members (
  org_id uuid not null references organizations(id) on delete cascade,
  user_id uuid not null references auth.users(id) on delete cascade,
  role text not null check (role in ('owner','admin','member','viewer')),
  created_at timestamptz not null default now(),
  primary key (org_id, user_id)
);

create table profiles (
  user_id uuid primary key references auth.users(id) on delete cascade,
  display_name text,
  avatar_url text,
  created_at timestamptz not null default now()
);
```

注册后自动创建 **personal organization**：

- 新用户 → `organizations` 一行
- `organization_members` 一行，`role=owner`
- 组织名默认用邮箱前缀
- 一个用户后续可以加入多个组织；MVP 只自动建一个

### 2.2 JWT

Supabase Custom Access Token Hook 在 JWT 中加：

```json
{
  "sub": "user_uuid",
  "active_org_id": "org_uuid",
  "org_role": "owner"
}
```

- 默认组织是 personal org。
- 用户切换组织时更新 `app_metadata.active_org_id`，刷新 token。
- API 同时校验 `organization_members`，不信任客户端自报角色。

### 2.3 产品如何认同一个账号

所有产品统一：

1. 使用同一个 Supabase 项目和 **public anon key** 登录；
2. 验证 Supabase JWT（JWKS）；
3. 从 JWT 或 `/v1/me` 得到 `active_org_id`；
4. 只使用 `org_id` 做账务和权限。

第一阶段不做跨域 SSO 单点登录：

- 每个产品各自用 Supabase 登录；
- 同一个账号、同一个组织；
- 不在 `roostbots.com` 和 `deepseek-alpha.com` 之间共享 cookie。

第二阶段可选：

- Console 作为 OIDC issuer；
- 产品跳转 Console 登录；
- 产品间 token exchange。

**MVP 不需要第二阶段也能做到“一个账号通用所有产品”。**

---

## 3. 积分与套餐

### 3.1 单位

```text
1 Roost Credit = ¥0.01
100 Roost Credits = ¥1
10,000 Roost Credits = ¥100
```

所有产品统一用 `credits` 报价和扣费；人民币分只用于支付订单。

### 3.2 Plan

```sql
create table billing.plans (
  code text primary key,
  name text not null,
  price_cents integer not null default 0,
  duration_days integer not null default 30,
  included_credits bigint not null default 0,
  product_caps jsonb not null default '{}',
  features jsonb not null default '{}',
  active boolean not null default true
);
```

建议首发：

| code | 价格 | 积分/周期 | 说明 |
|---|---:|---:|---|
| free | ¥0 | 1,000 一次性 | 体验 |
| starter | ¥49/30d | 10,000 | 个人日常 |
| pro | ¥199/30d | 50,000 | 高频/多产品 |
| max | ¥499/30d | 150,000 | 高并发/团队 |
| byok-pro | ¥199/30d | 20,000 | 自带模型 key，token 不计 |

价格和积分都可配置，不在代码里写死。

### 3.3 产品权限

```sql
create table billing.entitlements (
  org_id uuid not null,
  product text not null,
  enabled boolean not null default true,
  monthly_cap_credits bigint,
  valid_until timestamptz,
  updated_at timestamptz not null default now(),
  primary key (org_id, product)
);
```

- 组织套餐包含所有产品，不再按产品单独卖套餐。
- `monthly_cap_credits` 防止一个产品吃光整个组织余额。
- `valid_until` 到期后产品访问停止或降级。

### 3.4 钱包与账本

```sql
create table billing.wallets (
  org_id uuid primary key,
  balance_credits bigint not null default 0 check (balance_credits >= 0),
  reserved_credits bigint not null default 0 check (reserved_credits >= 0),
  version bigint not null default 0,
  updated_at timestamptz not null default now()
);

create table billing.ledger (
  id bigserial primary key,
  org_id uuid not null,
  product text,
  delta_credits bigint not null,
  reason text not null,
  ref_id text,
  idempotency_key text unique,
  created_at timestamptz not null default now()
);

create table billing.usage_leases (
  id uuid primary key default gen_random_uuid(),
  org_id uuid not null,
  product text not null,
  run_ref text not null,
  reserved_credits bigint not null check (reserved_credits >= 0),
  status text not null check (status in ('active','settled','released','expired')),
  expires_at timestamptz not null,
  settled_credits bigint,
  created_at timestamptz not null default now(),
  unique (org_id, product, run_ref)
);
```

关键不变量：

- `balance_credits >= reserved_credits >= 0`
- 每条账本 `delta` 可正可负，钱包结余只能由账本推导
- 同一 `idempotency_key` 只能写一次
- 产品不能直接改余额，只能走 Console API

---

## 4. 虎皮椒支付

虎皮椒是 **API 模式、单次支付**。不假设自动续费，不做静默扣款。

### 4.1 SKU

```text
credit_50          ¥50    5,000 credits
credit_100         ¥100   10,000 credits
credit_500         ¥500   50,000 credits
plan_starter_30d   ¥49    10,000 credits + starter 30 天
plan_pro_30d       ¥199   50,000 credits + pro 30 天
plan_max_30d       ¥499   150,000 credits + max 30 天
plan_byok_pro_30d  ¥199   20,000 credits + byok-pro 30 天
```

### 4.2 订单表

```sql
create table billing.orders (
  id uuid primary key default gen_random_uuid(),
  org_id uuid not null,
  sku text not null,
  amount_cents integer not null,
  credits bigint not null default 0,
  status text not null default 'created'
    check (status in ('created','pending','paid','fulfilled','failed','refunded','expired')),
  provider text not null default 'xunhupay',
  provider_order_id text unique,
  provider_trade_no text unique,
  paid_at timestamptz,
  fulfilled_at timestamptz,
  raw jsonb not null default '{}',
  created_at timestamptz not null default now()
);

create table billing.payment_events (
  id bigserial primary key,
  provider text not null,
  event_id text not null,
  order_id uuid,
  payload jsonb not null,
  processed_at timestamptz,
  unique (provider, event_id)
);
```

### 4.3 流程

```text
1. 用户登录 console
2. 选择套餐/积分包
3. POST /v1/billing/orders
4. Console API 创建 order(status=pending)
5. 调虎皮椒下单，拿到 qr/url
6. 前端展示二维码/跳转
7. 用户微信/支付宝支付
8. 虎皮椒 POST notify
9. 验签 + 校验金额 + 锁 order
10. order.status=paid
11. 写 ledger(正积分)
12. 若是套餐：延长 subscription + 更新 entitlements
13. order.status=fulfilled
14. 控制台轮询/刷新看到余额
```

### 4.4 回调安全

- 用 `appsecret` 验签，不能信任任何前端参数
- 校验 `total_fee` 等于 order.amount_cents
- 校验 `out_trade_no`/provider_order_id
- 同一个 `trade_no` 只能入账一次
- 回调返回纯文本 `success`（按虎皮椒文档）
- 失败返回非 success，触发重试
- 记录原始 payload 到 `payment_events`
- 管理员可手工补单/退款，写审计

### 4.5 第二通道：USDT（EPUSDT，可选）

虎皮椒仍是默认入口。配置 `EPUSDT_*` 后增加一个可选的稳定币通道，
接自建的 EPUSDT 网关（独立进程，只走它的 GMPay HTTP API）。

- 订单仍落在 `billing.orders`，`provider='epusdt'`，`provider_order_id` 存网关 trade_id，
  `pay_url` 是网关收银台地址；入账走同一个 `Fulfill`，余额不分通道。
- 回调 `POST /v1/payments/epusdt/notify`：验 HMAC-SHA256 签名与 PID，
  校验订单号、trade_id、人民币金额等于 `amount_cents`，返回 `ok`；否则返回非 2xx 触发重试。
- 对账按订单的 `provider` 分流，绝不拿一个通道的结果给另一个通道的订单入账。
- 可见范围由 `EPUSDT_ALLOW_EMAILS` 控制，接口侧强制校验。
- 部署与运维见 `deploy/README.md`。

### 4.6 自动续费

虎皮椒单次支付不支持自动续费，因此：

- 默认卖 **30 天通行证** 或 **积分包**
- 到期前 7/3/1 天站内信 + 邮件提醒
- 不做静默扣款
- 后续如需自动续费，再评估微信/支付宝签约代扣

---

## 5. Console API

### 5.1 用户态

```text
GET    /v1/me
GET    /v1/orgs
POST   /v1/orgs
GET    /v1/orgs/{org_id}/billing
GET    /v1/orgs/{org_id}/members
POST   /v1/orgs/{org_id}/members/invite
GET    /v1/billing/plans
GET    /v1/billing/orders
POST   /v1/billing/orders
GET    /v1/billing/orders/{id}
GET    /v1/billing/ledger
GET    /v1/billing/usage
```

订单创建响应：

```json
{
  "id": "order_uuid",
  "status": "pending",
  "amount_cents": 19900,
  "credits": 50000,
  "qr_url": "https://...",
  "pay_url": "https://..."
}
```

### 5.2 产品服务态

产品用独立服务凭证调用，不能拿用户 JWT 直接扣款。

```text
POST /internal/v1/entitlements/resolve
  { org_id, product }
  → { enabled, plan, monthly_cap, remaining }

POST /internal/v1/usage/leases
  { org_id, product, run_ref, estimated_credits }
  → { lease_id, reserved, balance, expires_at }

POST /internal/v1/usage/leases/{lease_id}/settle
  { actual_credits }
  → { settled, balance }

POST /internal/v1/usage/leases/{lease_id}/release

POST /internal/v1/usage/sandbox
  { org_id, product, sandbox_id, ..., total_credits?, idempotency_key }
  → { id, charged_credits, balance_credits }

GET /v1/orgs/{orgID}/billing/details?days=30
  → { summary, tokens[], sandboxes[] }   用户自助扣费明细
```

认证：

- HMAC service token + timestamp + nonce
- 或 mTLS
- `org_id` 必须来自产品服务端，不接受用户直接伪造

### 5.3 租约并发

使用 Postgres 事务 + `SELECT ... FOR UPDATE`：

```sql
-- 伪代码
begin;
select * from billing.wallets where org_id = :org for update;
available = balance_credits - reserved_credits;
if available < estimate then rollback; end;
update wallets set reserved_credits = reserved_credits + estimate;
insert usage_leases(...);
commit;
```

结算：

```sql
begin;
select wallet for update;
select lease for update;
reserved -= lease.reserved;
balance -= actual;
insert ledger(-actual);
update lease settled;
commit;
```

过期 worker 释放 `active` 且 `expires_at < now()` 的租约。

---

## 6. 产品接入

### 6.1 dslpha

现状：FastAPI + Supabase Auth + 独立 quota-service credits。

改造：

- 用户：继续用同一个 Supabase 项目
- 组织：`user → organization_members → org_id`
- 删除 quota-service 的 wallet/user_plans/ledger（没有用户，直接删）
- credit gate：
  - 每回合开始调 `/internal/v1/usage/leases`
  - 结束调 settle
  - 本地缓存余额，避免每 token 调 API
- 升级按钮：
  - 跳 `console.roostbots.com/billing?org=...`
- 沙箱费用：
  - 由 dslpha 按实际秒数换算 credits 后 settle
- 产品配额：
  - 保留每回合上限
  - 月度 cap 从 entitlement 读取

### 6.2 roostbot

现状：SQLite accounts/sessions + budget quotas。

改造：

- 删除本地登录/账号表
- 用 Supabase JWT；`workspace.tenant_id = org_id`
- 保留 token/run/容器秒预算（操作配额）
- 不再保存人民币余额
- 用量通过 Console API reserve/settle
- 前端“用量与预算”页显示 Console 钱包，链到 console

### 6.3 market

现状：Go，自建 tenants/users/sessions/wallet/ledger；tenant 1:1 Daytona org。

改造：

- `tenant.id = org_id`；创建 org 时 provision Daytona org
- 删除本地用户/session；验证 Supabase JWT
- 钱包/ledger 移到 Console Billing
- 沙箱租约开始时 reserve，按 60s 结算
- provider 结算仍归 market，但资金账本只读 Console
- market 控制台改为调用 Console API 显示余额；不再有独立充值

### 6.4 causal-memory

后续：

- 认同 `org_id`
- 用量事件上报 Console
- 不建独立钱包

---

## 7. 前端控制台

页面：

```text
/login
/onboarding                 创建/加入组织
/orgs/:id/overview
/orgs/:id/billing           套餐、积分、订单、发票
/orgs/:id/billing/pay/:orderId  二维码/支付状态
/orgs/:id/members
/orgs/:id/products          产品开关/用量
/orgs/:id/usage
/orgs/:id/settings
```

关键体验：

- 登录后没有组织 → 自动创建 personal org
- 余额和用量按产品拆分
- 支付页轮询订单状态
- 显示“30 天后到期”，不做自动续费承诺
- 所有金额同时显示 credits 和 ¥

技术：

- React 19 + Vite + TypeScript
- Supabase JS 登录
- TanStack Query
- 与现有 Roost 官网视觉一致（深色 + 橙色强调）
- 不引入重型 UI 框架

---

## 8. 安全与合规

- Supabase RLS：成员只能读自己组织；钱包/账本只允许 Console API service role 写
- 支付私钥只在 Console API 环境变量
- 前端永远不接触 appsecret
- 回调验签 + 金额校验 + trade_no 幂等
- 所有管理员充值/退款写 `admin_audit`
- API 限流：登录、下单、回调
- 订单金额服务端计算，不接受前端传金额
- 日志脱敏：不记录手机号/支付凭证/完整回调敏感字段
- 条款/隐私/退款政策在 console 与官网可访问
- 国内支付需要 ICP/商户资质，按虎皮椒要求配置回调域名

---

## 9. 迁移策略

没有用户，直接硬切：

1. 停用 dslpha quota-service 的钱包/套餐入口
2. 停用 roostbot SQLite 登录/账号
3. 停用 market 本地用户/钱包
4. 清空测试数据
5. 所有测试账号重新在 Supabase 注册
6. 新系统从第一个真实用户开始

不做 user_id/org_id 历史映射。

---

## 10. 实施阶段

### Phase 1：身份与 Console 骨架

- 新仓库 `roost-console`
- Supabase organizations/members
- Go API：JWT 校验、/v1/me、org 创建
- Web：登录、组织创建、基础布局

验收：新用户注册 → 自动 personal org → 查询 /v1/me 返回 org。

### Phase 2：账本与套餐

- billing schema
- plans / wallets / ledger / usage_leases
- Console API 查询余额、套餐、用量
- 手工充值（管理员）
- 产品内部 lease/settle API

验收：同一 org 在 dslpha、roostbot、market 都能读到同一余额。

### Phase 3：虎皮椒

- 订单表、支付事件表
- 虎皮椒下单、回调、验签、幂等
- 收银台 + 二维码 + 轮询
- 套餐/积分到账

验收：真实 ¥0.01/¥1 测试支付 → 余额增加 → 三个产品可见。

### Phase 4：dslpha 接入

- 删除旧 quota-service 钱包
- credit gate 改 central leases
- 升级入口跳 console

### Phase 5：roostbot 接入

- 删除 SQLite 登录
- workspace tenant=org
- 预算/用量接入

### Phase 6：market 接入

- tenant=org
- Supabase JWT
- 沙箱 reserve/settle
- 删本地钱包

### Phase 7：打磨

- 邀请成员
- 发票/退款
- 到期提醒
- 监控/告警
- causal-memory 接入

---

## 11. 已定与待定

已定：

- 虎皮椒 API 模式
- Supabase 用 dslpha 现有项目
- 新仓库 `roost-console`
- 先接 dslpha、roostbot、market
- 不做历史迁移

待定：

1. 首发套餐价格和积分（建议值见上）
2. 是否做 7 天免费试用
3. 是否需要发票/对公转账
4. 是否做年付折扣
5. 是否要邀请成员/席位费
6. Go API 的部署位置（SG 同机或独立）
