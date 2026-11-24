# 邮箱模板（Supabase Auth）

三个应用共用一个 Supabase 项目（`veysvzfcyjxxbhvcfhuv`），所以确认/重置/魔法链接
模板必须使用 `{{ .RedirectTo }}`（由各应用在 `emailRedirectTo` 里指定），不能写死
某一个域名。

## 模板

确认注册：

```html
<h2>确认你的邮箱</h2>
<p>点击下面的按钮完成注册。此链接只能使用一次。</p>
<p><a href="{{ .RedirectTo }}?token_hash={{ .TokenHash }}&type=signup">确认邮箱</a></p>
<p>如果不是你发起的注册，请忽略这封邮件。</p>
```

重置密码：

```html
<h2>重置你的密码</h2>
<p>点击下面的按钮设置新密码。此链接只能使用一次。</p>
<p><a href="{{ .RedirectTo }}?token_hash={{ .TokenHash }}&type=recovery">重置密码</a></p>
<p>如果不是你发起的请求，请忽略这封邮件。</p>
```

魔法链接：

```html
<h2>登录链接</h2>
<p>点击下面的按钮登录。此链接只能使用一次。</p>
<p><a href="{{ .RedirectTo }}?token_hash={{ .TokenHash }}&type=magiclink">登录</a></p>
```

## 应用侧要求

`token_hash` 不是 implicit flow 的 `#access_token`，`detectSessionInUrl` 不会处理它。
每个应用必须自己兑换：

- console：`apps/web/src/lib/emailConfirm.ts`（`verifyOtp` 后清掉 URL 参数）
- dsalpha：`web/src/pages/Login/AuthConfirm.tsx`（已有）
- 桌面端：`withShellReturn` 依赖 `.RedirectTo` 才会带上 `/desktop` 段

## 应用模板（Management API）

```bash
curl -X PATCH "https://api.supabase.com/v1/projects/<ref>/config/auth" \
  -H "Authorization: Bearer $SUPABASE_ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  --data @templates.json
```

改完用真实注册验证链接域名 = 发起应用域名；Supabase 侧模板可能有几分钟缓存。
