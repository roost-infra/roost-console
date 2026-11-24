import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react';
import { api, type MemoryActivity, type RelayMemory, type RelayPreferences } from './lib/api';
import './control-memory.css';

const inherited: RelayPreferences = { compression_enabled: null, compression_percent: null, memory_enabled: null };

function message(error: unknown) {
  return error instanceof Error ? error.message : '请求失败';
}

const toggle = (value: boolean | null) => value === null ? '' : value ? 'on' : 'off';
const parseToggle = (value: string) => value === '' ? null : value === 'on';

export function RelayControls({ token, orgId, admin }: { token: string; orgId: string; admin: boolean }) {
  const [scope, setScope] = useState<'personal' | 'organization'>('personal');
  const [settings, setSettings] = useState<Awaited<ReturnType<typeof api.relayPreferences>>>();
  const [draft, setDraft] = useState(inherited);
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    const abort = new AbortController();
    api.relayPreferences(token, orgId, abort.signal).then((data) => {
      setSettings(data);
      setDraft(data.personal);
    }).catch((err: unknown) => { if (!abort.signal.aborted) setError(message(err)); });
    return () => abort.abort();
  }, [token, orgId]);

  function selectScope(value: 'personal' | 'organization') {
    setScope(value);
    setDraft(settings?.[value] ?? inherited);
    setNotice('');
    setError('');
  }

  async function save(event: FormEvent) {
    event.preventDefault();
    setSaving(true);
    setError('');
    setNotice('');
    try {
      const data = await api.saveRelayPreferences(token, orgId, scope, draft);
      setSettings(data);
      setDraft(data[scope]);
      setNotice('已保存，下一次新请求生效。');
    } catch (err) {
      setError(message(err));
    } finally {
      setSaving(false);
    }
  }

  function source(field: keyof RelayPreferences) {
    if (settings?.personal[field] != null) return '个人设置';
    if (settings?.organization[field] != null) return '组织默认';
    return '部署默认';
  }

  const effective = settings?.effective;
  return <section className="glass relay-controls" aria-labelledby="relay-controls-title">
    <div className="relay-controls__heading"><h2 id="relay-controls-title">上下文与模型记忆</h2><a href="#/memory">打开记忆脑图 →</a></div>
    {error && <p role="alert" className="form-error">{error}</p>}
    {!settings && !error && <p>正在读取设置…</p>}
    {settings && <form onSubmit={save}>
      {admin && <label>设置范围 <select value={scope} disabled={saving}
        onChange={(event) => selectScope(event.target.value === 'organization' ? 'organization' : 'personal')}>
        <option value="personal">个人</option><option value="organization">组织默认</option>
      </select></label>}
      <div className="relay-controls__fields">
        <label>上下文压缩
          <select aria-label="上下文压缩" value={toggle(draft.compression_enabled)} onChange={(event) => setDraft({ ...draft, compression_enabled: parseToggle(event.target.value) })}>
            <option value="">继承</option><option value="on">开启</option><option value="off">关闭</option>
          </select>
          <small>当前 {effective?.compression_enabled == null ? '自动' : effective.compression_enabled ? '开启' : '关闭'} · {source('compression_enabled')}</small>
        </label>
        <fieldset>
          <legend>目标减少的输入 token</legend>
          <label className="relay-controls__inherit"><input type="checkbox" checked={draft.compression_percent === null}
            onChange={(event) => setDraft({ ...draft, compression_percent: event.target.checked ? null : effective?.compression_percent ?? 30 })} />继承比例</label>
          <div className="relay-controls__slider">
            <input aria-label="压缩比例滑杆" type="range" min="0" max="100" disabled={draft.compression_percent === null}
              value={draft.compression_percent ?? effective?.compression_percent ?? 0}
              onChange={(event) => setDraft({ ...draft, compression_percent: event.target.valueAsNumber })} />
            <input aria-label="压缩百分比" type="number" min="0" max="100" required disabled={draft.compression_percent === null}
              value={draft.compression_percent ?? ''} onChange={(event) => setDraft({ ...draft, compression_percent: event.target.valueAsNumber || 0 })} />%
          </div>
          <small>当前 {effective?.compression_percent == null ? '自动' : effective.compression_percent + '%'} · {source('compression_percent')}</small>
        </fieldset>
        <label>模型记忆
          <select aria-label="模型记忆" value={toggle(draft.memory_enabled)} onChange={(event) => setDraft({ ...draft, memory_enabled: parseToggle(event.target.value) })}>
            <option value="">继承</option><option value="on">开启</option><option value="off">关闭</option>
          </select>
          <small>当前 {effective?.memory_enabled == null ? '继承部署默认' : effective.memory_enabled ? '开启' : '关闭'} · {source('memory_enabled')}</small>
        </label>
      </div>
      <p className="relay-controls__terms">0% 跳过压缩。实际比例取决于可压缩历史。只有实际节省输入 token 时才计压缩明细；原始用量计费模式下，当前按原始输入估算加 2% 缓冲，再抵扣节省量的 30%。已采用的摘要成本在总额内列示一次。个人记忆会保存本次对话的文本片段，可在记忆页管理。</p>
      <button className="btn btn--ink" disabled={saving}>保存设置</button>
      {notice && <span role="status" className="relay-controls__notice">{notice}</span>}
    </form>}
  </section>;
}

const phaseLabel = { search: '检索', match: '命中', write: '写回' } as const;
const statusLabel: Record<string, string> = { running: '进行中', ok: '已完成', miss: '无命中', error: '失败' };

export function ControlMemory({ token, orgId }: { token: string; orgId: string }) {
  const [items, setItems] = useState<RelayMemory[]>([]);
  const [events, setEvents] = useState<MemoryActivity[]>([]);
  const [query, setQuery] = useState('');
  const [filter, setFilter] = useState('');
  const [offset, setOffset] = useState(0);
  const [more, setMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const [connection, setConnection] = useState('连接中');
  const [error, setError] = useState('');
  const [pending, setPending] = useState(false);
  const [selected, setSelected] = useState<string[]>([]);
  const [editor, setEditor] = useState<{ id?: string; version?: number; body: string }>();
  const [active, setActive] = useState('');
  const [view, setView] = useState<'graph' | 'list'>('graph');
  const [zoom, setZoom] = useState(1);
  const [pan, setPan] = useState({ x: 0, y: 0 });
  const drag = useRef<{ pointer: number; x: number; y: number } | null>(null);
  const [displayEvent, setDisplayEvent] = useState<MemoryActivity>();
  const [trackedRequest, setTrackedRequest] = useState('');
  const animationQueue = useRef<MemoryActivity[]>([]);
  const cursor = useRef(0);
  const initiallyLoaded = useRef(false);

  const refresh = useCallback(async (signal?: AbortSignal) => {
    try {
      const data = await api.relayMemories(token, orgId, filter, offset, signal);
      signal?.throwIfAborted();
      setItems(data.items ?? []);
      setMore(data.more);
    } catch (err) {
      if (!signal?.aborted) setError(message(err));
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, [token, orgId, filter, offset]);

  useEffect(() => {
    const abort = new AbortController();
    void refresh(abort.signal);
    return () => abort.abort();
  }, [refresh]);

  useEffect(() => {
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    async function poll() {
      try {
        const latest = await api.relayMemoryActivity(token, orgId, cursor.current, abort.signal);
        if (abort.signal.aborted) return;
        setConnection('实时活动');
        const batch = (latest ?? []).sort((left, right) => left.id - right.id);
        if (batch.length) {
          if (!initiallyLoaded.current) setDisplayEvent(batch.at(-1));
          else animationQueue.current.push(...batch.slice(-12));
          cursor.current = batch.at(-1)!.id;
          setEvents((existing) => [...existing, ...batch].slice(-60));
          if (batch.some((event) => event.phase === 'write' && event.status === 'ok')) void refresh(abort.signal);
        }
        initiallyLoaded.current = true;
      } catch {
        if (!abort.signal.aborted) setConnection('正在重连');
      } finally {
        if (!abort.signal.aborted) timer = setTimeout(poll, document.hidden ? 6000 : 1200);
      }
    }
    void poll();
    return () => { abort.abort(); clearTimeout(timer); };
  }, [token, orgId, refresh]);

  useEffect(() => {
    const timer = setInterval(() => {
      const next = animationQueue.current.shift();
      if (next) setDisplayEvent(next);
    }, 650);
    return () => clearInterval(timer);
  }, []);

  const requests = Array.from(new Set(events.map(({ request_id }) => request_id))).reverse();
  const requestId = trackedRequest || displayEvent?.request_id || '';
  const visibleEvents = events.filter((event) => event.request_id === requestId && (trackedRequest || event.id <= (displayEvent?.id ?? 0)));
  const current = visibleEvents.at(-1);
  const highlighted = new Set(current?.memory_ids);
  const activeItem = items.find(({ id }) => id === active);
  const glyphs = (['manual', 'model'] as const).flatMap((source) => items.filter((item) => item.source === source).map((item, index) => {
    const ring = Math.floor(index / 10);
    const angle = (source === 'manual' ? Math.PI / 2 : -Math.PI / 2) + ((index % 10 + 1) * Math.PI / 11);
    const radius = 15 + ring * 5.3;
    return { item, x: 50 + Math.cos(angle) * radius * 1.1, y: 50 + Math.sin(angle) * radius };
  }));

  async function save(event: FormEvent) {
    event.preventDefault();
    if (!editor) return;
    setPending(true);
    setError('');
    try {
      const saved = await api.saveRelayMemory(token, orgId, editor);
      setActive(saved.id);
      setEditor(undefined);
      await refresh();
    } catch (err) {
      setError(message(err));
    } finally {
      setPending(false);
    }
  }

  async function remove(ids: string[], all = false) {
    const label = all ? '清空你在当前组织下的所有个人记忆？' : '删除所选的 ' + ids.length + ' 条个人记忆？';
    if (!window.confirm(label)) return;
    setPending(true);
    setError('');
    try {
      await api.deleteRelayMemory(token, orgId, ids, all);
      setSelected((existing) => all ? [] : existing.filter((id) => !ids.includes(id)));
      if (all || ids.includes(active)) setActive('');
      if (all || (editor?.id && ids.includes(editor.id))) setEditor(undefined);
      if (all) {
        setEvents([]);
        setDisplayEvent(undefined);
        setTrackedRequest('');
        animationQueue.current = [];
        setOffset(0);
      }
      await refresh();
    } catch (err) {
      setError(message(err));
    } finally {
      setPending(false);
    }
  }

  return <section className="glass memory-control" aria-labelledby="memory-title">
    <header className="memory-control__header">
      <div><h1 id="memory-title">记忆脑图</h1><p>查看模型如何检索和写回你的个人记忆。</p></div>
      <a className="btn btn--ghost" href="#/keys">模型设置</a>
    </header>
    <div className="memory-control__actions">
      <form onSubmit={(event) => { event.preventDefault(); setLoading(true); setOffset(0); setSelected([]); if (filter === query.trim() && offset === 0) void refresh(); else setFilter(query.trim()); }}>
        <input aria-label="搜索记忆" placeholder="搜索记忆内容" value={query} onChange={(event) => setQuery(event.target.value)} />
        <button className="btn btn--ghost">搜索</button>
      </form>
      <button className="btn btn--ink" onClick={() => setEditor({ body: '' })}>新增记忆</button>
      <button className="btn btn--ghost" disabled={pending} onClick={() => void remove([], true)}>清空个人记忆</button>
    </div>
    {error && <p className="form-error" role="alert">{error}</p>}
    <div className="memory-control__workspace">
      <div className="memory-control__visual">
        <div className="memory-control__view">
          <button aria-pressed={view === 'graph'} onClick={() => setView('graph')}>脑图</button>
          <button aria-pressed={view === 'list'} onClick={() => setView('list')}>列表</button>
          {view === 'graph' && <><button aria-label="缩小" onClick={() => setZoom((value) => Math.max(0.7, value - 0.15))}>−</button>
            <button aria-label="重置画布" onClick={() => { setZoom(1); setPan({ x: 0, y: 0 }); }}>{Math.round(zoom * 100)}%</button>
            <button aria-label="放大" onClick={() => setZoom((value) => Math.min(2.2, value + 0.15))}>＋</button></>}
        </div>
        {view === 'graph' ? <div className="memory-graph" onPointerDown={(event) => {
          if ((event.target as Element).closest('button')) return;
          event.currentTarget.setPointerCapture(event.pointerId);
          drag.current = { pointer: event.pointerId, x: event.clientX - pan.x, y: event.clientY - pan.y };
        }} onPointerMove={(event) => {
          if (drag.current?.pointer === event.pointerId) setPan({ x: event.clientX - drag.current.x, y: event.clientY - drag.current.y });
        }} onLostPointerCapture={() => { drag.current = null; }}>
          <div className="memory-graph__surface" style={{ transform: 'translate(' + pan.x + 'px,' + pan.y + 'px) scale(' + zoom + ')' }}>
            <svg className="memory-graph__topology" viewBox="0 0 100 100" preserveAspectRatio="none" aria-hidden="true">
              <path className="memory-graph__outline" d="M50 16 C43 8 34 11 30 17 C21 16 15 23 15 33 C9 38 10 48 13 54 C9 62 15 73 22 75 C24 84 35 89 42 83 C46 86 49 83 50 79 C54 85 60 87 65 83 C74 86 81 79 82 72 C89 68 89 60 86 54 C91 47 88 38 85 33 C87 25 78 18 71 18 C65 11 56 10 50 16Z" />
              <path className="memory-graph__lobes" d="M50 16 C45 28 53 38 50 47 S54 68 50 79 M15 33 Q29 22 37 40 T45 59 M22 75 Q25 62 36 65 M85 33 Q71 22 63 40 T55 59 M82 72 Q76 62 64 65" />
              <ellipse cx="50" cy="50" rx="36" ry="37" /><ellipse cx="50" cy="50" rx="27" ry="28" /><ellipse cx="50" cy="50" rx="17" ry="18" />
              {glyphs.filter(({ item }) => highlighted.has(item.id)).map(({ item, x, y }) => <path key={item.id + ':' + current?.id}
                className={current?.phase === 'write' ? 'memory-edge memory-edge--write' : 'memory-edge'}
                d={'M 50 50 Q ' + (x + 50) / 2 + ' ' + (y - 8) + ' ' + x + ' ' + y} pathLength="1" />)}
            </svg>
            <span className="memory-graph__cluster memory-graph__cluster--manual">手动记忆</span>
            <span className="memory-graph__cluster memory-graph__cluster--model">模型写回</span>
            <div key={current?.id} className={'memory-graph__center' + (current?.phase === 'search' ? ' memory-graph__center--pulse' : '')}>
              <span>海马体</span><small>{current ? phaseLabel[current.phase] : '待检索'}</small>
            </div>
            {glyphs.map(({ item, x, y }) => <button key={item.id} aria-label={item.body.slice(0, 80)}
              aria-pressed={active === item.id} title={item.body.slice(0, 100)}
              className={'memory-node memory-node--' + item.source + (highlighted.has(item.id) ? ' memory-node--lit' : '')}
              style={{ left: x + '%', top: y + '%' }} onClick={() => setActive(item.id)} />)}
          </div>
          {!items.length && <p className="memory-graph__empty">{loading ? '正在加载…' : filter ? '没有匹配的记忆' : '暂无个人记忆'}</p>}
        </div> : <div className="memory-list">
          {!items.length && <p>{loading ? '正在加载…' : '没有匹配的记忆'}</p>}
          {items.map((item) => <div className="memory-list__row" key={item.id}>
            <input aria-label={'选择记忆 ' + item.body.slice(0, 32)} type="checkbox" checked={selected.includes(item.id)}
              onChange={(event) => setSelected((value) => event.target.checked ? [...value, item.id] : value.filter((id) => id !== item.id))} />
            <button onClick={() => setActive(item.id)}>{item.body}</button>
          </div>)}
        </div>}
        <div className="memory-control__pagination">
          <button className="btn btn--ghost" disabled={offset === 0} onClick={() => { setLoading(true); setSelected([]); setOffset(offset - 50); }}>上一页</button>
          <button className="btn btn--ghost" disabled={!more} onClick={() => { setLoading(true); setSelected([]); setOffset(offset + 50); }}>下一页</button>
          {!!selected.length && <button className="btn btn--ghost" disabled={pending} onClick={() => void remove(selected)}>删除所选（{selected.length}）</button>}
        </div>
      </div>
      <aside className="memory-control__detail">
        {editor ? <form onSubmit={save}>
          <label>{editor.id ? '编辑记忆' : '新增记忆'}<textarea value={editor.body} maxLength={4000} required rows={6}
            onChange={(event) => setEditor({ ...editor, body: event.target.value })} /></label>
          <button className="btn btn--ink" disabled={pending || !editor.body.trim()}>保存</button>
          <button type="button" className="btn btn--ghost" onClick={() => setEditor(undefined)}>取消</button>
        </form> : activeItem && <>
          <h2>记忆详情</h2>
          <small>{activeItem.source === 'manual' ? '手动创建' : '模型写回'} · {new Date(activeItem.updated_at).toLocaleString()}</small>
          <p className="memory-detail__body">{activeItem.body}</p>
          <button className="btn btn--ghost" onClick={() => setEditor(activeItem)}>编辑</button>
          <button className="btn btn--ghost" disabled={pending} onClick={() => void remove([activeItem.id])}>删除</button>
        </>}
        <h2>调用路径 <small>{connection}</small></h2>
        <select aria-label="选择调用路径" value={trackedRequest} onChange={(event) => setTrackedRequest(event.target.value)}>
          <option value="">跟随最新调用</option>
          {requests.map((id) => <option key={id} value={id}>{id.slice(0, 16)}</option>)}
        </select>
        <ol className="memory-control__timeline">
          {(['search', 'match', 'write'] as const).map((phase) => {
            const status = [...visibleEvents].reverse().find((event) => event.phase === phase);
            const resolved = phase === 'search' && visibleEvents.some((event) => event.phase === 'match') ? 'ok' : status?.status;
            return <li key={phase} data-current={current?.phase === phase}><span>{phaseLabel[phase]}</span>
              <small>{resolved ? (statusLabel[resolved] ?? resolved) : '等待'}{phase === 'match' && status?.memory_ids.length ? ' · ' + status.memory_ids.length + ' 条' : ''}</small>
            </li>;
          })}
        </ol>
      </aside>
    </div>
  </section>;
}
