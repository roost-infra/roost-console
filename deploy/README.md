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

不要提交 `api.env`、`SERVICE_SECRET`、Supabase service key、虎皮椒 appsecret 或 EPUSDT `secret_key`。

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

## USDT 支付（EPUSDT，可选）

第二个支付通道：自建 [EPUSDT](https://github.com/GMWalletApp/epusdt) 网关，
非托管，币直接进自己的钱包。Console 只通过它的 GMPay HTTP API 通信，不引入其代码。
不配置 `EPUSDT_API_BASE` 时整条通道关闭，虎皮椒流程不受影响。

| 变量 | 说明 |
| --- | --- |
| `EPUSDT_API_BASE` | 网关的服务间地址，如 `http://127.0.0.1:8000` |
| `EPUSDT_PID` / `EPUSDT_SECRET_KEY` | 网关后台「API 密钥」里的 PID 与 secret_key（secret，不入库不提交） |
| `EPUSDT_NOTIFY_URL` | `https://www.roostbots.com/console-api/v1/payments/epusdt/notify`，必须公网可达 |
| `EPUSDT_RETURN_URL` | 可选，支付完成后跳回的控制台地址 |
| `EPUSDT_ALLOW_EMAILS` | 可选，逗号分隔的邮箱白名单；留空 = 所有登录用户可见 |

四个必填项（base / pid / secret / notify）齐全才会启用。启用后
`GET /v1/billing/plans` 返回 `payment_methods`，前端在多于一种方式时让用户选择；
白名单在下单接口同样生效，不只是隐藏按钮。

流程与对账：

- 下单不指定币种和网络，用户在网关收银台自选；金额以人民币传给网关，汇率由网关决定。
- 网关回调带 HMAC-SHA256 签名，Console 校验签名、PID、订单号、网关 trade_id 和人民币金额
  后入账，返回 `ok`。入账用的交易号是 `epusdt_<trade_id>`，链上哈希保存在订单 `raw` 里。
- 回调丢失时，60 秒一次的对账任务按订单自己的 provider 查单：已支付→入账，
  已过期→关单，确认中/人工审核→保持 pending。
- 多付、少付会进网关的人工审核，不会自动到账：在网关后台处理后回调入账，
  或用上面的管理员补单接口。
- 网关默认 10 分钟订单过期（`order_expiration_time`），短于 Console 的 `PAYMENT_ORDER_TTL`；
  以网关为准，过期后 Console 对账会把订单置为 expired。过期后才到账的转账仍会回调入账。
- 网关的 API 密钥建议把 IP 白名单限制为 console-api 所在机器。
- 日志关注：`epusdt notify rejected`、`epusdt notify trade id mismatch`、
  `epusdt fulfill failed`、`epusdt order fulfilled`。

可选公开基址（CLI 引导配置 `GET /v1/cli/config` 会返回，默认值已指向现网；
staging 时按需覆盖，均为公开值不是 secret）：

- `CONSOLE_API_URL=https://www.roostbots.com/console-api`
- `MARKET_API_URL=https://www.roostbots.com/api`
- `CAUSAL_MEMORY_MCP_URL=https://causal-memory.com/memory/mcp`

当前使用路径式部署，不依赖新 DNS。
