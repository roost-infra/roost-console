import { useState } from 'react';

// Public addresses of the Roost relay. One key works on every protocol below;
// a client only needs the address in its "Base URL" field plus the key.
export const RELAY_ORIGIN = 'https://api.roostbots.com';
export const RELAY_BASE_URL = `${RELAY_ORIGIN}/v1`;
export const MODELS_URL = '/relay.html#models';
export const DOCS_URL = '/relay.html';
export const MEMORY_MCP_URL = 'https://causal-memory.com/memory/mcp';
export const DSALPHA_URL = 'https://dsalpha.roostbots.com';

export const ENDPOINTS = [
  {
    key: 'openai',
    label: 'OpenAI 兼容',
    url: RELAY_BASE_URL,
    note: 'Chat Completions · OpenAI SDK、Cherry Studio、LobeChat、Dify 等绝大多数工具',
    primary: true,
  },
  {
    key: 'anthropic',
    label: 'Anthropic 兼容',
    url: RELAY_ORIGIN,
    note: 'Messages API · Claude Code、Anthropic SDK（客户端会自动拼接 /v1/messages）',
    primary: false,
  },
  {
    key: 'responses',
    label: 'Responses API',
    url: RELAY_BASE_URL,
    note: 'Responses API · Codex CLI、OpenAI Agents SDK',
    primary: false,
  },
] as const;

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Non-secure contexts (http, some webviews) have no async clipboard.
    try {
      const area = document.createElement('textarea');
      area.value = text;
      area.setAttribute('readonly', '');
      area.style.position = 'fixed';
      area.style.opacity = '0';
      document.body.appendChild(area);
      area.select();
      const ok = document.execCommand('copy');
      document.body.removeChild(area);
      return ok;
    } catch {
      return false;
    }
  }
}

export function CopyButton({ text, label = '复制', className = '' }: { text: string; label?: string; className?: string }) {
  const [state, setState] = useState<'idle' | 'ok' | 'fail'>('idle');
  async function onClick() {
    const ok = await copyText(text);
    setState(ok ? 'ok' : 'fail');
    window.setTimeout(() => setState('idle'), 1600);
  }
  return (
    <button type="button" className={`copy-btn ${state === 'ok' ? 'is-ok' : ''} ${className}`} onClick={() => void onClick()}>
      {state === 'ok' ? '已复制' : state === 'fail' ? '请手动复制' : label}
    </button>
  );
}

export function CopyField({ label, value, hint, secret = false }: { label?: string; value: string; hint?: string; secret?: boolean }) {
  return (
    <div className="copy-field">
      {label && <span className="copy-field__label">{label}</span>}
      <div className={`copy-field__box ${secret ? 'copy-field__box--secret' : ''}`}>
        <code title={value}>{value}</code>
        <CopyButton text={value} />
      </div>
      {hint && <span className="copy-field__hint">{hint}</span>}
    </div>
  );
}

export function EndpointList({ compact = false }: { compact?: boolean }) {
  const list = compact ? ENDPOINTS.filter((item) => item.key !== 'responses') : ENDPOINTS;
  return (
    <div className={`endpoint-list ${compact ? 'endpoint-list--compact' : ''}`}>
      {list.map((item) => (
        <div className={`endpoint ${item.primary ? 'endpoint--primary' : ''}`} key={item.key}>
          <div className="endpoint__head">
            <span className="endpoint__label">{item.label}</span>
            {item.primary && <span className="pill pill--accent">推荐</span>}
          </div>
          <div className="copy-field__box">
            <code title={item.url}>{item.url}</code>
            <CopyButton text={item.url} />
          </div>
          <p className="endpoint__note">{item.note}</p>
        </div>
      ))}
    </div>
  );
}

const KEY_PLACEHOLDER = 'rk-你的APIKey';

function snippets(apiKey: string) {
  const key = apiKey || KEY_PLACEHOLDER;
  return [
    {
      key: 'curl',
      label: 'cURL',
      code: `curl ${RELAY_BASE_URL}/chat/completions \\
  -H "Authorization: Bearer ${key}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "deepseek-v4-pro",
    "messages": [{"role": "user", "content": "你好"}]
  }'`,
    },
    {
      key: 'python',
      label: 'Python',
      code: `from openai import OpenAI

client = OpenAI(
    api_key="${key}",
    base_url="${RELAY_BASE_URL}",
)

resp = client.chat.completions.create(
    model="deepseek-v4-pro",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)`,
    },
    {
      key: 'claude',
      label: 'Claude Code',
      code: `export ANTHROPIC_BASE_URL="${RELAY_ORIGIN}"
export ANTHROPIC_AUTH_TOKEN="${key}"
export ANTHROPIC_MODEL="claude-sonnet-5"

claude`,
    },
    {
      key: 'codex',
      label: 'Codex',
      code: `# ~/.codex/config.toml
model = "gpt-6-sol"
model_provider = "roost"

[model_providers.roost]
name = "Roost"
base_url = "${RELAY_BASE_URL}"
env_key = "ROOST_API_KEY"
wire_api = "responses"

# 终端里：export ROOST_API_KEY="${key}"`,
    },
  ];
}

export function SnippetTabs({ apiKey = '' }: { apiKey?: string }) {
  const items = snippets(apiKey);
  const [active, setActive] = useState(items[0].key);
  const current = items.find((item) => item.key === active) ?? items[0];
  return (
    <div className="snippet">
      <div className="snippet__bar">
        <div className="seg" role="tablist" aria-label="接入示例">
          {items.map((item) => (
            <button
              key={item.key}
              type="button"
              role="tab"
              aria-selected={item.key === active}
              className={`seg__btn ${item.key === active ? 'is-active' : ''}`}
              onClick={() => setActive(item.key)}
            >
              {item.label}
            </button>
          ))}
        </div>
        <CopyButton text={current.code} label="复制代码" />
      </div>
      <pre className="snippet__code"><code>{current.code}</code></pre>
      {!apiKey && <p className="snippet__hint">把 <code>{KEY_PLACEHOLDER}</code> 换成你创建的 API Key。</p>}
    </div>
  );
}

export function KeyCreatedModal({ apiKey, onClose }: { apiKey: string; onClose: () => void }) {
  return (
    <div className="modal" role="dialog" aria-modal="true" aria-label="API Key 已创建">
      <div className="modal__panel modal__panel--wide">
        <button className="modal__close" onClick={onClose} aria-label="关闭">×</button>
        <p className="kicker">API KEY 已创建</p>
        <h2>复制并保存你的 Key</h2>
        <p className="muted">出于安全考虑，完整 Key 只显示这一次。关闭后只能看到它的指纹，丢失请重新创建。</p>
        <CopyField label="API Key" value={apiKey} secret />
        <CopyField label="Base URL（OpenAI 兼容）" value={RELAY_BASE_URL} hint="Claude Code / Anthropic SDK 填 https://api.roostbots.com" />
        <p className="kicker" style={{ marginTop: 22 }}>直接可用的接入代码</p>
        <SnippetTabs apiKey={apiKey} />
        <div className="modal__actions">
          <button className="btn btn--ink btn--wide" onClick={onClose}>我已保存</button>
        </div>
      </div>
    </div>
  );
}
