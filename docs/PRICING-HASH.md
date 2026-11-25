# 价格书 hash 对齐（官网 `sha256:3a1bb82e…` vs Console `sha256:25c72df…`）

状态：已定位，Console 侧已修复；官网未部署、未重新生成（按 P1 约束）。
相关代码：`apps/api/internal/billing/canonical.go`、`apps/api/internal/billing/price_book.go`
校验脚本：`apps/api/scripts/check-price-hash.py`

## 1. 结论

**不是价格数值过期，而是三类“序列化形状”不一致。**

1. **canonicalization 不同**：Console 原来直接 `json.Marshal` Go struct，顶层字段按结构体
   声明顺序（`version, effective_at, fx, models, tools`），模型对象按
   `model, currency, tiers, peak`，价格 map 由 Go 排序；官网
   `generate-prices.mjs` 的 `canonicalJson` 是**每一层对象都按 key 排序**、数组保持顺序。
   同样的内容，两种序列化必然得到不同 sha256。
2. **数组顺序不同**：官网的 canonical 只排序 object 的 key，**数组保持文档顺序**。
   fixture 的 `models` 按 CSV 顺序（33 个模型），而 Console `Finalize` 按 model
   名排序；`tools` 在 fixture 里是 `search, fetch`，Console 按名称排序成
   `fetch, search`。数组顺序是 hash 的一部分，所以即使 key 排序相同，输入顺序
   不同 hash 也不同。
3. **官网页面不是从 Console API 生成的**：`relay.html` 当前由
   `worktrees/fixtures/pricing-book.v1.json` 生成。该 fixture 不带 `peak` 字段；
   Console 公开视图按契约 §2.1 总是输出 `peak: []`。所以即使 Console 改用官网的
   canonicalization，**只要页面仍从旧 fixture 生成，hash 也不会等于 API 的 hash**。

一句话：**价格数值一致（seed CSV = fixture，逐 model/tier/component 相同），但 API 的
canonicalization（key + 数组顺序）与页面输入书的形状（缺 `peak`）都不一致；
页面必须从 API 重新生成，hash 才会一致。**

## 2. 复现与量化

用 `worktrees/seeds/price-book-v1.csv`（33 个模型 / 39 个 tier）重建两种投影：

| 输入 / 序列化 | hash |
|---|---|
| 页面 fixture `pricing-book.v1.json`（无 `peak`，tools search→fetch）+ 官网 canonical | `sha256:3a1bb82ead471b423bcc3efda632f42ff65a037086cecc94170c5f6db8002843`（页面当前值） |
| seed CSV + 旧 Console 形状（`peak: []`、tools fetch→search）+ Go struct 顺序 | `sha256:25c72df78ac3254e72dcc2b307804255d19d8369b87d321e318b7d2939d62155`（Console 当前值，已用 seed 重建复现） |
| seed CSV + 新 Console 形状（同上）+ **官网 canonical（key 排序）** | `sha256:cbb08ad37d204ddea81da37d62a52bc7b44bc6cf31c53d0be3ecd6ed09e7ef8b`（修复后的 API 值，本地 seed 库已验证） |

复现命令：

```bash
cd worktrees/console-main/apps/api

# 官网 fixture：通过的 canonical 校验
python3 scripts/check-price-hash.py \
  --book ../../../../worktrees/fixtures/pricing-book.v1.json

# 现场 API（只读，可在 staging 执行）
python3 scripts/check-price-hash.py \
  --book https://<staging-console>/v1/pricing/book \
  --page ../../../../roost-website/relay.html

# 证明“数值一致、形状不同”：只比较 model/tier/component 的 retail
python3 scripts/check-price-hash.py \
  --book https://<staging-console>/v1/pricing/book \
  --compare ../../../../worktrees/fixtures/pricing-book.v1.json
```

脚本会把 mismatch 诊断为：

- `declared == legacy_hash` → 旧 Console 的 struct 顺序问题（本次根因 1）；
- 两者都不等 → 内容本身与声明 hash 的来源不同（fixture/seed 真的过期）；
- `--compare` 只比价格数值，用来排除“价格过期”。

另外，`worktrees/reports/pricing-impact-v1-uniform2.5x.md` 和
`worktrees/fixtures/README.md` 里写的 hash 是 `sha256:d82fa1b1…`，而当前 fixture
的实际 hash 是 `sha256:3a1bb82e…`，属于**文档陈旧**，不影响 API。

## 3. 修复

### 3.1 Console（已改）

`billing.PriceBook.computeHash` 改为使用 `canonicalJSON`：

- `json.Marshal` 后按 `UseNumber` 反解为 `any`，再递归写回；
- 每层 object 的 key 排序，数组保持顺序，无空白；
- string 用 `SetEscapeHTML(false)`，与 Node `JSON.stringify` 一致（`<` `>` `&` 不转义）；
- 数字用 `json.Number` 保留整数精度；
- 顶层 `hash` 字段仍不参与计算。

这样 **API 返回的 `hash` == 官网用同一份响应算出的 `bookHash`**。单测
`TestPriceBookHashUsesCanonicalJSON` 把契约 golden Sonnet 书钉在
`sha256:159b1370cd8ef4a60ac43d222713949873c972165e869d8faef71cc3950dcc08`，与 Node 脚本一致。

### 3.2 官网（未执行，按约束不部署）

页面必须用生产/预发 API 重新生成，而不是继续吃旧 fixture：

```bash
cd roost-website
# 只生成到临时文件先核对，不覆盖 relay.html
node scripts/generate-prices.mjs \
  --book https://<staging-console>/v1/pricing/book \
  --out /tmp/relay.html
node scripts/check-prices.mjs \
  --book https://<staging-console>/v1/pricing/book \
  --page /tmp/relay.html
```

核对通过后再覆盖 `relay.html` 并部署（本任务不做）。fixture 若要继续作为本地
默认书，需同时补 `peak: []` 并把 `tools` 按名称排序，或直接从 API 导出，否则
fixture 的 hash 永远等于不了 API 的 hash。

### 3.3 上线注意

- 修复后 ETag/hash 会变化一次（`25c72df…` → 当前 seed 为 `cbb08ad3…`），这是
  预期行为；客户端继续用 `If-None-Match`/ETag 即可。
- 页面、relay 配置生成器、Console `/v1/pricing/book` 三者应在同一次发布后用
  脚本校验，避免“页面已换、relay 还是旧配置”。
- 不要手工编辑 `pricing-book*.json` 的 `hash`；用生成器/脚本重算。

## 4. 验收清单

- [ ] `go test ./internal/billing -run 'TestCanonical|TestPriceBookHash'`
- [ ] staging `GET /v1/pricing/book` 的 `hash` 能被
      `scripts/check-price-hash.py --book <url>` 验证为 canonical；
- [ ] `--compare` 与当前 fixture 无价格差异（排除数值过期）；
- [ ] 从 API 生成的页面 `data-price-hash` == API `hash`（部署前）；
- [ ] 不修改、不部署官网页面（本分支约束）。
