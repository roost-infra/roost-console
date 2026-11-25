import { useCallback, useEffect, useState, type ReactNode } from 'react';
import { DOCS_URL, MODELS_URL } from './access';

export type RouteKey =
  | 'overview'
  | 'keys'
  | 'settings'
  | 'usage'
  | 'memory'
  | 'dsalpha'
  | 'wallet'
  | 'billing'
  | 'referral'
  | 'labs'
  | 'admin';

const ROUTES: RouteKey[] = ['overview', 'keys', 'settings', 'usage', 'memory', 'dsalpha', 'wallet', 'billing', 'referral', 'labs', 'admin'];

// Old single-page anchors (#billing, #api-keys, …) keep working as deep links.
const LEGACY: Record<string, RouteKey> = {
  'api-keys': 'keys',
  'relay-settings': 'settings',
  usage: 'usage',
  'causal-memory': 'memory',
  plans: 'dsalpha',
  recharge: 'wallet',
  billing: 'wallet',
  orders: 'billing',
  'billing-details': 'billing',
  referral: 'referral',
  products: 'overview',
};

function parseHash(hash: string): RouteKey {
  if (hash.startsWith('#/')) {
    const raw = hash.slice(2).split(/[?&]/)[0];
    return (ROUTES as string[]).includes(raw) ? (raw as RouteKey) : 'overview';
  }
  const legacy = hash.replace(/^#/, '').split(/[?&]/)[0];
  return LEGACY[legacy] ?? 'overview';
}

// A legacy anchor (#orders) names an element on the routed page; scroll to it
// once that page has mounted. Route links (#/billing) start at the top.
function legacyAnchor(hash: string) {
  return hash.startsWith('#/') ? '' : hash.replace(/^#/, '').split(/[?&]/)[0];
}

function revealAnchor(hash: string) {
  const id = legacyAnchor(hash);
  if (!id) {
    window.scrollTo({ top: 0 });
    return;
  }
  let tries = 0;
  const seek = () => {
    const el = document.getElementById(id);
    if (el) el.scrollIntoView({ block: 'start' });
    else if (tries++ < 20) window.setTimeout(seek, 100);
  };
  window.setTimeout(seek, 0);
}

export function useRoute(): [RouteKey, (next: RouteKey) => void] {
  const [route, setRoute] = useState<RouteKey>(() => parseHash(window.location.hash));
  useEffect(() => {
    if (legacyAnchor(window.location.hash)) revealAnchor(window.location.hash);
    const onHash = () => {
      setRoute(parseHash(window.location.hash));
      revealAnchor(window.location.hash);
    };
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);
  const navigate = useCallback((next: RouteKey) => {
    if (window.location.hash !== `#/${next}`) window.location.hash = `/${next}`;
  }, []);
  return [route, navigate];
}

// 1.5px stroke icons, 24px grid (lucide-compatible paths).
const ICONS: Record<string, ReactNode> = {
  overview: <><rect x="3" y="3" width="7" height="9" rx="1.5" /><rect x="14" y="3" width="7" height="5" rx="1.5" /><rect x="14" y="12" width="7" height="9" rx="1.5" /><rect x="3" y="16" width="7" height="5" rx="1.5" /></>,
  keys: <><circle cx="7.5" cy="15.5" r="4.5" /><path d="m10.7 12.3 9.8-9.8M17 6l3 3M14.5 8.5l2 2" /></>,
  settings: <><circle cx="12" cy="12" r="3" /><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.9l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.9-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.9.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.9 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.9l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.9.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.9-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.9V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1Z" /></>,
  usage: <><path d="M3 3v18h18" /><path d="m7 15 4-4 3 3 6-7" /></>,
  models: <><path d="M12 2 3 7l9 5 9-5-9-5Z" /><path d="m3 12 9 5 9-5M3 17l9 5 9-5" /></>,
  docs: <><path d="M4 19.5V5a2 2 0 0 1 2-2h12v16H6a2 2 0 0 0-2 2Z" /><path d="M8 7h6M8 11h6" /></>,
  memory: <><path d="M9.5 3a3.5 3.5 0 0 0-3.4 4.3A3.5 3.5 0 0 0 5 14a3.5 3.5 0 0 0 4.5 5.4V3Z" /><path d="M14.5 3a3.5 3.5 0 0 1 3.4 4.3A3.5 3.5 0 0 1 19 14a3.5 3.5 0 0 1-4.5 5.4V3Z" /></>,
  dsalpha: <><path d="M3 17l5-5 4 4 8-9" /><path d="M14 7h6v6" /></>,
  wallet: <><rect x="3" y="6" width="18" height="14" rx="2.5" /><path d="M3 10h18M16 15h2" /><path d="M6 6V5a2 2 0 0 1 2-2h9" /></>,
  billing: <><path d="M6 2h12v20l-3-2-3 2-3-2-3 2V2Z" /><path d="M9 7h6M9 11h6M9 15h4" /></>,
  referral: <><path d="M20 12v9H4v-9M2 7h20v5H2zM12 21V7" /><path d="M12 7H7.5a2.5 2.5 0 1 1 0-5C11 2 12 7 12 7ZM12 7h4.5a2.5 2.5 0 1 0 0-5C13 2 12 7 12 7Z" /></>,
  labs: <><path d="M9 3h6M10 3v6L4.5 19A1.5 1.5 0 0 0 6 21h12a1.5 1.5 0 0 0 1.5-2L14 9V3" /></>,
  admin: <><path d="M12 3 4 6v6c0 4.5 3.4 8.3 8 9 4.6-.7 8-4.5 8-9V6l-8-3Z" /></>,
  external: <><path d="M14 4h6v6M20 4l-9 9" /><path d="M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5" /></>,
  menu: <><path d="M4 7h16M4 12h16M4 17h16" /></>,
};

export function Icon({ name, size = 18 }: { name: string; size?: number }) {
  return (
    <svg className="icon" width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      {ICONS[name]}
    </svg>
  );
}

type NavItem = { key: RouteKey; label: string; icon: string } | { href: string; label: string; icon: string };

const NAV: Array<{ title: string; items: NavItem[]; when?: 'owner' | 'admin' }> = [
  { title: '', items: [{ key: 'overview', label: '概览', icon: 'overview' }] },
  {
    title: 'API 中转',
    items: [
      { key: 'keys', label: 'API Key 与接入', icon: 'keys' },
      { key: 'usage', label: '用量统计', icon: 'usage' },
      { key: 'settings', label: '上下文与记忆', icon: 'settings' },
      { href: MODELS_URL, label: '模型与价格', icon: 'models' },
      { href: DOCS_URL, label: '接入文档', icon: 'docs' },
    ],
  },
  {
    title: '产品',
    items: [
      { key: 'dsalpha', label: 'DSAlpha 投研', icon: 'dsalpha' },
      { key: 'memory', label: '记忆 Memory', icon: 'memory' },
    ],
  },
  {
    title: '账户',
    items: [
      { key: 'wallet', label: '钱包与充值', icon: 'wallet' },
      { key: 'billing', label: '账单与订单', icon: 'billing' },
      { key: 'referral', label: '邀请奖励', icon: 'referral' },
    ],
  },
  { title: '更多', items: [{ key: 'labs', label: '沙箱与算力', icon: 'labs' }] },
  { title: '平台', items: [{ key: 'admin', label: '运营后台', icon: 'admin' }], when: 'admin' },
];

export const ROUTE_TITLES: Record<RouteKey, string> = {
  overview: '概览',
  keys: 'API Key 与接入',
  settings: '上下文与记忆',
  usage: '用量统计',
  memory: '记忆 Memory',
  dsalpha: 'DSAlpha 投研',
  wallet: '钱包与充值',
  billing: '账单与订单',
  referral: '邀请奖励',
  labs: '沙箱与算力',
  admin: '运营后台',
};

export function Brand() {
  return (
    <a className="brand" href="/" aria-label="Roost 首页">
      <span className="brand__mark" aria-hidden="true">
        <svg viewBox="0 0 28 28" focusable="false">
          <path d="M4 22V10.5L14 5l10 5.5V22" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinejoin="round" />
          <path d="M9 22v-7.2h10V22" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinejoin="round" opacity=".55" />
          <circle cx="14" cy="10.6" r="2.1" fill="currentColor" />
        </svg>
      </span>
      <span className="brand__name">Roost</span>
      <span className="brand__tag">Console</span>
    </a>
  );
}

export function Sidebar({
  route,
  isAdmin,
  isOwner,
  open,
  onClose,
}: {
  route: RouteKey;
  isAdmin: boolean;
  isOwner: boolean;
  open: boolean;
  onClose: () => void;
}) {
  return (
    <>
      <div className={`scrim ${open ? 'is-open' : ''}`} onClick={onClose} aria-hidden="true" />
      <aside className={`sidebar glass ${open ? 'is-open' : ''}`} aria-label="控制台导航">
        <div className="sidebar__brand"><Brand /></div>
        <nav className="sidebar__nav">
          {NAV.filter((group) => group.when !== 'admin' || isAdmin).map((group) => (
            <div className="nav-group" key={group.title || 'root'}>
              {group.title && <p className="nav-group__title">{group.title}</p>}
              {group.items
                .filter((item) => !('key' in item) || item.key !== 'referral' || isOwner)
                .map((item) =>
                  'key' in item ? (
                    <a
                      key={item.key}
                      href={`#/${item.key}`}
                      className={`nav-item ${route === item.key ? 'is-active' : ''}`}
                      aria-current={route === item.key ? 'page' : undefined}
                      onClick={onClose}
                    >
                      <Icon name={item.icon} />
                      <span>{item.label}</span>
                    </a>
                  ) : (
                    <a key={item.href} href={item.href} className="nav-item" target="_blank" rel="noreferrer">
                      <Icon name={item.icon} />
                      <span>{item.label}</span>
                      <span className="nav-item__ext"><Icon name="external" size={13} /></span>
                    </a>
                  ),
                )}
            </div>
          ))}
        </nav>
        <div className="sidebar__foot">
          <a href="/" className="sidebar__link">返回官网</a>
          <span>100 credits = ¥1</span>
        </div>
      </aside>
    </>
  );
}

export function Topbar({
  route,
  email,
  availableCredits,
  onMenu,
  onSignOut,
}: {
  route: RouteKey;
  email: string;
  availableCredits: number;
  onMenu: () => void;
  onSignOut: () => void;
}) {
  return (
    <header className="topbar glass">
      <button className="topbar__menu" type="button" onClick={onMenu} aria-label="打开导航"><Icon name="menu" /></button>
      <div className="topbar__title">
        <span className="topbar__crumb">控制台</span>
        <span className="topbar__sep">/</span>
        <strong>{ROUTE_TITLES[route]}</strong>
      </div>
      <div className="topbar__right">
        <a className="balance-chip" href="#/wallet" title="可用余额">
          <span className="balance-chip__dot" />
          <span className="balance-chip__value">{new Intl.NumberFormat('zh-CN').format(availableCredits)}</span>
          <span className="balance-chip__unit">credits</span>
          <span className="balance-chip__cta">充值</span>
        </a>
        <a className="btn btn--ink btn--small topbar__keys" href="#/keys"><Icon name="keys" size={15} />API Key</a>
        <span className="who" title={email}>{email}</span>
        <button className="btn btn--ghost btn--small" onClick={onSignOut}>退出</button>
      </div>
    </header>
  );
}
