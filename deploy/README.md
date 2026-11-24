# Deployment

生产部署当前使用：

```text
www.roostbots.com/console/      静态控制台
www.roostbots.com/console-api/  Go API（反代到 127.0.0.1:8790）
/etc/roost-console/api.env      API 环境变量
roost-console-api.service       systemd
```

部署步骤：

1. `cd apps/api && GOOS=linux GOARCH=amd64 go build -o /tmp/roost-console-api ./cmd/roost-console`
2. `cd apps/web && VITE_SUPABASE_URL=... VITE_SUPABASE_ANON_KEY=... VITE_API_URL=/console-api npm run build -- --base=/console/`
3. 上传 API 二进制和 `apps/web/dist/`。
4. 更新 `/etc/roost-console/api.env`，`systemctl restart roost-console-api`。
5. `nginx -t && systemctl reload nginx`。

不要提交 `api.env`、`SERVICE_SECRET`、Supabase service key 或虎皮椒 appsecret。

## 上线前必须补

- 虎皮椒 `XUNHUPAY_APPID` / `XUNHUPAY_APPSECRET`
- `XUNHUPAY_NOTIFY_URL=https://www.roostbots.com/console-api/v1/payments/xunhupay/notify`
- `XUNHUPAY_QUERY_URL`（可选，默认 `https://api.xunhupay.com/payment/query.html`）：
  API 每 60 秒用它主动查单，回调丢失也能补单
- `PAYMENT_ORDER_TTL`（可选，默认 `2h`）：超时的 created/pending 订单
  自动置为 expired，不再长期显示“处理中”
- 生产数据库角色 `roost_api` 和密码
- DNS：`console.roostbots.com` / `api.roostbots.com`（如需独立域名）

## 支付运维

- 回调丢失/验签失败导致订单停在 pending 时，管理员手动补单：

```bash
curl -X POST https://www.roostbots.com/console-api/v1/admin/orders/<orderID>/fulfill \
  -H "Authorization: Bearer <admin-supabase-jwt>" \
  -H "Content-Type: application/json" \
  -d '{"provider_trade_no":"<虎皮椒交易号>"}'
```

  幂等：已 fulfilled 再次调用返回 `already_fulfilled`；refunded 拒绝。
- 日志关注：`xunhupay notify bad signature`、`order reconciled as paid`、
  `expired stale orders`。
- 前端订单页对 pending 订单直接给“去支付”入口。

可选公开基址（CLI 引导配置 `GET /v1/cli/config` 会返回，默认值已指向现网；
staging 时按需覆盖，均为公开值不是 secret）：

- `CONSOLE_API_URL=https://www.roostbots.com/console-api`
- `MARKET_API_URL=https://www.roostbots.com/api`
- `CAUSAL_MEMORY_MCP_URL=https://causal-memory.com/memory/mcp`

当前使用路径式部署，不依赖新 DNS。

## USDT 直付（收款到自己的 OKX 地址）

没有支付网关：控制台按汇率算出一笔**唯一金额**，用户往我们的 OKX 充币地址转账，
后端定时用 OKX API 拉入账记录、按「链 + 金额」匹配订单并自动入账。

最低配置（只有地址和汇率也能收款，入账由管理员手动确认）：

```bash
# 每条链一个充币地址，只填需要开放的链
OKX_USDT_ADDRESS_TRC20=T...
OKX_USDT_ADDRESS_ERC20=0x...
OKX_USDT_ADDRESS_POLYGON=0x...
# 报价用的 CNY/USDT 汇率；改了立即影响下一笔订单
OKX_USDT_CNY_RATE=7.20
# 可选：报价有效期（默认 30m）、只对某些账号开放
OKX_USDT_ORDER_TTL=30m
OKX_USDT_ALLOW_EMAILS=
```

自动入账还需要一个**只读**的 OKX API Key（资产-充值查询权限）：

```bash
OKX_API_KEY=...
OKX_API_SECRET=...
OKX_API_PASSPHRASE=...
# 境外站默认 https://www.okx.com；OKX 其它区域站按需覆盖
OKX_API_BASE=https://www.okx.com
```

工作方式与注意事项：

- 每笔订单加一个随机尾数保证金额唯一（如 `13.8801`），这是同一地址能被区分的原因；
  订单未支付时该金额不会被重复分配。
- 匹配条件是「链 + 金额完全一致 + 入账时间晚于下单时间」。
  用户从交易所提现时若**扣除手续费**，到账金额会少，自动匹配不会命中，
  需管理员按 `## 支付运维` 的手动补单接口处理。
- 后台每 60 秒拉一次 OKX 入账记录（一次请求覆盖所有待支付订单），
  订单超过 `OKX_USDT_ORDER_TTL` 未支付会置为 expired。
- USDT 订单不走虎皮椒；账本、积分、发票口径与其它渠道一致（`billing.orders.provider = okx_usdt`）。
