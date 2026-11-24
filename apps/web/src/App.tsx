import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import type { Session } from '@supabase/supabase-js';
import { supabase } from './lib/supabase';
import { redeemEmailTokenHash } from './lib/emailConfirm';
import { CopyButton, CopyField, DSALPHA_URL, EndpointList, KeyCreatedModal, MEMORY_MCP_URL, MODELS_URL, RELAY_BASE_URL, SnippetTabs } from './access';
import { Brand, Icon, Sidebar, Topbar, useRoute } from './shell';
import { api, type AdminOrg, type AdminOverview, type AdminOverviewDaily, type AdminReferrals, type BillingDetails, type BillingSandboxRow, type BillingTokenRow, type BillingView, type MeResponse, type Order, type Plan, type ReferralOverview, type RelayKey, type UsageDailyRow, type UsageRecentRow, type UsageSessionRow, type UsageTotals } from './lib/api';

const PRODUCT_LABELS: Record<string, string> = {
  dsalpha: 'dsalpha · 投研工作台',
  dslpha: 'dsalpha · 投研工作台',
  market: 'Roost Market · 算力市场',
  'causal-memory': 'Causal Memory · 智能体记忆',
};

const CREDIT_PACKS = [
  { sku: 'token_50', label: '5,000 credits', price: '¥50', note: '≈ DeepSeek V4-Pro 25M 输入 tokens，全产品通用' },
  { sku: 'token_100', label: '10,000 credits', price: '¥100', note: '≈ DeepSeek V4-Pro 50M 输入 tokens，全产品通用' },
  { sku: 'token_500', label: '50,000 credits', price: '¥500', note: '≈ DeepSeek V4-Pro 252M 输入 tokens，全产品通用' },
];

const LABS = [
  {
    key: 'sandbox',
    meta: 'SANDBOX · 实验性',
    title: 'Roost Sandbox 云电脑',
    desc: '给 Agent 用的隔离 Linux 环境：Terminal、文件、快照与规格。面向开发者，普通使用不需要开通。',
    action: '打开 Sandbox',
    href: '/market/sandboxes?sso=1&next=%2Fsandboxes',
  },
  {
    key: 'market',
    meta: 'MARKET · 实验性',
    title: 'Roost Market 算力市场',
    desc: 'Provider 节点接入、租约、上架容量与收益结算。',
    action: '进入 Market',
    href: '/market/?sso=1&next=%2Fmarket',
  },
];

function LabsPage() {
  return (
    <section className="section" aria-labelledby="labs-title">
      <div className="section__head">
        <div>
          <p className="kicker">LABS</p>
          <h2 id="labs-title">沙箱与算力</h2>
        </div>
        <p className="section__intro">
          面向开发者的底层能力，仍在迭代。它们和其他产品共用同一个组织余额，按实际运行时长计费。
        </p>
      </div>
      <div className="workspace-grid">
        {LABS.map((item) => (
          <a className="workspace-card glass" href={item.href} key={item.key}>
            <div className="workspace-card__top">
              <span className="workspace-card__meta">{item.meta}</span>
              <span className="workspace-card__arrow" aria-hidden="true">→</span>
            </div>
            <h3>{item.title}</h3>
            <p>{item.desc}</p>
            <span className="workspace-card__action">{item.action}</span>
          </a>
        ))}
      </div>
    </section>
  );
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// Orgs created by the product bridges are named after the Supabase user id.
// That is a correct unique key but a terrible label, so render a person org.
function displayOrgName(name: string, ownerUserID: string): string {
  if (!name) return '—';
  if (UUID_RE.test(name) && (!ownerUserID || name === ownerUserID)) return '个人组织';
  return name;
}

function credits(n: number) {
  return new Intl.NumberFormat('zh-CN').format(n);
}

// Tokens get a compact form so millions stay readable: 1,234,567 -> 1.2M,
// 12,345 -> 12.3K, and small counts stay raw.
function compactFraction(value: number) {
  return value.toFixed(1).replace(/\.0$/, '');
}

function tokens(n: number) {
  const abs = Math.abs(n);
  if (abs >= 1_000_000) return `${compactFraction(n / 1_000_000)}M`;
  if (abs >= 1_000) return `${compactFraction(n / 1_000)}K`;
  return credits(n);
}

// The relay reports compression savings in minor units: 1000 minor = 1 credit.
function minorCredits(minor: number) {
  return new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 3 }).format(minor / 1000);
}

function yuan(cents: number) {
  return `¥${(cents / 100).toFixed(2)}`;
}

function planLabel(code: string) {
  return code === 'byok-pro' ? 'BYOK Pro' : code.charAt(0).toUpperCase() + code.slice(1);
}

// Referral codes are uppercase URL-safe tokens. Parsing is deliberately strict:
// strip everything that cannot be part of a code so a pasted link/hash or a
// trailing punctuation character still resolves.
const REFERRAL_STORAGE_KEY = 'roost_ref';

function normalizeReferralCode(raw: string | null | undefined) {
  return (raw ?? '').trim().toUpperCase().replace(/[^A-Z0-9]/g, '').slice(0, 32);
}

function referralFromQuery(raw: string) {
  const query = raw.includes('?') ? raw.slice(raw.indexOf('?') + 1) : raw.replace(/^#/, '');
  return normalizeReferralCode(new URLSearchParams(query).get('ref'));
}

// The invite link is /console/?ref=CODE, but Supabase auth redirects may put the
// query in the hash; support both forms.
function referralCodeFromLocation() {
  if (typeof window === 'undefined') return '';
  const fromSearch = normalizeReferralCode(new URLSearchParams(window.location.search).get('ref'));
  if (fromSearch) return fromSearch;
  return referralFromQuery(window.location.hash || '');
}

function storedReferralCode() {
  try {
    return normalizeReferralCode(window.localStorage.getItem(REFERRAL_STORAGE_KEY));
  } catch {
    return '';
  }
}

function storeReferralCode(code: string) {
  try {
    if (code) window.localStorage.setItem(REFERRAL_STORAGE_KEY, code);
    else window.localStorage.removeItem(REFERRAL_STORAGE_KEY);
  } catch {
    /* private mode / storage disabled: the in-memory flow still works */
  }
}

// Drop ?ref=... once claimed so a reload does not re-open the claim prompt for
// an invite that has already been rewarded. Keeps other query/hash parameters.
function stripReferralFromLocation() {
  if (typeof window === 'undefined') return;
  try {
    const url = new URL(window.location.href);
    let changed = false;
    if (url.searchParams.has('ref')) {
      url.searchParams.delete('ref');
      changed = true;
    }
    const hash = url.hash;
    const queryIndex = hash.indexOf('?');
    if (queryIndex >= 0) {
      const params = new URLSearchParams(hash.slice(queryIndex + 1));
      if (params.has('ref')) {
        params.delete('ref');
        const rest = params.toString();
        url.hash = hash.slice(0, queryIndex) + (rest ? `?${rest}` : '');
        changed = true;
      }
    }
    if (changed) window.history.replaceState(null, '', url.toString());
  } catch {
    /* history may be unavailable in embedded webviews */
  }
}

function ledgerReasonLabel(reason: string) {
  switch (reason) {
    case 'admin_gift': return '管理员赠送';
    case 'admin_adjust': return '管理员调整';
    case 'referral_inviter': return '邀请奖励 · 邀请人';
    case 'referral_invitee': return '邀请奖励 · 新用户';
    case 'free_grant': return '新组织赠送';
    default: return reason;
  }
}

function Logo() {
  return <Brand />;
}

function AuthScreen({ initialError = '' }: { initialError?: string }) {
  const [mode, setMode] = useState<'login' | 'signup'>('login');
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState(initialError);
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState(false);
  const [refCode, setRefCode] = useState(() => referralCodeFromLocation() || storedReferralCode());

  useEffect(() => {
    const incoming = referralCodeFromLocation();
    if (incoming) {
      storeReferralCode(incoming);
      setRefCode(incoming);
    }
  }, []);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError('');
    setNotice('');
    try {
      if (mode === 'signup') {
        const cleanRef = normalizeReferralCode(refCode);
        storeReferralCode(cleanRef);
        const { data, error } = await supabase.auth.signUp({
          email,
          password,
          options: {
            emailRedirectTo: `${window.location.origin}/console/`,
            data: cleanRef ? { referral_code: cleanRef } : undefined,
          },
        });
        if (error) throw error;
        if (!data.session) {
          setNotice('账号已创建，请查收验证邮件后登录。');
        }
      } else {
        const { error } = await supabase.auth.signInWithPassword({ email, password });
        if (error) throw error;
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : '登录失败';
      setError(message === 'Invalid login credentials'
        ? '邮箱或密码不正确。如果这个账号是通过邮箱链接注册的（没有设置密码），请点下方「发送登录链接」。'
        : message);
    } finally {
      setBusy(false);
    }
  }

  async function sendLoginLink() {
    setBusy(true);
    setError('');
    setNotice('');
    try {
      const cleanEmail = email.trim();
      if (!cleanEmail) throw new Error('请先填写邮箱');
      const { error } = await supabase.auth.signInWithOtp({
        email: cleanEmail,
        options: {
          shouldCreateUser: false,
          emailRedirectTo: `${window.location.origin}/console/`,
        },
      });
      if (error) throw error;
      setNotice('登录链接已发送，请到邮箱点击链接（1 小时内有效）。');
    } catch (err) {
      const message = err instanceof Error ? err.message : '发送失败';
      setError(message === 'Signups not allowed for otp'
        ? '这个邮箱还没有账号，请先创建组织。'
        : message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="auth">
      <section className="auth__story">
        <Logo />
        <div className="auth__story-body">
          <p className="eyebrow"><span className="dot" /> ONE ORGANIZATION</p>
          <h1>一个 Key，<br /><span>所有主流模型。</span></h1>
          <p className="auth__lead">
            注册即得 API Key：Claude、GPT、Gemini、DeepSeek、Kimi、GLM 统一 OpenAI / Anthropic 兼容接口。
            同一份余额还能用于 DSAlpha 投研和 Agent 记忆。
          </p>
          <div className="auth__ledger">
            <div className="auth__ledger-head">
              <span>roost · api</span>
              <span className="tag tag--live">active</span>
            </div>
            <div className="auth__ledger-row"><span>Base URL</span><strong className="mono">api.roostbots.com/v1</strong></div>
            <div className="auth__ledger-row"><span>产品</span><span>API 中转 · DSAlpha · 记忆</span></div>
            <div className="auth__ledger-row"><span>计费</span><span>按量扣费 · 100 credits = ¥1</span></div>
          </div>
        </div>
        <p className="auth__foot">Roost · AI 模型中转与智能体平台</p>
      </section>
      <section className="auth__pane">
        <div className="auth__form-wrap">
          <p className="kicker">{mode === 'login' ? 'WELCOME BACK' : 'CREATE YOUR ORG'}</p>
          <h2>{mode === 'login' ? '登录 Roost' : '创建 Roost 账号'}</h2>
          <p className="muted">
            {mode === 'login' ? '登录后进入你的组织控制台。' : '注册后自动创建一个 personal organization。'}
          </p>
          <form onSubmit={submit} className="form">
            <label>
              邮箱
              <input type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="you@example.com" required autoComplete="email" />
            </label>
            <label>
              密码
              <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder={mode === 'login' ? '没有密码可留空，用下方登录链接' : '至少 8 位'} required={mode === 'signup'} minLength={mode === 'signup' ? 8 : undefined} autoComplete={mode === 'login' ? 'current-password' : 'new-password'} />
            </label>
            {mode === 'signup' && (
              <label>
                邀请码 <span className="muted">（可选）</span>
                <input
                  value={refCode}
                  onChange={(e) => setRefCode(normalizeReferralCode(e.target.value))}
                  placeholder="好友分享的邀请码"
                  autoComplete="off"
                  spellCheck={false}
                />
              </label>
            )}
            {mode === 'login' && refCode && (
              <p className="form-notice">检测到邀请码 {refCode}，登录后将自动领取邀请奖励。</p>
            )}
            {error && <p className="form-error">{error}</p>}
            {notice && <p className="form-notice">{notice}</p>}
            <button className="btn btn--ink btn--wide" disabled={busy}>
              {busy ? '请稍候…' : mode === 'login' ? '登录' : '注册'}
            </button>
          </form>
          {mode === 'login' && (
            <button className="auth__switch" type="button" onClick={() => void sendLoginLink()} disabled={busy}>
              没有设置密码？发送登录链接
            </button>
          )}
          <button className="auth__switch" type="button" onClick={() => { setMode(mode === 'login' ? 'signup' : 'login'); setError(''); setNotice(''); }}>
            {mode === 'login' ? '还没有账号？创建组织' : '已有账号？直接登录'}
          </button>
        </div>
      </section>
    </main>
  );
}

function WalletCard({ billing }: { billing: BillingView }) {
  return (
    <section className="wallet" id="billing">
      <div className="wallet__main">
        <p className="eyebrow"><span className="dot" /> AVAILABLE CREDITS</p>
        <div className="wallet__amount">
          <strong>{credits(billing.wallet.available_credits)}</strong>
          <span>credits</span>
        </div>
        <p className="wallet__cash">≈ ¥{(billing.wallet.available_credits / 100).toFixed(2)} · 100 credits = ¥1</p>
        <div className="wallet__meta">
          <span>总额 {credits(billing.wallet.balance_credits)}</span>
          <span>冻结 {credits(billing.wallet.reserved_credits)}</span>
          <span>套餐 {planLabel(billing.plan.code)}</span>
        </div>
      </div>
      <div className="wallet__side">
        <p className="mono">ORG_ID</p>
        <code>{billing.org.id}</code>
        <div className="wallet__side-row"><span>组织</span><strong>{displayOrgName(billing.org.name, '')}</strong></div>
        <div className="wallet__side-row"><span>角色</span><strong>{billing.org.role || 'owner'}</strong></div>
        <CopyButton text={billing.org.id} label="复制组织 ID" className="wallet__copy" />
      </div>
    </section>
  );
}

// Invite card for the organization owner. The link and code are generated by the
// API on first read; the reward amounts come from the same response so the UI
// cannot drift from REFERRAL_INVITER_CREDITS / REFERRAL_INVITEE_CREDITS.
function ReferralCard({ token, version }: { token: string; version: number }) {
  const [data, setData] = useState<ReferralOverview | null>(null);
  const [error, setError] = useState('');
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setError('');
    api.referral(token)
      .then((overview) => { if (!cancelled) setData(overview); })
      .catch((err) => { if (!cancelled) setError(err instanceof Error ? err.message : '加载邀请链接失败'); });
    return () => { cancelled = true; };
  }, [token, version]);

  async function copy() {
    if (!data) return;
    try {
      await navigator.clipboard.writeText(data.link);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1600);
    } catch {
      setError('复制失败，请手动选择链接复制。');
    }
  }

  return (
    <section className="section referral" id="referral">
      <div className="section__head">
        <div><p className="kicker">REFERRAL · INVITE</p><h2>邀请好友，双方得积分。</h2></div>
        <p className="section__intro">
          把邀请链接发给好友。好友注册后 7 天内首次领取，你和好友各得奖励；
          每个新账号只能领取一次，每个组织只有一个邀请码。
        </p>
      </div>
      {error && <p className="form-error">{error}</p>}
      {!data ? (
        <p className="muted">正在生成邀请链接…</p>
      ) : (
        <div className="referral__card">
          <div className="referral__main">
            <p className="kicker">YOUR INVITE LINK</p>
            <code className="referral__link">{data.link}</code>
            <div className="referral__actions">
              <button className="btn btn--accent btn--small" type="button" onClick={() => void copy()}>
                {copied ? '已复制' : '复制邀请链接'}
              </button>
              <span className="muted">邀请码 <strong className="mono">{data.code}</strong></span>
            </div>
            <p className="referral__hint">
              好友得 {credits(data.invitee_credits)} credits，你得 {credits(data.inviter_credits)} credits。
              奖励直接进入组织钱包，并写入账本（邀请奖励 · 邀请人 / 新用户）。
            </p>
          </div>
          <div className="referral__stats">
            <div><span>已邀请</span><strong>{credits(data.invited_count)}</strong></div>
            <div><span>你已获得</span><strong>{credits(data.total_inviter_credits)}</strong></div>
            <div><span>好友共获得</span><strong>{credits(data.total_invitee_credits)}</strong></div>
          </div>
        </div>
      )}
    </section>
  );
}

// The three products ordinary users come for. Sandbox/Market live under Labs.
const CORE_PRODUCTS = [
  {
    key: 'relay',
    usageKeys: ['relay'],
    name: 'API 中转',
    tagline: 'Claude、GPT、Gemini、DeepSeek、Kimi、GLM 一个 Key 全部可用，价格低于官方。',
    cta: '获取 API Key',
    href: '#/keys',
    external: false,
  },
  {
    key: 'dsalpha',
    usageKeys: ['dsalpha', 'dslpha'],
    name: 'DSAlpha 投研',
    tagline: 'AI 投研工作台：行情、研报、因子与定时任务，开箱即用。',
    cta: '打开 DSAlpha',
    href: DSALPHA_URL,
    external: true,
  },
  {
    key: 'memory',
    usageKeys: ['causal-memory'],
    name: '记忆 Memory',
    tagline: '给 Agent 的长期记忆。通过 MCP 接入 Claude Code、Cursor 等客户端。',
    cta: '接入记忆',
    href: '#/memory',
    external: false,
  },
] as const;

const SAMPLE_MODELS = ['deepseek-v4-pro', 'claude-sonnet-5', 'gpt-6-sol', 'gemini-3.1-pro'];

function productUsage(billing: BillingView, keys: readonly string[]) {
  return billing.products
    .filter((row) => keys.includes(row.product))
    .reduce((sum, row) => sum + row.used_this_month, 0);
}

function Overview({ me, token }: { me: MeResponse; token: string }) {
  const orgId = me.org.id;
  const [keys, setKeys] = useState<RelayKey[] | null>(null);
  const [week, setWeek] = useState<UsageTotals | null>(null);
  const [keysFailed, setKeysFailed] = useState(false);
  const [weekFailed, setWeekFailed] = useState(false);
  const [reload, setReload] = useState(0);
  const [created, setCreated] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  useEffect(() => {
    let cancelled = false;
    setKeysFailed(false);
    setWeekFailed(false);
    api.relayKeys(token, orgId)
      .then((data) => { if (!cancelled) setKeys((data.tokens ?? []).filter((k) => !k.revoked_at)); })
      .catch(() => { if (!cancelled) setKeysFailed(true); });
    api.usageSummary(token, orgId, 7)
      .then((data) => { if (!cancelled) setWeek(data.totals); })
      .catch(() => { if (!cancelled) setWeekFailed(true); });
    return () => { cancelled = true; };
  }, [token, orgId, reload]);

  async function createKey() {
    setBusy(true);
    setError('');
    try {
      const data = await api.createRelayKey(token, orgId);
      setCreated(data.token);
      const list = await api.relayKeys(token, orgId);
      setKeys((list.tokens ?? []).filter((k) => !k.revoked_at));
    } catch (err) {
      setError(err instanceof Error ? err.message : '创建 API Key 失败');
    } finally {
      setBusy(false);
    }
  }

  const wallet = me.billing.wallet;
  const hasKey = (keys?.length ?? 0) > 0;
  const weekTotals = week ?? EMPTY_USAGE_TOTALS;

  return (
    <div className="overview">
      <section className="hero glass">
        <div className="hero__text">
          <p className="kicker">欢迎回来 · {displayOrgName(me.org.name, '')}</p>
          <h1>{hasKey ? '一切就绪，开始调用吧。' : '3 步开始调用 Roost API'}</h1>
          <p className="hero__lead">
            一个账号、一份余额，覆盖 API 中转、DSAlpha 与记忆。所有接入信息都在这一页。
          </p>
        </div>
        <div className="hero__actions">
          <a className="btn btn--accent" href="#/keys"><Icon name="keys" size={16} />API Key 与接入</a>
          <a className="btn btn--glass" href={MODELS_URL} target="_blank" rel="noreferrer">模型与价格</a>
        </div>
      </section>

      <div className="ov-grid">
        <section className="quickstart glass" aria-labelledby="qs-title">
          <div className="card-head">
            <div>
              <p className="kicker">QUICK START</p>
              <h2 id="qs-title">接入 API 中转</h2>
            </div>
            <a className="text-link" href="#/keys">完整接入代码 →</a>
          </div>
          {error && <p className="form-error">{error}</p>}
          <ol className="steps">
            <li className={`step ${hasKey ? 'is-done' : ''}`}>
              <span className="step__num">1</span>
              <div className="step__body">
                <h3>创建 API Key</h3>
                {keysFailed ? (
                  <p className="muted">Key 列表暂时读取失败。<button className="link-btn" onClick={() => setReload((n) => n + 1)}>重试</button></p>
                ) : keys === null ? (
                  <p className="muted">正在读取 Key…</p>
                ) : hasKey ? (
                  <p className="muted">已有 {keys.length} 个可用 Key。完整 Key 只在创建时显示，丢失可再建一个。</p>
                ) : (
                  <p className="muted">Key 用来鉴权和计费，消费组织余额。</p>
                )}
              </div>
              <div className="step__action">
                {hasKey ? (
                  <a className="btn btn--ghost btn--small" href="#/keys">管理 Key</a>
                ) : (
                  <button className="btn btn--accent btn--small" onClick={() => void createKey()} disabled={busy || keys === null || keysFailed}>
                    {busy ? '创建中…' : '创建 API Key'}
                  </button>
                )}
              </div>
            </li>
            <li className="step">
              <span className="step__num">2</span>
              <div className="step__body">
                <h3>填入 Base URL</h3>
                <p className="muted">在客户端的 “Base URL / API 地址” 里填：</p>
                <CopyField value={RELAY_BASE_URL} hint="Claude Code、Anthropic SDK 填 https://api.roostbots.com（不带 /v1）" />
              </div>
            </li>
            <li className="step">
              <span className="step__num">3</span>
              <div className="step__body">
                <h3>选择模型</h3>
                <p className="muted">模型名直接写进请求的 <code className="inline-code">model</code> 字段，点击复制：</p>
                <div className="chips">
                  {SAMPLE_MODELS.map((model) => <CopyButton key={model} text={model} label={model} className="chip" />)}
                  <a className="chip chip--link" href={MODELS_URL} target="_blank" rel="noreferrer">全部模型 →</a>
                </div>
              </div>
            </li>
          </ol>
        </section>

        <section className="balance glass" aria-labelledby="bal-title">
          <p className="kicker" id="bal-title">可用余额</p>
          <div className="balance__amount">
            <strong>{credits(wallet.available_credits)}</strong>
            <span>credits</span>
          </div>
          <p className="balance__cash">≈ ¥{(wallet.available_credits / 100).toFixed(2)} · 所有产品共用</p>
          <a className="btn btn--ink btn--wide" href="#/wallet">充值</a>
          {weekFailed ? (
            <p className="muted balance__stats-error">近 7 天用量暂不可用。<button className="link-btn" onClick={() => setReload((n) => n + 1)}>重试</button></p>
          ) : (
            <div className="balance__stats">
              <div><span>近 7 天消费</span><strong>{week ? minorCredits(weekTotals.credits_minor) : '…'}</strong></div>
              <div><span>近 7 天请求</span><strong>{week ? credits(weekTotals.requests) : '…'}</strong></div>
              <div><span>近 7 天 Tokens</span><strong>{week ? tokens(weekTotals.total_tokens) : '…'}</strong></div>
            </div>
          )}
          <a className="text-link" href="#/usage">查看用量详情 →</a>
        </section>
      </div>

      <section className="section" aria-labelledby="prod-title">
        <div className="section__head">
          <div>
            <p className="kicker">PRODUCTS</p>
            <h2 id="prod-title">我的产品</h2>
          </div>
          <p className="section__intro">所有产品共用组织余额，本月用量按产品分别统计。</p>
        </div>
        <div className="product-grid">
          {CORE_PRODUCTS.map((product) => (
            <a
              className={`product-card glass product-card--${product.key}`}
              href={product.href}
              key={product.key}
              target={product.external ? '_blank' : undefined}
              rel={product.external ? 'noreferrer' : undefined}
            >
              <div className="product-card__head">
                <span className={`product-icon product-icon--${product.key}`}>
                  <Icon name={product.key === 'relay' ? 'keys' : product.key} size={18} />
                </span>
                <span className="product-card__usage">本月 {credits(productUsage(me.billing, product.usageKeys))} credits</span>
              </div>
              <h3>{product.name}</h3>
              <p className="muted">{product.tagline}</p>
              <span className="product-card__cta">{product.cta} →</span>
            </a>
          ))}
        </div>
      </section>

      {created && <KeyCreatedModal apiKey={created} onClose={() => setCreated('')} />}
    </div>
  );
}

const USAGE_RANGES = [7, 30, 90];

const EMPTY_USAGE_TOTALS: UsageTotals = {
  requests: 0,
  input_tokens: 0,
  output_tokens: 0,
  cache_read_tokens: 0,
  cache_write_tokens: 0,
  total_tokens: 0,
  credits_minor: 0,
  upstream_cost_minor: 0,
  cache_hit_rate: 0,
  saved_tokens: 0,
  user_share_tokens: 0,
  user_share_minor: 0,
  summary_cost_minor: 0,
};

type ChartSegment = {
  key: string;
  label: string;
  field: 'input_tokens' | 'output_tokens' | 'cache_read_tokens' | 'cache_write_tokens';
};

const CHART_SEGMENTS: ChartSegment[] = [
  { key: 'input', label: 'Input', field: 'input_tokens' },
  { key: 'output', label: 'Output', field: 'output_tokens' },
  { key: 'cache_read', label: 'Cache read', field: 'cache_read_tokens' },
  { key: 'cache_write', label: 'Cache write', field: 'cache_write_tokens' },
];

// 1000 relay minor units = 1 credit, and the console displays 100 credits = ¥1.
function percent(rate: number) {
  if (!Number.isFinite(rate) || rate <= 0) return '0%';
  const value = rate * 100;
  return `${value >= 10 ? value.toFixed(0) : value.toFixed(1)}%`;
}

function minorYuan(minor: number) {
  return `¥${(minor / 100000).toFixed(2)}`;
}

function zeroUsageDay(date: string): UsageDailyRow {
  return {
    date,
    requests: 0,
    input_tokens: 0,
    output_tokens: 0,
    cache_read_tokens: 0,
    cache_write_tokens: 0,
    total_tokens: 0,
    credits_minor: 0,
    saved_tokens: 0,
    user_share_tokens: 0,
    user_share_minor: 0,
  };
}

// The API returns only days with data; the chart still needs one bucket per
// calendar day in the selected range. Dates are UTC on both sides.
function buildUsageSeries(rows: UsageDailyRow[], days: number): UsageDailyRow[] {
  const byDate = new Map(rows.map((row) => [row.date, row]));
  const today = new Date();
  const end = Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), today.getUTCDate());
  const series: UsageDailyRow[] = [];
  for (let offset = days - 1; offset >= 0; offset -= 1) {
    const date = new Date(end - offset * 86400000).toISOString().slice(0, 10);
    series.push(byDate.get(date) ?? zeroUsageDay(date));
  }
  return series;
}

function UsageCard({ label, value, unit, children }: { label: string; value: string; unit?: string; children?: ReactNode }) {
  return (
    <article className="usage-card">
      <p className="usage-card__label">{label}</p>
      <div className="usage-card__value">
        <strong>{value}</strong>
        {unit && <span>{unit}</span>}
      </div>
      <div className="usage-card__notes">{children}</div>
    </article>
  );
}

const BILLING_DETAIL_TABS = [
  { key: 'tokens' as const, label: 'Tokens' },
  { key: 'sandboxes' as const, label: 'Sandboxes' },
];

type BillingDetailTab = (typeof BILLING_DETAIL_TABS)[number]['key'];

function billingTime(value: string | null) {
  if (!value) return '—';
  return new Date(value).toLocaleString('zh-CN', { hour12: false });
}

function sandboxPriceText(prices: Record<string, unknown> | null) {
  if (!prices) return '—';
  const entries = Object.entries(prices);
  if (entries.length === 0) return '—';
  return entries
    .map(([key, value]) => {
      const text = typeof value === 'number'
        ? String(value)
        : typeof value === 'string'
          ? value
          : JSON.stringify(value);
      return `${key} ${text}`;
    })
    .join(' · ');
}

// The ledger records one settled amount per lease. When that lease carried a
// single model the backend returns exact credits; otherwise credits are null and
// we show a dash. When credits exist but no input/output split was reported, a
// blended credits-per-million rate is still factually derivable and is labelled
// as such instead of pretending to be an input or output price.
function effectiveTokenPrice(row: BillingTokenRow) {
  const inRate = row.effective_input_credits_per_million;
  const outRate = row.effective_output_credits_per_million;
  if (inRate != null || outRate != null) {
    const input = inRate != null ? inRate.toFixed(2) : '—';
    const output = outRate != null ? outRate.toFixed(2) : '—';
    return { text: `${input} in · ${output} out`, note: 'credits / 1M tokens' };
  }
  if (row.credits_minor != null && row.total_tokens > 0) {
    const blended = ((row.credits_minor / 1000) * 1_000_000) / row.total_tokens;
    return { text: `${blended.toFixed(2)} 混合`, note: 'credits / 1M tokens' };
  }
  return { text: '—', note: '' };
}

// 扣费明细: per-model token spend and per-sandbox charges for the user's own
// organization. It complements (not replaces) the Usage dashboard: this section
// is about money, the Usage section is about token/analytics trends.
function BillingDetailsSection({ token, orgId }: { token: string; orgId: string }) {
  const [days, setDays] = useState(30);
  const [details, setDetails] = useState<BillingDetails | null>(null);
  const [tab, setTab] = useState<BillingDetailTab>('tokens');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [reload, setReload] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError('');
    api.billingDetails(token, orgId, days)
      .then((data) => {
        if (!cancelled) setDetails(data);
      })
      .catch((err) => {
        if (!cancelled) setError(err instanceof Error ? err.message : '加载扣费明细失败');
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => { cancelled = true; };
  }, [token, orgId, days, reload]);

  const summary = details?.summary;
  const tokenRows = details?.tokens ?? [];
  const sandboxRows = details?.sandboxes ?? [];
  const totalMinor = summary?.total_minor ?? 0;
  const averageSandboxCredits = summary && summary.sandbox_count > 0
    ? summary.sandbox_credits / summary.sandbox_count
    : 0;
  const rangeLabel = details
    ? `${details.range_start.slice(0, 10)} → ${details.range_end.slice(0, 10)}`
    : `最近 ${days} 天`;

  return (
    <section className="section billing-detail" id="billing-details" aria-busy={loading}>
      <div className="section__head">
        <div><p className="kicker">BILLING · DETAIL</p><h2>扣费明细。</h2></div>
        <p className="section__intro">
          按模型和沙箱拆分的实际扣费：多少 token、多少沙箱、各扣了多少 credits 和人民币。
          账本按结算总额记账，因此同一结算合并多个模型时不做估算，直接显示 —。
        </p>
      </div>

      <div className="usage-toolbar">
        <div className="usage-range" role="tablist" aria-label="扣费明细时间范围">
          {USAGE_RANGES.map((range) => (
            <button
              key={range}
              type="button"
              role="tab"
              aria-selected={days === range}
              className={`usage-range__btn ${days === range ? 'is-active' : ''}`}
              onClick={() => setDays(range)}
            >
              {range} 天
            </button>
          ))}
        </div>
        <div className="usage-toolbar__meta">
          <span>UTC 日聚合</span>
          <span>{rangeLabel}</span>
        </div>
      </div>

      {error && (
        <div className="form-error usage-error" role="alert">
          <span>{error}</span>
          <button className="btn btn--ghost btn--small" type="button" onClick={() => setReload((n) => n + 1)}>重试</button>
        </div>
      )}

      <div className={`usage-cards usage-cards--four ${loading ? 'is-loading' : ''}`}>
        <UsageCard label="总扣费" value={details ? minorCredits(totalMinor) : '—'} unit="credits">
          <span>{details ? `≈ ${minorYuan(totalMinor)} · 1 credit = ¥0.01` : '正在汇总账本…'}</span>
        </UsageCard>
        <UsageCard label="Token 扣费" value={details && summary ? minorCredits(summary.token_minor) : '—'} unit="credits">
          <span>{details && summary ? `≈ ${minorYuan(summary.token_minor)} · ${credits(summary.total_tokens)} tokens` : '按模型拆分的 token 扣费'}</span>
        </UsageCard>
        <UsageCard label="沙箱扣费" value={details && summary ? minorCredits(summary.sandbox_minor) : '—'} unit="credits">
          <span>{details && summary ? `≈ ${minorYuan(summary.sandbox_minor)} · 按运行秒数计费` : '沙箱运行秒数折算'}</span>
        </UsageCard>
        <UsageCard label="沙箱数量" value={details && summary ? credits(summary.sandbox_count) : '—'} unit="个">
          <span>{details && summary ? `平均 ${averageSandboxCredits.toFixed(2)} credits / 沙箱` : '区间内已计费沙箱'}</span>
        </UsageCard>
      </div>

      <div className="billing-detail__tabs" role="tablist" aria-label="扣费明细分类">
        {BILLING_DETAIL_TABS.map((item) => {
          const count = item.key === 'tokens' ? tokenRows.length : (summary?.sandbox_count ?? 0);
          return (
            <button
              key={item.key}
              type="button"
              role="tab"
              aria-selected={tab === item.key}
              className={`billing-detail__tab ${tab === item.key ? 'is-active' : ''}`}
              onClick={() => setTab(item.key)}
            >
              {item.label} <span>{credits(count)}</span>
            </button>
          );
        })}
      </div>

      {tab === 'tokens' ? (
        <section className="usage-panel usage-panel--recent billing-detail__panel">
          <div className="usage-panel__head">
            <div><p className="kicker">TOKENS BY MODEL</p><h3>按模型汇总</h3></div>
            <span className="tag">{credits(summary?.requests ?? 0)} 次请求</span>
          </div>
          <div className="usage-table billing-table billing-table--tokens">
            <div className="usage-table__head">
              <span>模型</span><span>请求</span><span>Input</span><span>Output</span><span>Cache R/W</span><span>Tokens</span><span>Credits</span><span>¥</span><span>有效单价</span>
            </div>
            {loading ? (
              <div className="usage-table__empty">正在加载 token 扣费…</div>
            ) : tokenRows.length === 0 ? (
              <div className="usage-table__empty">
                这个区间还没有 token 扣费记录。token 明细来自已结算请求的用量上报，早于接入期的请求不会出现。
              </div>
            ) : tokenRows.map((row) => {
              const effective = effectiveTokenPrice(row);
              return (
                <div className="usage-table__row" key={row.model || 'unknown'}>
                  <span className="billing-model" title={row.model}>{row.model || '未知模型'}</span>
                  <span className="mono">{credits(row.requests)}</span>
                  <span className="mono">{tokens(row.input_tokens)}</span>
                  <span className="mono">{tokens(row.output_tokens)}</span>
                  <span className="usage-tokens">
                    <strong>{tokens(row.cache_read_tokens)}</strong>
                    <span className="usage-sub">{tokens(row.cache_write_tokens)} write</span>
                  </span>
                  <span className="mono">{tokens(row.total_tokens)}</span>
                  <span className="usage-cost">
                    <strong title={row.credits_minor == null ? '该结算合并了多个模型，无法精确拆分' : undefined}>
                      {row.credits_minor == null ? '—' : minorCredits(row.credits_minor)}
                    </strong>
                  </span>
                  <span className="mono">{row.credits_minor == null ? '—' : minorYuan(row.credits_minor)}</span>
                  <span className="billing-effective">
                    <strong>{effective.text}</strong>
                    {effective.note && <span className="usage-sub">{effective.note}</span>}
                  </span>
                </div>
              );
            })}
          </div>
        </section>
      ) : (
        <section className="usage-panel usage-panel--recent billing-detail__panel">
          <div className="usage-panel__head">
            <div><p className="kicker">SANDBOX CHARGES</p><h3>沙箱扣费</h3></div>
            <span className="tag">
              {credits(summary?.sandbox_count ?? 0)} 个沙箱
              {sandboxRows.length < (summary?.sandbox_count ?? 0) ? ` · 明细仅显示前 ${sandboxRows.length} 条` : ''}
            </span>
          </div>
          <div className="usage-table billing-table billing-table--sandboxes">
            <div className="usage-table__head">
              <span>Sandbox</span><span>产品</span><span>用户</span><span>时长</span><span>CPU / RAM / 磁盘</span><span>Credits</span><span>¥</span><span>单价</span>
            </div>
            {loading ? (
              <div className="usage-table__empty">正在加载沙箱扣费…</div>
            ) : sandboxRows.length === 0 ? (
              <div className="usage-table__empty">
                这个区间还没有沙箱扣费记录。沙箱按实际运行秒数计费，首次产生沙箱用量后会出现在这里。
              </div>
            ) : sandboxRows.map((row: BillingSandboxRow) => (
              <div className="usage-table__row" key={row.id}>
                <span className="billing-sandbox">
                  <strong className="mono">{row.sandbox_id}</strong>
                  <span className="usage-sub">
                    {row.provider || 'provider —'}
                    {row.provider_sandbox_id ? ` · ${row.provider_sandbox_id}` : ''}
                  </span>
                  <span className="usage-sub">{billingTime(row.started_at)}</span>
                </span>
                <span title={row.product}>{PRODUCT_LABELS[row.product] ?? row.product}</span>
                <span className="billing-user" title={row.user_id || undefined}>{row.user_id || '—'}</span>
                <span className="usage-tokens">
                  <strong>{row.billable_hours.toFixed(2)} h</strong>
                  <span className="usage-sub">{credits(row.billable_seconds)}s 计费</span>
                </span>
                <span className="billing-resources">
                  <span>CPU {credits(row.cpu_seconds)}s</span>
                  <span className="usage-sub">RAM {credits(row.memory_mib_seconds)} MiB·s</span>
                  <span className="usage-sub">Disk {credits(row.disk_mib_seconds)} MiB·s</span>
                </span>
                <span className="usage-cost"><strong>{minorCredits(row.credits_minor)}</strong></span>
                <span className="mono">{minorYuan(row.credits_minor)}</span>
                <span className="billing-unit" title={sandboxPriceText(row.unit_prices)}>{sandboxPriceText(row.unit_prices)}</span>
              </div>
            ))}
          </div>
        </section>
      )}

      <p className="billing-detail__note">
        Credits 与 ¥ 来自 billing.ledger；沙箱明细来自 billing.sandbox_usage（自启用后开始记录）。
        同一笔结算包含多个模型时 credits 显示 —，这是账本口径的如实呈现，不是漏记。
      </p>
    </section>
  );
}

function UsageDashboard({ token, orgId }: { token: string; orgId: string }) {
  const [days, setDays] = useState(30);
  const [summary, setSummary] = useState<UsageTotals | null>(null);
  const [sessions, setSessions] = useState<UsageSessionRow[]>([]);
  const [sessionCount, setSessionCount] = useState(0);
  const [sessionHitRate, setSessionHitRate] = useState(0);
  const [daily, setDaily] = useState<UsageDailyRow[]>([]);
  const [recent, setRecent] = useState<UsageRecentRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [reload, setReload] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError('');
    Promise.all([
      api.usageSummary(token, orgId, days),
      api.usageDaily(token, orgId, days),
      api.usageRecent(token, orgId, 50),
    ])
      .then(([summaryData, dailyData, recentData]) => {
        if (cancelled) return;
        setSummary(summaryData.totals);
        setSessions(summaryData.sessions ?? []);
        setSessionCount(summaryData.session_count ?? 0);
        setSessionHitRate(summaryData.session_hit_rate ?? 0);
        setDaily(dailyData.days ?? []);
        setRecent(recentData.rows ?? []);
      })
      .catch((err) => {
        if (!cancelled) setError(err instanceof Error ? err.message : '加载用量失败');
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => { cancelled = true; };
  }, [token, orgId, days, reload]);

  const totals = summary ?? EMPTY_USAGE_TOTALS;
  const series = useMemo(() => buildUsageSeries(daily, days), [daily, days]);
  const maxDailyTokens = series.reduce((max, row) => Math.max(max, row.total_tokens), 1);
  const hasDailyData = series.some((row) => row.total_tokens > 0);

  const models = useMemo(() => {
    const grouped = new Map<string, { model: string; requests: number; tokens: number; creditsMinor: number; cacheRead: number; input: number }>();
    for (const row of recent) {
      const key = row.model?.trim() || 'unknown';
      const entry = grouped.get(key) ?? { model: key, requests: 0, tokens: 0, creditsMinor: 0, cacheRead: 0, input: 0 };
      entry.requests += row.requests || 1;
      entry.tokens += row.total_tokens;
      entry.creditsMinor += row.credits_minor;
      entry.cacheRead += row.cache_read_tokens;
      entry.input += row.input_tokens;
      grouped.set(key, entry);
    }
    return Array.from(grouped.values()).sort((a, b) => b.tokens - a.tokens).slice(0, 5);
  }, [recent]);
  const maxModelTokens = models.reduce((max, model) => Math.max(max, model.tokens), 1);

  return (
    <section className="section usage" id="usage" aria-busy={loading}>
      <div className="section__head">
        <div><p className="kicker">USAGE · ANALYTICS</p><h2>用量与节省。</h2></div>
        <p className="section__intro">
          模型请求、token、缓存命中和积分消耗，按日汇总。压缩节省会和你分成的部分一起展示；1 credit = ¥0.01。
        </p>
      </div>

      <div className="usage-toolbar">
        <div className="usage-range" role="tablist" aria-label="用量时间范围">
          {USAGE_RANGES.map((range) => (
            <button
              key={range}
              type="button"
              role="tab"
              aria-selected={days === range}
              className={`usage-range__btn ${days === range ? 'is-active' : ''}`}
              onClick={() => setDays(range)}
            >
              {range} 天
            </button>
          ))}
        </div>
        <div className="usage-toolbar__meta">
          <span>UTC 日聚合</span>
          <span>{loading ? '正在刷新…' : `最近 ${days} 天`}</span>
        </div>
      </div>

      {error && (
        <div className="form-error usage-error" role="alert">
          <span>{error}</span>
          <button className="btn btn--ghost btn--small" type="button" onClick={() => setReload((n) => n + 1)}>重试</button>
        </div>
      )}

      <div className={`usage-cards ${loading ? 'is-loading' : ''}`}>
        <UsageCard label="Requests" value={credits(totals.requests)}>
          <span>{credits(totals.input_tokens)} in · {credits(totals.output_tokens)} out</span>
        </UsageCard>
        <UsageCard label="Total tokens" value={credits(totals.total_tokens)}>
          <span>{credits(totals.cache_read_tokens)} cache read · {credits(totals.cache_write_tokens)} write</span>
        </UsageCard>
        <UsageCard label="Cache hit rate" value={percent(totals.cache_hit_rate)}>
          <span>全部请求 · cache_read / (input + cache_read)</span>
        </UsageCard>
        <UsageCard label="Session cache hit" value={sessionCount > 0 ? percent(sessionHitRate) : '—'}>
          <span>{credits(sessionCount)} 个会话 · 有 cache_read 的请求</span>
        </UsageCard>
        <UsageCard label="Spend" value={minorCredits(totals.credits_minor)} unit="credits">
          <span>≈ {minorYuan(totals.credits_minor)} · 上游成本 {minorCredits(totals.upstream_cost_minor)} credits</span>
        </UsageCard>
        <UsageCard label="Saved / 分给你" value={credits(totals.saved_tokens)} unit="tokens saved">
          <span>分给你 {credits(totals.user_share_tokens)} tokens · {minorCredits(totals.user_share_minor)} credits</span>
          <span>摘要成本 {minorCredits(totals.summary_cost_minor)} credits</span>
        </UsageCard>
      </div>

      <div className="usage-panels">
        <section className="usage-panel usage-panel--chart">
          <div className="usage-panel__head">
            <div><p className="kicker">DAILY TOKENS</p><h3>每日 token 用量</h3></div>
            <div className="usage-legend">
              {CHART_SEGMENTS.map((segment) => (
                <span className="usage-legend__item" key={segment.key}>
                  <span className={`usage-legend__swatch usage-legend__swatch--${segment.key}`} />
                  {segment.label}
                </span>
              ))}
            </div>
          </div>
          <div className="usage-chart" role="img" aria-label={`最近 ${days} 天的 input、output 与 cache token 堆叠柱状图`}>
            <div className="usage-chart__plot">
              {series.map((row) => {
                const height = row.total_tokens === 0 ? 0 : Math.max(3, (row.total_tokens / maxDailyTokens) * 100);
                const title = `${row.date} · ${credits(row.total_tokens)} tokens · ${credits(row.input_tokens)} in / ${credits(row.output_tokens)} out / ${credits(row.cache_read_tokens)} cache read`;
                return (
                  <div className="usage-chart__column" key={row.date}>
                    <div
                      className={`usage-chart__bar ${row.total_tokens === 0 ? 'usage-chart__bar--empty' : ''}`}
                      style={row.total_tokens === 0 ? undefined : { height: `${height}%` }}
                      title={title}
                    >
                      {row.total_tokens > 0 && CHART_SEGMENTS.map((segment) => {
                        const value = row[segment.field];
                        return value > 0 ? (
                          <span
                            key={segment.key}
                            className={`usage-chart__segment usage-chart__segment--${segment.key}`}
                            style={{ height: `${(value / row.total_tokens) * 100}%` }}
                          />
                        ) : null;
                      })}
                    </div>
                  </div>
                );
              })}
            </div>
            <div className="usage-chart__axis">
              {series.map((row, index) => {
                const step = Math.max(1, Math.ceil(series.length / 8));
                const show = index === 0 || index === series.length - 1 || index % step === 0;
                return <span className="usage-chart__axis-label" key={row.date}>{show ? row.date.slice(5) : ''}</span>;
              })}
            </div>
          </div>
          {!hasDailyData && <p className="usage-empty-note">{loading ? '正在加载每日用量…' : '这个区间还没有 token 用量。'}</p>}
        </section>

        <section className="usage-panel usage-panel--models">
          <div className="usage-panel__head">
            <div><p className="kicker">MODEL ACTIVITY</p><h3>模型活跃度</h3></div>
            <span className="tag">最近 {recent.length} 条</span>
          </div>
          {models.length === 0 ? (
            <p className="usage-empty-note">{loading ? '正在加载模型记录…' : '还没有模型调用记录。'}</p>
          ) : (
            <ul className="usage-models">
              {models.map((model) => {
                const cacheRate = model.input + model.cacheRead > 0 ? model.cacheRead / (model.input + model.cacheRead) : 0;
                return (
                  <li className="usage-model" key={model.model}>
                    <div className="usage-model__head">
                      <span className="usage-model__name">{model.model}</span>
                      <span className="usage-model__tokens">{credits(model.tokens)} tokens</span>
                    </div>
                    <div className="usage-model__bar"><span style={{ width: `${Math.max(4, (model.tokens / maxModelTokens) * 100)}%` }} /></div>
                    <div className="usage-model__foot">
                      <span>{model.requests} req</span>
                      <span>{minorCredits(model.creditsMinor)} credits</span>
                      <span>{percent(cacheRate)} cache</span>
                    </div>
                  </li>
                );
              })}
            </ul>
          )}
        </section>
      </div>

      <section className="usage-panel usage-panel--sessions">
        <div className="usage-panel__head">
          <div><p className="kicker">SESSION CACHE</p><h3>会话缓存</h3></div>
          <span className="tag">Top {sessions.length} · 按 token 排序</span>
        </div>
        <div className="usage-table billing-table billing-table--sessions">
          <div className="usage-table__head">
            <span>会话</span><span>请求</span><span>Tokens</span><span>Cache read</span><span>命中率*</span><span>最后活跃</span>
          </div>
          {sessions.length === 0 ? (
            <div className="usage-table__empty">
              {loading ? '正在加载会话…' : '这个区间还没有带 session header 的请求（新请求开始记录后显示）。'}
            </div>
          ) : sessions.map((row) => (
            <div className="usage-table__row" key={row.session_id}>
              <span className="usage-model-name mono" title={row.session_id}>
                {row.session_id.length > 28 ? `${row.session_id.slice(0, 28)}…` : row.session_id}
              </span>
              <span className="mono">{credits(row.requests)}</span>
              <span className="usage-tokens">
                <strong>{credits(row.total_tokens)}</strong>
                <span className="usage-sub">{credits(row.input_tokens)} in · {credits(row.output_tokens)} out</span>
              </span>
              <span className="mono">{credits(row.cache_read_tokens)}</span>
              <span className="mono">{row.cache_read_tokens > 0 ? percent(row.cache_hit_rate) : '—'}</span>
              <span className="usage-table__time">{new Date(row.last_seen).toLocaleString('zh-CN', { hour12: false })}</span>
            </div>
          ))}
        </div>
        <p className="admin-note">* 命中率只统计该会话里有 cache_read 的请求，避免首轮冷启动把会话拉低。</p>
      </section>

      <section className="usage-panel usage-panel--recent">
        <div className="usage-panel__head">
          <div><p className="kicker">RECENT REQUESTS</p><h3>最近请求</h3></div>
          <span className="tag">最近 {recent.length} 条</span>
        </div>
        <div className="usage-table">
          <div className="usage-table__head">
            <span>时间</span><span>模型</span><span>请求</span><span>Tokens</span><span>缓存命中</span><span>花费</span><span>压缩与分成</span>
          </div>
          {recent.length === 0 ? (
            <div className="usage-table__empty">{loading ? '正在加载请求…' : '这个区间还没有结算请求。'}</div>
          ) : recent.map((row) => (
            <div className="usage-table__row" key={`${row.request_id}-${row.created_at}`}>
              <span className="usage-table__time">{new Date(row.created_at).toLocaleString('zh-CN', { hour12: false })}</span>
              <span className="usage-model-name">{row.model || '—'}</span>
              <span className="mono usage-request" title={row.request_id}>{row.request_id.slice(0, 12)}{row.request_id.length > 12 ? '…' : ''}</span>
              <span className="usage-tokens">
                <strong>{credits(row.total_tokens)}</strong>
                <span className="usage-sub">{credits(row.input_tokens)} in · {credits(row.output_tokens)} out</span>
              </span>
              <span className="mono">{percent(row.cache_hit_rate)}</span>
              <span className="usage-cost">
                <strong>{minorCredits(row.credits_minor)}</strong>
                <span className="usage-sub">{minorYuan(row.credits_minor)}</span>
              </span>
              <span className="usage-compression">
                {(row.compression_applied || row.original_input_tokens > 0) ? (
                  <>
                    <span className="mono">{credits(row.original_input_tokens)} → {credits(row.compressed_input_tokens)}</span>
                    <span className="usage-sub">+{credits(row.saved_tokens)} saved · 分给你 {credits(row.user_share_tokens)} tokens / {minorCredits(row.user_share_minor)} credits</span>
                  </>
                ) : <span className="muted">—</span>}
              </span>
            </div>
          ))}
        </div>
      </section>
    </section>
  );
}

function CausalMemory({ token, orgId }: { token: string; orgId: string }) {
  const [mcpToken, setMcpToken] = useState('');
  const [mcpUrl, setMcpUrl] = useState(MEMORY_MCP_URL);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [copied, setCopied] = useState(false);

  async function issue() {
    setBusy(true);
    setError('');
    try {
      const issued = await api.issueCausalToken(token, orgId);
      setMcpToken(issued.token);
      if (issued.mcp_url) setMcpUrl(issued.mcp_url);
    } catch (err) {
      setError(err instanceof Error ? err.message : '生成失败');
    } finally {
      setBusy(false);
    }
  }

  async function copy() {
    if (!mcpToken) return;
    await navigator.clipboard.writeText(mcpToken);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  }

  return (
    <section className="causal glass" id="causal-memory">
      <div>
        <p className="kicker">MEMORY · MCP</p>
        <h2>把长期记忆接到 Agent</h2>
        <p className="muted">
          生成一个 MCP Token，把下面的地址加到 Claude Code、Cursor 等支持 MCP 的客户端，
          Agent 就能跨会话记住项目、偏好和结论。调用和存储从组织余额扣费。
        </p>
        <CopyField label="MCP 地址" value={mcpUrl} hint={mcpToken ? undefined : '以生成 Token 后显示的地址为准'} />
      </div>
      <div className="causal__action">
        {mcpToken ? (
          <>
            <p className="kicker">TOKEN · 只显示一次</p>
            <code className="causal__token">{mcpToken}</code>
            <button className="btn btn--ghost btn--small" onClick={() => void copy()}>{copied ? '已复制' : '复制 token'}</button>
            <p className="muted causal__hint">在 MCP 客户端里配置 Authorization: Bearer &lt;token&gt;</p>
            <pre className="snippet__code causal__config"><code>{`claude mcp add --transport http roost-memory \\
  ${mcpUrl} \\
  --header "Authorization: Bearer ${mcpToken}"`}</code></pre>
          </>
        ) : (
          <button className="btn btn--ink" onClick={() => void issue()} disabled={busy}>{busy ? '生成中…' : '生成 MCP Token'}</button>
        )}
        {error && <p className="form-error">{error}</p>}
      </div>
    </section>
  );
}

function Recharge({ current, alwaysOnUntil, onBuy, busy }: { current: string; alwaysOnUntil?: string | null; onBuy: (sku: string, amountCents?: number) => void; busy: string }) {
  const addon = current === 'pro' || current === 'max';
  const [customYuan, setCustomYuan] = useState('200');
  const customValue = Number(customYuan);
  const customCents = Math.round(customValue * 100);
  const customValid = Number.isFinite(customValue) && customCents >= 1000 && customCents <= 500000;
  return (
    <section className="section" id="recharge">
      <div className="section__head">
        <div><p className="kicker">TOP UP</p><h2>单独充值积分。</h2></div>
        <p className="section__intro">不绑定套餐，按需购买；支付成功后自动到账，Sandbox、Market、dsalpha、Causal Memory 共用同一份余额。</p>
      </div>
      <div className="recharge-grid">
        {CREDIT_PACKS.map((pack) => (
          <article className="recharge-card" key={pack.sku}>
            <p className="recharge-card__credits">{pack.label}</p>
            <div className="recharge-card__price"><strong>{pack.price}</strong><span>一次性</span></div>
            <p className="recharge-card__note">{pack.note}</p>
            <button className="btn btn--ink btn--wide" disabled={busy === pack.sku} onClick={() => onBuy(pack.sku)}>
              {busy === pack.sku ? '创建订单…' : '立即充值'}
            </button>
          </article>
        ))}
        <article className="recharge-card recharge-card--custom">
          <p className="recharge-card__credits">自定义金额</p>
          <div className="recharge-card__amount">
            <span>¥</span>
            <input
              inputMode="decimal"
              value={customYuan}
              onChange={(e) => setCustomYuan(e.target.value.replace(/[^0-9.]/g, ''))}
              aria-label="充值金额（元）"
            />
          </div>
          <p className="recharge-card__note">
            ¥10–¥5000；1 credit = ¥0.01，到账 {customValid ? credits(customCents) : '—'} credits。
          </p>
          <button
            className="btn btn--ink btn--wide"
            disabled={!customValid || busy === 'credit_custom'}
            onClick={() => onBuy('credit_custom', customCents)}
          >
            {busy === 'credit_custom' ? '创建订单…' : customValid ? `充值 ¥${customValue}` : '请输入 ¥10–¥5000'}
          </button>
        </article>
        {addon && (
          <article className="recharge-card">
            <p className="recharge-card__credits">Always-on 沙箱</p>
            <div className="recharge-card__price"><strong>¥49</strong><span>每台 / 30 天</span></div>
            <p className="recharge-card__note">
              {alwaysOnUntil
                ? `已开通至 ${new Date(alwaysOnUntil).toLocaleDateString('zh-CN')}，可继续续期。`
                : '常驻沙箱，不占普通沙箱配额。'}
            </p>
            <button className="btn btn--ink btn--wide" disabled={busy === 'addon_always_on_30d'} onClick={() => onBuy('addon_always_on_30d')}>
              {busy === 'addon_always_on_30d' ? '创建订单…' : '开通 / 续期'}
            </button>
          </article>
        )}
      </div>
    </section>
  );
}

function Plans({ plans, current, alwaysOnUntil, onBuy, busy }: { plans: Plan[]; current: string; alwaysOnUntil?: string | null; onBuy: (sku: string) => void; busy: string }) {
  const visible = plans.filter((plan) => plan.product === 'dsalpha' && plan.code !== 'dsalpha_free');
  return (
    <section className="section" id="plans">
      <div className="section__head">
        <div>
          <p className="kicker">DSALPHA PLANS</p>
          <h2>DSAlpha 套餐。</h2>
        </div>
        <p className="section__intro">7 天免费试用；首月 5 折；年付 85 折。套餐额度按月发放，超出后可用通用积分。</p>
      </div>
      <div className="plan-grid">
        {visible.map((plan) => {
          const featured = plan.code === 'dsalpha_pro';
          const monthly = plan.price_cents;
          const annual = Math.round(monthly * 12 * (10000 - plan.annual_discount_bps) / 10000);
          const firstMonth = Math.round(monthly * (10000 - plan.first_month_discount_bps) / 10000);
          const trialSku = `trial_${plan.code}_7d`;
          const monthlySku = `plan_${plan.code}_30d`;
          const annualSku = `plan_${plan.code}_365d`;
          return (
            <article className={`plan-card ${featured ? 'plan-card--featured' : ''}`} key={plan.code}>
              {featured && <span className="plan-card__badge">最受欢迎</span>}
              <div className="plan-card__head">
                <h3>{plan.name.replace('DSAlpha ', '')}</h3>
                <span className="tag">{plan.trial_days > 0 ? `${plan.trial_days} 天试用` : '月付'}</span>
              </div>
              <div className="plan-card__price">
                <strong>{yuan(monthly)}</strong>
                <span>/ 月</span>
              </div>
              <p className="muted">
                <s>{yuan(firstMonth)}</s> 首月 · 年付 {yuan(annual)}
              </p>
              <p className="muted">{credits(plan.included_credits)} credits / 月 · DSAlpha 专用</p>
              <ul className="plan-list">
                <li><span>工作区</span><span>{String(plan.features.workspaces ?? '—')}</span></li>
                <li><span>定时任务</span><span>{String(plan.features.scheduled_tasks ?? '—')}</span></li>
                <li><span>BYOK</span><span>{plan.features.byok ? '支持' : '—'}</span></li>
                <li><span>模型档位</span><span>tier {String(plan.features.model_tier ?? 0)}</span></li>
              </ul>
              <button className={`btn ${featured ? 'btn--accent' : 'btn--ink'} btn--wide`} disabled={busy === monthlySku || current === plan.code} onClick={() => onBuy(monthlySku)}>
                {current === plan.code ? '当前套餐' : busy === monthlySku ? '创建订单…' : `月付 ${yuan(firstMonth)}（首月）`}
              </button>
              <button className="btn btn--ghost btn--wide" disabled={busy === annualSku} onClick={() => onBuy(annualSku)}>
                {busy === annualSku ? '创建订单…' : `年付 ${yuan(annual)}`}
              </button>
              {plan.trial_days > 0 && (
                <button className="btn btn--ghost btn--wide" disabled={busy === trialSku} onClick={() => onBuy(trialSku)}>
                  {busy === trialSku ? '开通中…' : `开始 ${plan.trial_days} 天免费试用`}
                </button>
              )}
            </article>
          );
        })}
      </div>
    </section>
  );
}

function OrderModal({ token, order, onClose, onPaid }: { token: string; order: Order; onClose: () => void; onPaid: () => void }) {
  const [current, setCurrent] = useState(order);
  const [error, setError] = useState('');
  const [paying, setPaying] = useState(false);
  const isDev = Boolean(current.pay_url?.includes('/dev-pay'));

  useEffect(() => {
    if (current.status === 'fulfilled' || current.status === 'paid') return;
    const id = window.setInterval(async () => {
      try {
        const { order: fresh } = await api.getOrder(token, current.id);
        setCurrent(fresh);
        if (fresh.status === 'fulfilled' || fresh.status === 'paid') {
          window.clearInterval(id);
          onPaid();
        }
      } catch { /* keep polling */ }
    }, 2000);
    return () => window.clearInterval(id);
  }, [current.id, current.status, onPaid, token]);

  async function devPay() {
    setPaying(true);
    setError('');
    try {
      await api.devPay(token, current.id);
      const { order: fresh } = await api.getOrder(token, current.id);
      setCurrent(fresh);
      onPaid();
    } catch (err) {
      setError(err instanceof Error ? err.message : '支付失败');
    } finally {
      setPaying(false);
    }
  }

  const done = current.status === 'fulfilled' || current.status === 'paid';
  return (
    <div className="modal" role="dialog" aria-modal="true" aria-label="支付订单">
      <div className="modal__panel">
        <button className="modal__close" onClick={onClose} aria-label="关闭">×</button>
        <p className="kicker">PAYMENT</p>
        <h2>{done ? '已到账' : '完成支付'}</h2>
        <p className="muted">订单 {current.id.slice(0, 8)} · {yuan(current.amount_cents)}</p>
        <div className="order-summary">
          <div><span>SKU</span><strong>{current.sku}</strong></div>
          <div><span>到账</span><strong>{credits(current.credits)} credits</strong></div>
          <div><span>状态</span><strong>{current.status}</strong></div>
        </div>
        {done ? (
          <div className="pay-done">
            <span className="pay-done__mark">✓</span>
            <p>积分已进入组织钱包，所有产品立即可用。</p>
            <button className="btn btn--ink btn--wide" onClick={onClose}>返回控制台</button>
          </div>
        ) : (
          <>
            {current.qr_url && <img className="qr" src={current.qr_url} alt="支付二维码" />}
            {!current.qr_url && isDev && (
              <p className="dev-note">本地开发模式：虎皮椒未配置，使用测试支付完成回调链路。</p>
            )}
            {error && <p className="form-error">{error}</p>}
            <div className="modal__actions">
              {isDev ? (
                <button className="btn btn--accent btn--wide" onClick={devPay} disabled={paying}>{paying ? '处理中…' : '模拟支付成功'}</button>
              ) : current.pay_url ? (
                <a className="btn btn--accent btn--wide" href={current.pay_url} target="_blank" rel="noreferrer">打开支付页面</a>
              ) : (
                <p className="muted">等待支付服务返回二维码…</p>
              )}
            </div>
          </>
        )}
      </div>
    </div>
  );
}


function RelayKeys({ token, orgId }: { token: string; orgId: string }) {
  const [keys, setKeys] = useState<RelayKey[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState('');
  const [created, setCreated] = useState('');
  const [confirming, setConfirming] = useState('');
  const [error, setError] = useState('');

  // Only the newest list request may write state, so a slow initial read
  // cannot overwrite the list refreshed after a create or revoke.
  const loadSeq = useRef(0);
  const load = useCallback(async () => {
    const seq = ++loadSeq.current;
    try {
      const data = await api.relayKeys(token, orgId);
      if (seq !== loadSeq.current) return;
      setKeys((data.tokens ?? []).filter((k) => !k.revoked_at));
      setError('');
    } catch (err) {
      if (seq !== loadSeq.current) return;
      setError(err instanceof Error ? `加载 API Key 失败：${err.message}` : '加载 API Key 失败');
    } finally {
      if (seq === loadSeq.current) setLoaded(true);
    }
  }, [token, orgId]);
  useEffect(() => { void load(); }, [load]);

  // Key mutations are mutually exclusive: one create or revoke at a time.
  async function create() {
    if (busy) return;
    setBusy('create'); setError(''); setConfirming('');
    try {
      const data = await api.createRelayKey(token, orgId);
      setCreated(data.token);
    } catch (err) {
      setError(err instanceof Error ? err.message : '创建 API Key 失败');
      setBusy('');
      return;
    }
    setBusy('');
    await load();
  }
  async function revoke(hash: string) {
    if (busy) return;
    setBusy(hash); setError('');
    try {
      await api.revokeRelayKey(token, orgId, hash);
    } catch (err) {
      setError(err instanceof Error ? err.message : '吊销失败');
      setBusy('');
      return;
    }
    setKeys((list) => list.filter((k) => k.token_hash !== hash));
    setConfirming('');
    setBusy('');
    await load();
  }

  return (
    <div className="keys-page">
      <section className="access glass" aria-labelledby="access-title">
        <div className="card-head">
          <div>
            <p className="kicker">ENDPOINTS · 接入地址</p>
            <h2 id="access-title">一个 Key，三种接法</h2>
          </div>
          <p className="section__intro">按你用的客户端选一个地址，填进它的 Base URL 字段，再填 API Key 即可。</p>
        </div>
        <EndpointList />
      </section>

      <section className="section glass panel" id="api-keys" aria-labelledby="keys-title">
        <div className="card-head">
          <div>
            <p className="kicker">API KEYS</p>
            <h2 id="keys-title">我的 API Key</h2>
          </div>
          <button className="btn btn--accent" onClick={() => void create()} disabled={!!busy || !loaded}>
            {busy === 'create' ? '创建中…' : '+ 创建 API Key'}
          </button>
        </div>
        <p className="muted keys-note">Key 消费组织共享余额。完整 Key 只在创建时显示一次，列表里只保留指纹；泄露或丢失请吊销后重建。</p>
        {error && <p className="form-error">{error}</p>}
        <div className="table table--keys">
          <div className="table__row table__row--head"><span>Key 指纹</span><span>创建时间</span><span>最近使用</span><span>状态</span><span /></div>
          {!loaded && <div className="table__empty">正在加载…</div>}
          {loaded && keys.length === 0 && !error && (
            <div className="table__empty table__empty--cta">
              <span>还没有 API Key，创建一个就能开始调用。</span>
              <button className="btn btn--accent btn--small" onClick={() => void create()} disabled={!!busy}>创建第一个 Key</button>
            </div>
          )}
          {keys.map((k) => (
            <div className="table__row" key={k.token_hash}>
              <span className="mono">rk-…{k.token_hash.slice(0, 10)}</span>
              <span>{new Date(k.created_at).toLocaleString('zh-CN', { hour12: false })}</span>
              <span>{k.last_used_at ? new Date(k.last_used_at).toLocaleString('zh-CN', { hour12: false }) : '从未使用'}</span>
              <span><span className="status status--paid">可用</span></span>
              <span className="table__actions">
                {confirming === k.token_hash ? (
                  <>
                    <button className="btn btn--danger btn--small" disabled={!!busy} onClick={() => void revoke(k.token_hash)}>
                      {busy === k.token_hash ? '吊销中…' : '确认吊销'}
                    </button>
                    <button className="btn btn--ghost btn--small" onClick={() => setConfirming('')}>取消</button>
                  </>
                ) : (
                  <button className="btn btn--ghost btn--small" disabled={!!busy} onClick={() => setConfirming(k.token_hash)}>吊销</button>
                )}
              </span>
            </div>
          ))}
        </div>
      </section>

      <section className="section glass panel" aria-labelledby="code-title">
        <div className="card-head">
          <div>
            <p className="kicker">EXAMPLES</p>
            <h2 id="code-title">接入示例</h2>
          </div>
          <a className="text-link" href={MODELS_URL} target="_blank" rel="noreferrer">查看全部模型与价格 →</a>
        </div>
        <SnippetTabs />
      </section>

      {created && <KeyCreatedModal apiKey={created} onClose={() => setCreated('')} />}
    </div>
  );
}

const ADMIN_RANGES = [7, 30, 90];

type AdminSpendField = 'relay' | 'market' | 'causal-memory' | 'dslpha';

const ADMIN_SPEND_PRODUCTS: Array<{ key: string; field: AdminSpendField; label: string; short: string }> = [
  { key: 'relay', field: 'relay', label: 'Relay · 模型中转', short: 'Relay' },
  { key: 'market', field: 'market', label: 'Market · 算力市场', short: 'Market' },
  { key: 'causal-memory', field: 'causal-memory', label: 'Causal Memory · 记忆', short: 'Memory' },
  { key: 'dsalpha', field: 'dslpha', label: 'dsalpha · 投研工作台', short: 'dsalpha' },
];

function zeroAdminDay(date: string): AdminOverviewDaily {
  return { date, relay: 0, market: 0, 'causal-memory': 0, dslpha: 0, tokens: 0, revenue_cents: 0 };
}

// The API returns only days with data; the chart needs one bucket per UTC
// calendar day in the selected range, matching the per-org usage dashboard.
function buildAdminSeries(rows: AdminOverviewDaily[], days: number): AdminOverviewDaily[] {
  const byDate = new Map(rows.map((row) => [row.date, row]));
  const today = new Date();
  const end = Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), today.getUTCDate());
  const series: AdminOverviewDaily[] = [];
  for (let offset = days - 1; offset >= 0; offset -= 1) {
    const date = new Date(end - offset * 86400000).toISOString().slice(0, 10);
    series.push(byDate.get(date) ?? zeroAdminDay(date));
  }
  return series;
}

function adminDaySpend(row: AdminOverviewDaily) {
  return row.relay + row.market + row['causal-memory'] + row.dslpha;
}

function AdminPanel({ token }: { token: string }) {
  const [days, setDays] = useState(30);
  const [overview, setOverview] = useState<AdminOverview | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [reload, setReload] = useState(0);

  const [q, setQ] = useState('');
  const [orgs, setOrgs] = useState<AdminOrg[]>([]);
  const [selected, setSelected] = useState<{ org: AdminOrg; billing: BillingView; ledger: Array<Record<string, unknown>>; orders: Order[] } | null>(null);
  const [delta, setDelta] = useState('');
  const [reason, setReason] = useState('运营调整');
  const [msg, setMsg] = useState('');
  const [orgError, setOrgError] = useState('');
  const [giftCredits, setGiftCredits] = useState('');
  const [giftNote, setGiftNote] = useState('运营赠送');
  const [giftKey, setGiftKey] = useState('');
  const [giftMsg, setGiftMsg] = useState('');
  const [giftBusy, setGiftBusy] = useState(false);
  const [referrals, setReferrals] = useState<AdminReferrals | null>(null);
  const [referralsError, setReferralsError] = useState('');

  useEffect(() => {
    let cancelled = false;
    setReferralsError('');
    api.adminReferrals(token, 20)
      .then((data) => { if (!cancelled) setReferrals(data); })
      .catch((err) => { if (!cancelled) setReferralsError(err instanceof Error ? err.message : '加载邀请记录失败'); });
    return () => { cancelled = true; };
  }, [token, reload]);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError('');
    api.adminOverview(token, days)
      .then((data) => { if (!cancelled) setOverview(data); })
      .catch((err) => { if (!cancelled) setError(err instanceof Error ? err.message : '加载平台总览失败'); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [token, days, reload]);

  async function search() {
    setOrgError('');
    try { const d = await api.adminOrgs(token, q); setOrgs(d.orgs ?? []); }
    catch (e) { setOrgError(e instanceof Error ? e.message : '查询失败'); }
  }
  async function open(orgId: string) {
    setOrgError(''); setMsg('');
    if (selected?.org.id !== orgId) {
      setGiftKey(''); setGiftCredits(''); setGiftMsg('');
    }
    try { setSelected(await api.adminOrg(token, orgId)); }
    catch (e) { setOrgError(e instanceof Error ? e.message : '加载失败'); }
  }
  async function adjust() {
    if (!selected) return;
    setOrgError(''); setMsg('');
    try {
      const r = await api.adminAdjust(token, selected.org.id, {
        delta_credits: Number(delta), reason,
        idempotency_key: `admin:${selected.org.id}:${Date.now()}`,
      });
      setMsg(`已调整，余额 ${r.balance_credits}`);
      await open(selected.org.id);
    } catch (e) { setOrgError(e instanceof Error ? e.message : '调整失败'); }
  }
  // Explicit gift with its own ledger reason and a client-generated idempotency
  // key. The key is kept across a failed attempt so a timeout retry is safe; it
  // is reset when the organization, amount or note changes.
  async function gift() {
    if (!selected) return;
    const amount = Number(giftCredits);
    if (!Number.isInteger(amount) || amount <= 0) {
      setOrgError('赠送积分必须是正整数');
      return;
    }
    setGiftBusy(true); setOrgError(''); setGiftMsg('');
    const key = giftKey || `gift:${selected.org.id}:${Date.now()}:${Math.random().toString(36).slice(2, 10)}`;
    setGiftKey(key);
    try {
      const r = await api.adminGift(token, selected.org.id, {
        credits: amount,
        note: giftNote,
        idempotency_key: key,
      });
      setGiftMsg(`已赠送 ${credits(amount)} credits（≈ ${yuan(amount)}），余额 ${credits(r.balance_credits)}。`);
      setGiftKey('');
      setGiftCredits('');
      await open(selected.org.id);
    } catch (e) {
      setOrgError(e instanceof Error ? e.message : '赠送失败');
    } finally {
      setGiftBusy(false);
    }
  }

  const totals = overview?.totals;
  const series = useMemo(() => buildAdminSeries(overview?.daily ?? [], days), [overview, days]);
  const maxSpend = series.reduce((max, row) => Math.max(max, adminDaySpend(row)), 0);
  const maxTokens = series.reduce((max, row) => Math.max(max, row.tokens), 0);
  const hasChartData = maxSpend > 0 || maxTokens > 0;
  const models = overview?.models ?? [];
  const topOrgs = overview?.top_orgs ?? [];
  const users = overview?.users ?? [];
  const recentOrders = overview?.recent_orders ?? [];
  const productCredits = (key: string) => totals?.product_spend_credits?.[key] ?? 0;
  const tokenTotals = totals?.tokens;
  const trackedRequests = tokenTotals?.tracked_requests ?? 0;
  const cacheRead = tokenTotals?.cache_read ?? 0;
  const cacheWrite = tokenTotals?.cache_write ?? 0;
  // A rate derived from zero events is not a real 0%: it means nothing was
  // tracked yet, or no request reported a cache read. Both states are shown
  // explicitly instead of a misleading rate.
  const hasTrackedUsage = trackedRequests > 0 || (tokenTotals?.total ?? 0) > 0;
  const hasCacheHitData = hasTrackedUsage && cacheRead > 0;
  const cacheRateValue = hasCacheHitData ? percent(tokenTotals?.cache_hit_rate ?? 0) : '—';

  return (
    <section className="section admin" id="admin" aria-busy={loading}>
      <div className="section__head">
        <div><p className="kicker">OPERATIONS · PLATFORM</p><h2>平台总览</h2></div>
        <p className="section__intro">
          所有组织的订单、积分消耗与 token 用量。数据按 UTC 日聚合，仅管理员可见；token 只从 usage_events
          开始记录后才有数据。
        </p>
      </div>

      <div className="usage-toolbar">
        <div className="usage-range" role="tablist" aria-label="平台总览时间范围">
          {ADMIN_RANGES.map((range) => (
            <button
              key={range}
              type="button"
              role="tab"
              aria-selected={days === range}
              className={`usage-range__btn ${days === range ? 'is-active' : ''}`}
              onClick={() => setDays(range)}
            >
              {range} 天
            </button>
          ))}
        </div>
        <div className="usage-toolbar__meta">
          <span>UTC 日聚合</span>
          <span>{loading ? '正在刷新…' : `最近 ${days} 天`}</span>
        </div>
      </div>

      {error && (
        <div className="form-error usage-error" role="alert">
          <span>{error}</span>
          <button className="btn btn--ghost btn--small" type="button" onClick={() => setReload((n) => n + 1)}>重试</button>
        </div>
      )}

      <div className={`admin-cards ${loading ? 'is-loading' : ''}`}>
        <UsageCard label="Organizations" value={credits(totals?.orgs ?? 0)}>
          <span>{credits(totals?.active_orgs ?? 0)} 个活跃组织</span>
          <span>区间内有消耗或 token 活动</span>
        </UsageCard>
        <UsageCard label="Users" value={credits(totals?.users ?? 0)}>
          <span>组织成员去重；有权限时取 auth.users</span>
        </UsageCard>
        <UsageCard label="Paid orders" value={credits(totals?.paid_orders ?? 0)}>
          <span>共 {credits(totals?.orders ?? 0)} 笔订单</span>
          <span>paid / fulfilled 计入营收</span>
        </UsageCard>
        <UsageCard label="Revenue" value={yuan(totals?.revenue_cents ?? 0)}>
          <span>已支付与已履约订单</span>
        </UsageCard>
        <UsageCard label="Credits consumed" value={credits(totals?.credits_consumed ?? 0)}>
          <span>仅 usage 扣费</span>
          <span>不含充值、赠送与运营调整</span>
        </UsageCard>
        <UsageCard label="Tokens" value={tokens(tokenTotals?.total ?? 0)}>
          <span>{tokens(tokenTotals?.input ?? 0)} in · {tokens(tokenTotals?.output ?? 0)} out</span>
          <span>{tokens(cacheRead)} cache read · {tokens(cacheWrite)} write</span>
          <span>{credits(trackedRequests)} tracked requests</span>
        </UsageCard>
        <UsageCard label="Cache hit rate" value={cacheRateValue}>
          <span>{tokens(cacheRead)} cache read · {tokens(cacheWrite)} cache write</span>
          <span>{credits(trackedRequests)} tracked requests</span>
          {!hasTrackedUsage ? (
            <span className="admin-warn">还没有 tracked usage_events；缓存命中率需要真实缓存读写数据</span>
          ) : !hasCacheHitData ? (
            <span className="admin-warn">无缓存命中数据（cache_read=0）</span>
          ) : (
            <span>cache_read / (input + cache_read)</span>
          )}
        </UsageCard>
      </div>

      {!loading && !hasTrackedUsage && (
        <p className="admin-note" role="status">
          当前区间没有 tracked usage_events，token 与缓存命中率都没有数据来源。缓存命中率 =
          cache_read / (input + cache_read)，需要 relay 上报真实的缓存读写；这时显示 0% 会误导，所以用 “—” 表示暂无数据。
        </p>
      )}
      {!loading && hasTrackedUsage && !hasCacheHitData && (
        <p className="admin-note" role="status">
          区间内有 {credits(trackedRequests)} 个 tracked 请求，但 cache_read=0，没有可计算的缓存命中率。
          这是“没有缓存读取”而不是“0% 命中”。
        </p>
      )}

      <div className="usage-panels">
        <section className="usage-panel usage-panel--chart">
          <div className="usage-panel__head">
            <div><p className="kicker">DAILY ACTIVITY</p><h3>每日消耗与 token</h3></div>
            <div className="usage-legend">
              {ADMIN_SPEND_PRODUCTS.map((product) => (
                <span className="usage-legend__item" key={product.key}>
                  <span className={`usage-legend__swatch admin-swatch--${product.key}`} />
                  {product.short}
                </span>
              ))}
              <span className="usage-legend__item"><span className="admin-legend__line" />Tokens</span>
            </div>
          </div>
          <div className="admin-chart" role="img" aria-label={`最近 ${days} 天产品积分消耗堆叠柱与 token 折线`}>
            <div className="admin-chart__plot">
              {series.map((row) => {
                const spend = adminDaySpend(row);
                const height = spend === 0 ? 0 : Math.max(2, (spend / maxSpend) * 100);
                return (
                  <div className="admin-chart__column" key={row.date}>
                    <div
                      className={`admin-chart__bar ${spend === 0 ? 'admin-chart__bar--empty' : ''}`}
                      style={spend === 0 ? undefined : { height: `${height}%` }}
                      title={`${row.date} · ${credits(spend)} credits · ${tokens(row.tokens)} tokens · ${yuan(row.revenue_cents)}`}
                    >
                      {spend > 0 && ADMIN_SPEND_PRODUCTS.map((product) => {
                        const value = row[product.field];
                        return value > 0 ? (
                          <span
                            key={product.key}
                            className={`admin-chart__segment admin-chart__segment--${product.key}`}
                            style={{ height: `${(value / spend) * 100}%` }}
                          />
                        ) : null;
                      })}
                    </div>
                  </div>
                );
              })}
              {maxTokens > 0 && (
                <svg className="admin-chart__line" viewBox="0 0 100 100" preserveAspectRatio="none" aria-hidden="true">
                  <polyline points={series.map((row, index) => `${((index + 0.5) / series.length) * 100},${100 - (row.tokens / maxTokens) * 100}`).join(' ')} />
                </svg>
              )}
            </div>
            <div className="usage-chart__axis">
              {series.map((row, index) => {
                const step = Math.max(1, Math.ceil(series.length / 8));
                const show = index === 0 || index === series.length - 1 || index % step === 0;
                return <span className="usage-chart__axis-label" key={row.date}>{show ? row.date.slice(5) : ''}</span>;
              })}
            </div>
          </div>
          {!hasChartData && <p className="usage-empty-note">{loading ? '正在加载每日数据…' : '这个区间还没有消耗或 token 活动。'}</p>}
          <p className="admin-note">
            {overview?.usage_tracking_start
              ? `Token 数据自 ${new Date(overview.usage_tracking_start).toLocaleDateString('zh-CN')} 起开始记录，更早区间只统计积分消耗。`
              : 'Token 数据尚未开始记录，当前只统计积分与订单。'}
          </p>
        </section>

        <section className="usage-panel usage-panel--products">
          <div className="usage-panel__head">
            <div><p className="kicker">PRODUCT SPEND</p><h3>产品消耗拆分</h3></div>
            <span className="tag">{credits(totals?.credits_consumed ?? 0)} credits</span>
          </div>
          <div className="admin-products">
            {ADMIN_SPEND_PRODUCTS.map((product) => {
              const value = productCredits(product.key);
              const consumed = totals?.credits_consumed ?? 0;
              const share = consumed > 0 ? value / consumed : 0;
              return (
                <article className="admin-product" key={product.key}>
                  <div className="admin-product__head">
                    <span className={`admin-product__dot admin-swatch--${product.key}`} />
                    <span>{product.label}</span>
                  </div>
                  <div className="admin-product__value">{credits(value)} <span>credits</span></div>
                  <div className="admin-product__bar">
                    <span className={`admin-swatch--${product.key}`} style={{ width: `${value > 0 ? Math.max(share * 100, 2) : 0}%` }} />
                  </div>
                  <p>{percent(share)} · 约占全部消耗</p>
                </article>
              );
            })}
          </div>
        </section>
      </div>

      <section className="usage-panel usage-panel--models">
        <div className="usage-panel__head">
          <div><p className="kicker">MODELS</p><h3>模型用量</h3></div>
          <span className="tag">{models.length} 个模型 · 按 token 排序</span>
        </div>
        <div className="admin-table-wrap">
          <table className="admin-table">
            <thead>
              <tr>
                <th>模型</th><th>请求</th><th>Input</th><th>Output</th>
                <th>Cache read</th><th>Cache write</th><th>Tokens</th><th>缓存命中</th>
              </tr>
            </thead>
            <tbody>
              {models.length === 0 ? (
                <tr>
                  <td colSpan={8} className="admin-table__empty">
                    {loading
                      ? '正在加载模型用量…'
                      : '还没有 tracked usage_events；缓存命中率需要真实缓存读写数据。'}
                  </td>
                </tr>
              ) : models.map((model) => (
                <tr key={model.model || 'unknown'}>
                  <td><span className="admin-table__org mono">{model.model || '未标注模型'}</span></td>
                  <td className="mono">{credits(model.requests)}</td>
                  <td className="mono">{tokens(model.input_tokens)}</td>
                  <td className="mono">{tokens(model.output_tokens)}</td>
                  <td className="mono">{tokens(model.cache_read_tokens)}</td>
                  <td className="mono">{tokens(model.cache_write_tokens)}</td>
                  <td className="mono">{tokens(model.total_tokens)}</td>
                  <td className="mono">
                    {model.cache_read_tokens > 0 ? percent(model.cache_hit_rate) : '—'}
                    <span className="admin-sub">
                      {model.cache_read_tokens > 0 ? `${tokens(model.cache_read_tokens)} cache read` : '无缓存命中数据（cache_read=0）'}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <p className="admin-note">
          仅统计 billing.usage_events 中已上报的请求。没有事件时 token 与缓存命中率都为空；
          cache_read=0 表示请求没有上报缓存读取，不是 0% 命中率。
        </p>
      </section>

      <section className="usage-panel usage-panel--top">
        <div className="usage-panel__head">
          <div><p className="kicker">ORGANIZATIONS / OWNERS</p><h3>组织 / 用户用量排行</h3></div>
          <span className="tag">按消耗积分排序 · Top {topOrgs.length}</span>
        </div>
        <div className="admin-table-wrap">
          <table className="admin-table">
            <thead>
              <tr>
                <th>组织</th><th>Owner</th><th>套餐</th><th>余额</th><th>订单</th><th>营收</th>
                <th>Relay</th><th>Market</th><th>Memory</th><th>dsalpha</th><th>Sandbox</th><th>Tokens</th><th>缓存命中</th>
              </tr>
            </thead>
            <tbody>
              {topOrgs.length === 0 ? (
                <tr><td colSpan={13} className="admin-table__empty">{loading ? '正在加载组织排行…' : '这个区间还没有组织活动。'}</td></tr>
              ) : topOrgs.map((org) => (
                <tr key={org.org_id}>
                  <td>
                    <span className="admin-table__org">{displayOrgName(org.name, org.owner_user_id)}</span>
                    <span className="admin-sub mono" title={org.org_id}>{org.org_id.slice(0, 8)}…</span>
                  </td>
                  <td>
                    <span className="admin-table__org">{org.owner_email || '—'}</span>
                    <span className="admin-sub mono" title={org.owner_user_id}>
                      {org.owner_user_id ? `${org.owner_user_id.slice(0, 8)}…` : '无 owner'}
                      {org.top_model ? ` · ${org.top_model}` : ''}
                    </span>
                  </td>
                  <td><span className="tag">{planLabel(org.plan_code)}</span></td>
                  <td className="mono">{credits(org.balance_credits)}</td>
                  <td className="mono">{credits(org.orders)}</td>
                  <td className="mono">{yuan(org.revenue_cents)}</td>
                  <td className="mono">{credits(org.relay_credits)}</td>
                  <td className="mono">{credits(org.market_credits)}</td>
                  <td className="mono">{credits(org.memory_credits)}</td>
                  <td className="mono">{credits(org.dslpha_credits)}</td>
                  <td className="mono">{credits(org.sandbox_credits)}</td>
                  <td>
                    <span className="mono">{tokens(org.total_tokens)}</span>
                    <span className="admin-sub">{tokens(org.input_tokens)} in · {tokens(org.output_tokens)} out</span>
                    <span className="admin-sub">{credits(org.requests)} req</span>
                  </td>
                  <td className="mono">
                    {org.cache_read_tokens > 0 ? percent(org.cache_hit_rate) : '—'}
                    <span className="admin-sub">
                      {tokens(org.cache_read_tokens)} read · {tokens(org.cache_write_tokens)} write
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <section className="usage-panel usage-panel--users">
        <div className="usage-panel__head">
          <div><p className="kicker">USERS · ACCOUNTABLE OWNERS</p><h3>用户用量</h3></div>
          <span className="tag">按 token 排序 · 最多 100 行</span>
        </div>
        <div className="admin-table-wrap">
          <table className="admin-table">
            <thead>
              <tr>
                <th>用户</th><th>组织</th><th>套餐</th><th>订单</th><th>营收</th>
                <th>Tokens</th><th>Top model</th>
                <th>Relay</th><th>Market</th><th>Memory</th><th>dsalpha</th><th>Sandbox</th>
              </tr>
            </thead>
            <tbody>
              {users.length === 0 ? (
                <tr><td colSpan={12} className="admin-table__empty">{loading ? '正在加载用户用量…' : '这个区间还没有可归因的用户用量。'}</td></tr>
              ) : users.map((user) => (
                <tr key={user.org_id}>
                  <td>
                    <span className="admin-table__org">{user.email || '—'}</span>
                    <span className="admin-sub mono" title={user.user_id}>
                      {user.user_id ? `${user.user_id.slice(0, 8)}…` : '无 owner'}
                    </span>
                  </td>
                  <td>
                    <span className="admin-table__org">{displayOrgName(user.org_name, user.user_id)}</span>
                    <span className="admin-sub mono" title={user.org_id}>{user.org_id.slice(0, 8)}…</span>
                  </td>
                  <td><span className="tag">{planLabel(user.plan_code)}</span></td>
                  <td className="mono">{credits(user.orders)}</td>
                  <td className="mono">{yuan(user.revenue_cents)}</td>
                  <td>
                    <span className="mono">{tokens(user.total_tokens)}</span>
                    <span className="admin-sub">{tokens(user.input_tokens)} in · {tokens(user.output_tokens)} out</span>
                    <span className="admin-sub">
                      {tokens(user.cache_read_tokens)} read · {tokens(user.cache_write_tokens)} write
                    </span>
                    <span className="admin-sub">
                      {credits(user.requests)} req · {user.cache_read_tokens > 0 ? percent(user.cache_hit_rate) + ' cache hit' : 'cache_read=0'}
                    </span>
                  </td>
                  <td className="mono">{user.top_model || '—'}</td>
                  <td className="mono">{credits(user.relay_credits)}</td>
                  <td className="mono">{credits(user.market_credits)}</td>
                  <td className="mono">{credits(user.memory_credits)}</td>
                  <td className="mono">{credits(user.dslpha_credits)}</td>
                  <td className="mono">{credits(user.sandbox_credits)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <p className="admin-note">
          计费按组织（billing is org-level）：用量与订单归到组织 owner，不臆造成员级拆分。
          没有 owner 邮箱通常表示 auth.users 不可读或组织尚未绑定 owner。
        </p>
      </section>

      <section className="usage-panel usage-panel--recent">
        <div className="usage-panel__head">
          <div><p className="kicker">RECENT ORDERS</p><h3>最近订单</h3></div>
          <span className="tag">区间内 · 最近 {recentOrders.length} 笔</span>
        </div>
        <div className="admin-table-wrap">
          <table className="admin-table admin-table--orders">
            <thead>
              <tr><th>时间</th><th>组织</th><th>SKU</th><th>金额</th><th>Credits</th><th>状态</th><th>支付时间</th></tr>
            </thead>
            <tbody>
              {recentOrders.length === 0 ? (
                <tr><td colSpan={7} className="admin-table__empty">{loading ? '正在加载订单…' : '这个区间还没有订单。'}</td></tr>
              ) : recentOrders.map((order) => (
                <tr key={order.id}>
                  <td className="mono">{new Date(order.created_at).toLocaleString('zh-CN', { hour12: false })}</td>
                  <td>
                    <span className="admin-table__org">{order.org_name || '—'}</span>
                    <span className="admin-sub mono" title={order.org_id}>{order.org_id.slice(0, 8)}…</span>
                  </td>
                  <td className="mono">{order.sku}</td>
                  <td className="mono">{yuan(order.amount_cents)}</td>
                  <td className="mono">{credits(order.credits)}</td>
                  <td><span className={`status status--${order.status}`}>{order.status}</span></td>
                  <td className="mono">{order.paid_at ? new Date(order.paid_at).toLocaleString('zh-CN', { hour12: false }) : '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <div className="admin-tools">
        <div className="admin-tools__head">
          <div><p className="kicker">ORGANIZATION TOOLS</p><h3>按组织查询与调整</h3></div>
          <p className="muted">查看账本与订单，手动调整积分；所有调整写入 ledger 审计。</p>
        </div>
        <div className="admin-search">
          <input className="input" placeholder="组织 ID / slug / 名称" value={q} onChange={(e) => setQ(e.target.value)} />
          <button className="btn btn--accent" onClick={() => void search()}>查询</button>
        </div>
        {orgError && <p className="form-error">{orgError}</p>}{msg && <p className="form-ok">{msg}</p>}
        <div className="table">
          <div className="table__row table__row--head"><span>组织</span><span>套餐</span><span>余额</span><span>操作</span></div>
          {orgs.length === 0 && <div className="table__empty">输入组织 ID / slug / 名称后查询。</div>}
          {orgs.map((o) => (
            <div className="table__row" key={o.id}>
              <span>{o.name} <code>{o.slug}</code></span>
              <span>{o.plan_code}</span>
              <span>{o.balance_credits}</span>
              <span><button className="btn btn--ghost" onClick={() => void open(o.id)}>查看</button></span>
            </div>
          ))}
        </div>
        {selected && (
          <div style={{ marginTop: 20 }}>
            <h3>{selected.org.name} <code>{selected.org.id}</code></h3>
            <p>套餐 {selected.billing.plan.code}，余额 {selected.billing.wallet.balance_credits}，可用 {selected.billing.wallet.available_credits}</p>
            <div className="admin-gift">
              <div className="admin-gift__head">
                <div><p className="kicker">GIFT CREDITS</p><h4>赠送积分</h4></div>
                <span className="tag">reason · admin_gift</span>
              </div>
              <p className="muted">显式赠送，仅支持正整数；写入独立账本记录（管理员赠送）并追加 admin_audit。通用调整仍在下方保留。</p>
              <div className="admin-gift__form">
                <input
                  className="input"
                  type="number"
                  min={1}
                  step={1}
                  placeholder="赠送数量（credits）"
                  value={giftCredits}
                  onChange={(e) => { setGiftCredits(e.target.value); setGiftKey(''); }}
                />
                <span className="admin-gift__yuan">≈ {yuan(Number(giftCredits) || 0)}</span>
                <input
                  className="input"
                  placeholder="备注（写入账本 ref_id）"
                  value={giftNote}
                  onChange={(e) => { setGiftNote(e.target.value); setGiftKey(''); }}
                />
                <button
                  className="btn btn--accent"
                  type="button"
                  onClick={() => void gift()}
                  disabled={giftBusy || !giftCredits || Number(giftCredits) <= 0}
                >
                  {giftBusy ? '赠送中…' : '赠送积分'}
                </button>
              </div>
              {giftMsg && <p className="form-ok">{giftMsg}</p>}
            </div>
            <div style={{ display: 'flex', gap: 8, alignItems: 'center', margin: '12px 0' }}>
              <input className="input" placeholder="调整数量，如 1000 或 -500" value={delta} onChange={(e) => setDelta(e.target.value)} />
              <input className="input" placeholder="原因" value={reason} onChange={(e) => setReason(e.target.value)} />
              <button className="btn btn--ghost" onClick={() => void adjust()} disabled={!delta || Number.isNaN(Number(delta))}>通用调整积分</button>
            </div>
            <h4>最近账本</h4>
            <div className="table">
              <div className="table__row table__row--head"><span>时间</span><span>产品</span><span>变动</span><span>原因</span><span>备注</span></div>
              {selected.ledger.map((r, i) => (
                <div className="table__row" key={i}><span>{String(r.created_at)}</span><span>{String(r.product || '-')}</span><span>{String(r.delta_credits)}</span><span>{ledgerReasonLabel(String(r.reason))}</span><span>{String(r.ref_id || '—')}</span></div>
              ))}
            </div>
            <h4>订单</h4>
            <div className="table">
              <div className="table__row table__row--head"><span>时间</span><span>SKU</span><span>金额</span><span>状态</span></div>
              {selected.orders.map((o) => (
                <div className="table__row" key={o.id}><span>{new Date(o.created_at).toLocaleString('zh-CN')}</span><span>{o.sku}</span><span>¥{(o.amount_cents / 100).toFixed(2)}</span><span>{o.status}</span></div>
              ))}
            </div>
          </div>
        )}
        <section className="admin-referrals">
          <div className="admin-tools__head">
            <div><p className="kicker">REFERRALS · PLATFORM</p><h3>邀请奖励记录</h3></div>
            <p className="muted">最近 20 条邀请；邀请人与新用户各 1000 credits 的奖励都来自账本。</p>
          </div>
          {referralsError && <p className="form-error">{referralsError}</p>}
          {referrals && (
            <>
              <div className="admin-referrals__totals">
                <span>共 <strong>{credits(referrals.total_referrals)}</strong> 条</span>
                <span>已奖励 <strong>{credits(referrals.rewarded_referrals)}</strong> 条</span>
                <span>邀请人获得 <strong>{credits(referrals.total_inviter_credits)}</strong> credits</span>
                <span>新用户获得 <strong>{credits(referrals.total_invitee_credits)}</strong> credits</span>
              </div>
              <div className="table">
                <div className="table__row table__row--head"><span>时间</span><span>邀请人 → 新用户</span><span>邀请码</span><span>奖励</span><span>状态</span></div>
                {referrals.referrals.length === 0 && <div className="table__empty">还没有邀请奖励记录。</div>}
                {referrals.referrals.map((row) => (
                  <div className="table__row" key={row.id}>
                    <span>{new Date(row.created_at).toLocaleString('zh-CN', { hour12: false })}</span>
                    <span>{row.inviter_org_name} → {row.invitee_org_name}</span>
                    <span className="mono">{row.code}</span>
                    <span>{credits(row.inviter_credits)} / {credits(row.invitee_credits)}</span>
                    <span><span className={`status status--${row.status}`}>{row.status}</span></span>
                  </div>
                ))}
              </div>
            </>
          )}
        </section>
      </div>
    </section>
  );
}

function Orders({ orders }: { orders: Order[] }) {
  orders = orders ?? [];
  return (
    <section className="section" id="orders">
      <div className="section__head">
        <div><p className="kicker">ORDERS</p><h2>订单记录。</h2></div>
      </div>
      <div className="table">
        <div className="table__row table__row--head"><span>时间</span><span>SKU</span><span>金额</span><span>积分</span><span>状态</span></div>
        {orders.length === 0 && <div className="table__empty">还没有订单。</div>}
        {orders.map((order) => (
          <div className="table__row" key={order.id}>
            <span>{new Date(order.created_at).toLocaleString('zh-CN')}</span>
            <span className="mono">{order.sku}</span>
            <span>{yuan(order.amount_cents)}</span>
            <span>{credits(order.credits)}</span>
            <span>
              <span className={`status status--${order.status}`}>{order.status}</span>
              {order.status === 'pending' && order.pay_url && (
                <a className="status__pay" href={order.pay_url} target="_blank" rel="noreferrer">去支付</a>
              )}
            </span>
          </div>
        ))}
      </div>
    </section>
  );
}

// First-party clients are trusted for the identity handoff: the user has
// already signed into the unified Roost organization, and the server still
// validates the registered redirect_uri + client secret at token exchange.
// Any other client keeps the explicit consent screen.
const FIRST_PARTY_OIDC_CLIENTS = new Set(['market', 'roostbot']);

function OAuthAuthorize({ session }: { session: Session }) {
  const params = new URLSearchParams(window.location.search);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const clientId = params.get('client_id') ?? 'roostbot';
  const trusted = FIRST_PARTY_OIDC_CLIENTS.has(clientId);
  const autoStarted = useRef(false);

  async function allow() {
    setBusy(true);
    setError('');
    try {
      const { redirect_url } = await api.authorizeOAuth(session.access_token, {
        response_type: params.get('response_type') ?? 'code',
        client_id: clientId,
        redirect_uri: params.get('redirect_uri') ?? '',
        state: params.get('state') ?? '',
        code_challenge: params.get('code_challenge') ?? '',
        code_challenge_method: params.get('code_challenge_method') ?? 'S256',
        scope: params.get('scope') ?? 'openid email profile',
        nonce: params.get('nonce') ?? '',
      });
      window.location.assign(redirect_url);
    } catch (err) {
      setError(err instanceof Error ? err.message : '授权失败');
      setBusy(false);
    }
  }

  useEffect(() => {
    if (!trusted || autoStarted.current) return;
    autoStarted.current = true;
    void allow();
  }, [trusted]);

  function deny() {
    const redirect = params.get('redirect_uri');
    if (!redirect) return;
    const url = new URL(redirect);
    url.searchParams.set('error', 'access_denied');
    const state = params.get('state');
    if (state) url.searchParams.set('state', state);
    window.location.assign(url.toString());
  }

  if (trusted) {
    return (
      <main className="consent">
        <div className="consent__card">
          <Logo />
          <p className="kicker" style={{ marginTop: 30 }}>FIRST-PARTY HANDOFF</p>
          <h1>正在连接 {clientId}</h1>
          <p className="consent__lead">
            你已登录统一 Roost 组织，正在把同一个身份交给 {clientId}。这是第一方工作台，
            不需要重复确认。
          </p>
          <div className="consent__org">
            <span className="tag tag--live">SIGNED IN</span>
            <span>{session.user.email}</span>
          </div>
          {error && <p className="form-error">{error}</p>}
          <div className="consent__actions">
            <button className="btn btn--ghost" onClick={deny} disabled={busy}>取消</button>
            <button className="btn btn--ink" onClick={allow} disabled={busy}>{busy ? '连接中…' : '重新连接'}</button>
          </div>
          <p className="consent__foot">Roost Cloud · 统一组织身份</p>
        </div>
      </main>
    );
  }
  return (
    <main className="consent">
      <div className="consent__card">
        <Logo />
        <p className="kicker" style={{ marginTop: 30 }}>AUTHORIZE APPLICATION</p>
        <h1>连接 {clientId}</h1>
        <p className="consent__lead">
          {clientId} 请求使用你的 Roost 组织身份登录。它只能拿到你的用户 ID、邮箱和组织
          ID，不能读取支付凭据或直接修改余额。
        </p>
        <div className="consent__org">
          <span className="tag tag--live">SIGNED IN</span>
          <span>{session.user.email}</span>
        </div>
        {error && <p className="form-error">{error}</p>}
        <div className="consent__actions">
          <button className="btn btn--ghost" onClick={deny} disabled={busy}>取消</button>
          <button className="btn btn--ink" onClick={allow} disabled={busy}>{busy ? '授权中…' : '允许访问'}</button>
        </div>
        <p className="consent__foot">Roost Cloud · 统一组织身份</p>
      </div>
    </main>
  );
}

function Console({ session }: { session: Session }) {
  const [me, setMe] = useState<MeResponse | null>(null);
  const [plans, setPlans] = useState<Plan[]>([]);
  const [orders, setOrders] = useState<Order[]>([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState('');
  const [pendingOrder, setPendingOrder] = useState<Order | null>(null);
  const [claimNotice, setClaimNotice] = useState('');
  const [claimError, setClaimError] = useState('');
  const [referralVersion, setReferralVersion] = useState(0);
  const claimAttemptRef = useRef('');
  const [route] = useRoute();
  const [navOpen, setNavOpen] = useState(false);
  const token = session.access_token;

  const load = useCallback(async () => {
    try {
      const [meData, plansData, ordersData] = await Promise.all([api.me(token), api.plans(token), api.orders(token)]);
      setMe(meData);
      setPlans(plansData.plans);
      setOrders(ordersData.orders);
      setError('');
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载失败');
    }
  }, [token]);

  useEffect(() => { void load(); }, [load]);

  // Claim a stored invite code once per authenticated session. The localStorage
  // entry is cleared only on success; any failure keeps it for the next login
  // without retrying in a loop during this session.
  useEffect(() => {
    if (!me) return;
    // An authenticated user can open an invite link directly, in which case
    // AuthScreen never runs; read the URL as well as the stored code.
    const pending = storedReferralCode() || referralCodeFromLocation();
    if (!pending) return;
    if (!storedReferralCode()) storeReferralCode(pending);
    const attempt = `${me.user.id}:${pending}`;
    if (claimAttemptRef.current === attempt) return;
    claimAttemptRef.current = attempt;
    let cancelled = false;
    void (async () => {
      try {
        const result = await api.claimReferral(token, pending);
        if (cancelled) return;
        storeReferralCode('');
        stripReferralFromLocation();
        setClaimError('');
        setClaimNotice(
          result.already_claimed
            ? `邀请码 ${result.code} 已领取过，本次未重复发放奖励。`
            : `邀请奖励已到账：+${credits(result.invitee_credits)} credits，当前余额 ${credits(result.balance_credits)}。`,
        );
        setReferralVersion((value) => value + 1);
        await load();
      } catch (err) {
        if (cancelled) return;
        setClaimError(
          `邀请奖励暂未领取：${err instanceof Error ? err.message : '请稍后重试'}。邀请码已保留，下次登录会自动重试。`,
        );
      }
    })();
    return () => { cancelled = true; };
  }, [me, token, load]);

  async function buy(sku: string, amountCents?: number) {
    setBusy(sku);
    setError('');
    try {
      const { order } = await api.createOrder(token, sku, amountCents);
      setPendingOrder(order);
    } catch (err) {
      setError(err instanceof Error ? err.message : '创建订单失败');
    } finally {
      setBusy('');
    }
  }

  const paid = useCallback(() => { void load(); }, [load]);

  if (!me) {
    return (
      <div className="loading">
        <Logo />
        <p>{error || '正在装配组织上下文…'}</p>
        {error && <button className="btn btn--ink" onClick={() => void load()}>重试</button>}
      </div>
    );
  }

  const isAdmin = me.user.email === '963841621@qq.com';
  const isOwner = me.org.role === 'owner';
  const page = route === 'admin' && !isAdmin ? 'overview' : route;

  let body: ReactNode;
  switch (page) {
    case 'keys':
      body = <RelayKeys token={token} orgId={me.org.id} />;
      break;
    case 'usage':
      body = <UsageDashboard token={token} orgId={me.org.id} />;
      break;
    case 'memory':
      body = <CausalMemory token={token} orgId={me.org.id} />;
      break;
    case 'dsalpha':
      body = (
        <>
          <section className="hero hero--compact glass">
            <div className="hero__text">
              <p className="kicker">DSALPHA</p>
              <h1>DSAlpha 投研工作台</h1>
              <p className="hero__lead">行情、研报、因子与定时任务。套餐额度按月发放，用完后自动使用通用余额。</p>
            </div>
            <div className="hero__actions">
              <a className="btn btn--accent" href={DSALPHA_URL} target="_blank" rel="noreferrer">打开 DSAlpha →</a>
            </div>
          </section>
          <Plans plans={plans} current={me.billing.plan.code} alwaysOnUntil={me.billing.always_on_until} onBuy={buy} busy={busy} />
        </>
      );
      break;
    case 'wallet':
      body = (
        <>
          <WalletCard billing={me.billing} />
          <Recharge current={me.billing.plan.code} alwaysOnUntil={me.billing.always_on_until} onBuy={buy} busy={busy} />
        </>
      );
      break;
    case 'billing':
      body = (
        <>
          <BillingDetailsSection token={token} orgId={me.org.id} />
          <Orders orders={orders} />
        </>
      );
      break;
    case 'referral':
      body = isOwner
        ? <ReferralCard token={token} version={referralVersion} />
        : <p className="muted">只有组织所有者可以邀请好友。</p>;
      break;
    case 'labs':
      body = <LabsPage />;
      break;
    case 'admin':
      body = <AdminPanel token={token} />;
      break;
    default:
      body = <Overview me={me} token={token} />;
  }

  return (
    <div className="shell">
      <div className="backdrop" aria-hidden="true" />
      <Sidebar route={page} isAdmin={isAdmin} isOwner={isOwner} open={navOpen} onClose={() => setNavOpen(false)} />
      <div className="shell__main">
        <Topbar
          route={page}
          email={me.user.email}
          availableCredits={me.billing.wallet.available_credits}
          onMenu={() => setNavOpen(true)}
          onSignOut={() => void supabase.auth.signOut()}
        />
        <main className="page" key={page}>
          {error && <p className="form-error">{error}</p>}
          {claimNotice && (
            <div className="form-notice claim-notice" role="status">
              <span>{claimNotice}</span>
              <button className="btn btn--ghost btn--small" type="button" onClick={() => setClaimNotice('')}>知道了</button>
            </div>
          )}
          {claimError && (
            <div className="form-notice form-notice--warn claim-notice" role="status">
              <span>{claimError}</span>
              <button className="btn btn--ghost btn--small" type="button" onClick={() => setClaimError('')}>关闭</button>
            </div>
          )}
          {body}
          <footer className="page__foot">
            <span>© 2026 Roost</span>
            <span>100 credits = ¥1 · 支付宝 / 微信支付</span>
          </footer>
        </main>
      </div>
      {pendingOrder && <OrderModal token={token} order={pendingOrder} onClose={() => setPendingOrder(null)} onPaid={paid} />}
    </div>
  );
}

export default function App() {
  const [session, setSession] = useState<Session | null>(null);
  const [loading, setLoading] = useState(true);
  const [confirmError, setConfirmError] = useState('');

  useEffect(() => {
    let cancelled = false;
    const boot = async () => {
      // Email links may carry a token_hash (the portable shape). Redeem it
      // before reading the session so the same page load lands signed in.
      const redeemed = await redeemEmailTokenHash();
      if (cancelled) return;
      if (redeemed.handled && redeemed.error) setConfirmError(redeemed.error);
      const { data } = await supabase.auth.getSession();
      if (cancelled) return;
      setSession(data.session);
      setLoading(false);
    };
    void boot();
    const { data: listener } = supabase.auth.onAuthStateChange((_event, next) => setSession(next));
    return () => {
      cancelled = true;
      listener.subscription.unsubscribe();
    };
  }, []);

  const isOAuth = window.location.pathname.includes('/oauth/authorize');
  const view = useMemo(() => {
    if (loading) return <div className="loading"><p>加载中…</p></div>;
    if (isOAuth) return session ? <OAuthAuthorize session={session} /> : <AuthScreen initialError={confirmError} />;
    return session ? <Console session={session} /> : <AuthScreen initialError={confirmError} />;
  }, [confirmError, isOAuth, loading, session]);

  return view;
}
