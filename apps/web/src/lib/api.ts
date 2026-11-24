export interface Plan {
  code: string;
  name: string;
  price_cents: number;
  duration_days: number;
  included_credits: number;
  product_caps: Record<string, number>;
  features: Record<string, unknown>;
  product: string;
  trial_days: number;
  first_month_discount_bps: number;
  annual_discount_bps: number;
}

export interface ProductState {
  product: string;
  enabled: boolean;
  monthly_cap_credits: number | null;
  used_this_month: number;
}

export interface Wallet {
  balance_credits: number;
  reserved_credits: number;
  available_credits: number;
}

export interface BillingView {
  org: { id: string; name: string; slug: string; plan_code: string; role: string };
  plan: Plan;
  wallet: Wallet;
  products: ProductState[];
  always_on_until?: string | null;
}

export interface MeResponse {
  user: { id: string; email: string };
  org: { id: string; name: string; slug: string; plan_code: string; role: string };
  billing: BillingView;
}

export interface ReferralOverview {
  code: string;
  link: string;
  invited_count: number;
  inviter_credits: number;
  invitee_credits: number;
  total_inviter_credits: number;
  total_invitee_credits: number;
}

export interface ReferralClaimResult {
  ok: boolean;
  referral_id: string;
  code: string;
  status: string;
  already_claimed: boolean;
  inviter_credits: number;
  invitee_credits: number;
  balance_credits: number;
  rewarded_at?: string | null;
}

export interface AdminReferralRow {
  id: string;
  code: string;
  inviter_user_id: string;
  inviter_org_id: string;
  inviter_org_name: string;
  invitee_user_id: string;
  invitee_org_id: string;
  invitee_org_name: string;
  status: string;
  inviter_credits: number;
  invitee_credits: number;
  created_at: string;
  rewarded_at: string | null;
}

export interface AdminReferrals {
  total_referrals: number;
  rewarded_referrals: number;
  total_inviter_credits: number;
  total_invitee_credits: number;
  referrals: AdminReferralRow[];
}

export interface Order {
  id: string;
  org_id: string;
  sku: string;
  amount_cents: number;
  credits: number;
  status: string;
  qr_url?: string;
  pay_url?: string;
  created_at: string;
}

const API = (import.meta.env.VITE_API_URL as string | undefined) ?? '';

async function request<T>(path: string, token: string, init?: RequestInit): Promise<T> {
  const response = await fetch(`${API}${path}`, {
    ...init,
    signal: init?.signal ?? AbortSignal.timeout(15000),
    headers: {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
      ...(init?.headers ?? {}),
    },
  });
  const body = await response.json().catch(() => null);
  if (!response.ok) {
    const message = body?.error?.message ?? body?.detail ?? '请求失败';
    throw new Error(message);
  }
  return body as T;
}

export interface RelayKey {
  token_hash: string;
  org_id: string;
  product: string;
  created_at: string;
  last_used_at?: string | null;
  revoked_at?: string | null;
}

export interface AdminOrg {
  id: string;
  name: string;
  slug: string;
  plan_code: string;
  owner_user_id: string;
  balance_credits: number;
  reserved_credits: number;
  created_at: string;
}

export interface AdminOverviewTokens {
  tracked_requests: number;
  input: number;
  output: number;
  cache_read: number;
  cache_write: number;
  total: number;
  cache_hit_rate: number;
}

export interface AdminOverviewTotals {
  orgs: number;
  users: number;
  active_orgs: number;
  orders: number;
  paid_orders: number;
  revenue_cents: number;
  credits_consumed: number;
  tokens: AdminOverviewTokens;
  product_spend_credits: Record<string, number>;
}

export interface AdminOverviewDaily {
  date: string;
  relay: number;
  market: number;
  'causal-memory': number;
  dslpha: number;
  tokens: number;
  revenue_cents: number;
}

export interface AdminModelRow {
  model: string;
  requests: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  total_tokens: number;
  cache_hit_rate: number;
}

export interface AdminTopOrg {
  org_id: string;
  name: string;
  owner_user_id: string;
  owner_email: string;
  plan_code: string;
  balance_credits: number;
  orders: number;
  revenue_cents: number;
  relay_credits: number;
  market_credits: number;
  memory_credits: number;
  dslpha_credits: number;
  sandbox_credits: number;
  requests: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  total_tokens: number;
  cache_hit_rate: number;
  top_model: string;
}

// One row per organization owner. Billing and usage are org-scoped, so the
// owner is the accountable user; the API does not invent per-member splits.
export interface AdminUserRow {
  org_id: string;
  org_name: string;
  user_id: string;
  email: string;
  plan_code: string;
  balance_credits: number;
  orders: number;
  revenue_cents: number;
  relay_credits: number;
  market_credits: number;
  memory_credits: number;
  dslpha_credits: number;
  sandbox_credits: number;
  requests: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  total_tokens: number;
  cache_hit_rate: number;
  top_model: string;
}

export interface AdminRecentOrder {
  id: string;
  org_id: string;
  org_name: string;
  sku: string;
  amount_cents: number;
  credits: number;
  status: string;
  created_at: string;
  paid_at: string | null;
}

export interface AdminOverview {
  days: number;
  usage_tracking_start: string | null;
  totals: AdminOverviewTotals;
  daily: AdminOverviewDaily[];
  models: AdminModelRow[];
  top_orgs: AdminTopOrg[];
  users: AdminUserRow[];
  recent_orders: AdminRecentOrder[];
}

export interface CompressionRow {
  id: number;
  org_id: string;
  request_id: string;
  settlement_id: string;
  lease_id: string;
  applied: boolean;
  original_input_tokens: number;
  compressed_input_tokens: number;
  saved_tokens: number;
  user_share_tokens: number;
  user_share_minor: number;
  summary_cost_minor: number;
  created_at: string;
}

export interface CompressionTotals {
  count: number;
  saved_tokens: number;
  user_share_tokens: number;
  user_share_minor: number;
  summary_cost_minor: number;
}

export interface CompressionUsage {
  rows: CompressionRow[];
  totals: CompressionTotals;
  limit: number;
}

export interface UsageTotals {
  requests: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  total_tokens: number;
  credits_minor: number;
  upstream_cost_minor: number;
  cache_hit_rate: number;
  saved_tokens: number;
  user_share_tokens: number;
  user_share_minor: number;
  summary_cost_minor: number;
}

export interface BillingDetailsSummary {
  total_credits: number;
  total_minor: number;
  total_yuan: number;
  token_credits: number;
  token_minor: number;
  sandbox_credits: number;
  sandbox_minor: number;
  sandbox_count: number;
  requests: number;
  total_tokens: number;
}

// One row per model. credits_minor is null when a single settlement lease
// carried more than one model, because the ledger charge cannot be split
// honestly; the UI shows "—" instead of an estimate.
export interface BillingTokenRow {
  model: string;
  requests: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  total_tokens: number;
  credits_minor: number | null;
  credits: number | null;
  yuan: number | null;
  effective_input_credits_per_million: number | null;
  effective_output_credits_per_million: number | null;
}

export interface BillingSandboxRow {
  id: number;
  sandbox_id: string;
  provider: string;
  provider_sandbox_id: string;
  user_id: string;
  product: string;
  started_at: string | null;
  ended_at: string | null;
  billable_seconds: number;
  billable_hours: number;
  cpu_seconds: number;
  memory_mib_seconds: number;
  disk_mib_seconds: number;
  gpu_seconds: number;
  cpu_credits: number;
  memory_credits: number;
  disk_credits: number;
  gpu_credits: number;
  total_credits: number;
  credits_minor: number;
  yuan: number;
  unit_prices: Record<string, unknown> | null;
}

export interface BillingDetails {
  days: number;
  range_start: string;
  range_end: string;
  summary: BillingDetailsSummary;
  tokens: BillingTokenRow[];
  sandboxes: BillingSandboxRow[];
}

export interface UsageSessionRow {
  session_id: string;
  requests: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  total_tokens: number;
  cache_hit_rate: number;
  last_seen: string;
}

export interface UsageSummaryResponse {
  days: number;
  totals: UsageTotals;
  sessions: UsageSessionRow[];
  session_count: number;
  session_hit_rate: number;
}

export interface UsageDailyRow {
  date: string;
  requests: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  total_tokens: number;
  credits_minor: number;
  saved_tokens: number;
  user_share_tokens: number;
  user_share_minor: number;
}

export interface UsageDailyResponse {
  days: UsageDailyRow[];
}

export interface UsageRecentRow {
  created_at: string;
  model: string;
  request_id: string;
  requests: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  total_tokens: number;
  cache_hit_rate: number;
  credits_minor: number;
  saved_tokens: number;
  user_share_tokens: number;
  user_share_minor: number;
  compression_applied: boolean;
  summary_cost_minor: number;
  original_input_tokens: number;
  compressed_input_tokens: number;
}

export interface UsageRecentResponse {
  rows: UsageRecentRow[];
  limit: number;
}

export interface RelayPreferences {
  compression_enabled: boolean | null;
  compression_percent: number | null;
  memory_enabled: boolean | null;
}

interface RelayPreferenceSet {
  personal: RelayPreferences;
  organization: RelayPreferences;
  effective: RelayPreferences;
}

export interface RelayMemory {
  id: string;
  body: string;
  source: 'manual' | 'model';
  request_id?: string;
  version: number;
  updated_at: string;
}

export interface MemoryActivity {
  id: number;
  request_id: string;
  phase: 'search' | 'match' | 'write';
  status: string;
  memory_ids: string[];
  created_at: string;
}

const relayControlPath = (orgId: string) => '/v1/orgs/' + encodeURIComponent(orgId) + '/relay';

export const api = {
  relayPreferences: (token: string, orgId: string, signal?: AbortSignal) =>
    request<RelayPreferenceSet>(relayControlPath(orgId) + '/preferences', token, { signal }),
  saveRelayPreferences: (token: string, orgId: string, scope: 'personal' | 'organization', values: RelayPreferences) =>
    request<RelayPreferenceSet>(relayControlPath(orgId) + '/preferences/' + scope, token, { method: 'PUT', body: JSON.stringify(values) }),
  relayMemories: (token: string, orgId: string, query: string, offset: number, signal?: AbortSignal) =>
    request<{ items: RelayMemory[]; more: boolean }>(relayControlPath(orgId) + '/memories?q=' + encodeURIComponent(query) + '&offset=' + offset, token, { signal }),
  saveRelayMemory: (token: string, orgId: string, item: { id?: string; body: string; version?: number }) =>
    request<RelayMemory>(relayControlPath(orgId) + '/memories' + (item.id ? '/' + encodeURIComponent(item.id) : ''), token, {
      method: item.id ? 'PUT' : 'POST', body: JSON.stringify({ body: item.body, version: item.version }),
    }),
  deleteRelayMemory: (token: string, orgId: string, ids: string[], all: boolean) =>
    request<{ deleted: number }>(relayControlPath(orgId) + '/memories/delete', token, { method: 'POST', body: JSON.stringify({ ids, all }) }),
  relayMemoryActivity: (token: string, orgId: string, after: number, signal?: AbortSignal) =>
    request<MemoryActivity[]>(relayControlPath(orgId) + '/memory-events?after=' + after, token, { signal }),
  me: (token: string) => request<MeResponse>('/v1/me', token),
  plans: (token: string) => request<{ plans: Plan[] }>('/v1/billing/plans', token),
  createOrder: (token: string, sku: string, amountCents?: number) =>
    request<{ order: Order }>('/v1/billing/orders', token, {
      method: 'POST',
      body: JSON.stringify(amountCents === undefined ? { sku } : { sku, amount_cents: amountCents }),
    }),
  getOrder: (token: string, id: string) => request<{ order: Order }>(`/v1/billing/orders/${id}`, token),
  devPay: (token: string, id: string) => request<{ ok: boolean }>(`/v1/dev/orders/${id}/pay`, token, { method: 'POST' }),
  orders: (token: string) => request<{ orders: Order[] }>('/v1/billing/orders', token),
  referral: (token: string) => request<ReferralOverview>('/v1/referral', token),
  claimReferral: (token: string, code: string) =>
    request<ReferralClaimResult>('/v1/referral/claim', token, { method: 'POST', body: JSON.stringify({ code }) }),
  compression: (token: string, orgId: string, limit = 20) =>
    request<CompressionUsage>(`/v1/orgs/${orgId}/billing/compression?limit=${limit}`, token),
  usageSummary: (token: string, orgId: string, days = 30) =>
    request<UsageSummaryResponse>(`/v1/orgs/${orgId}/usage/summary?days=${days}`, token),
  usageDaily: (token: string, orgId: string, days = 30) =>
    request<UsageDailyResponse>(`/v1/orgs/${orgId}/usage/daily?days=${days}`, token),
  usageRecent: (token: string, orgId: string, limit = 50) =>
    request<UsageRecentResponse>(`/v1/orgs/${orgId}/usage/recent?limit=${limit}`, token),
  billingDetails: (token: string, orgId: string, days = 30) =>
    request<BillingDetails>(`/v1/orgs/${orgId}/billing/details?days=${days}`, token),
  adminOverview: (token: string, days = 30) =>
    request<AdminOverview>(`/v1/admin/overview?days=${days}`, token),
  adminOrgs: (token: string, q: string) =>
    request<{ orgs: AdminOrg[] }>(`/v1/admin/orgs?q=${encodeURIComponent(q)}`, token),
  adminOrg: (token: string, orgId: string) =>
    request<{ org: AdminOrg; billing: BillingView; ledger: Array<Record<string, unknown>>; orders: Order[] }>(`/v1/admin/orgs/${orgId}`, token),
  adminAdjust: (token: string, orgId: string, body: { delta_credits: number; reason: string; idempotency_key: string }) =>
    request<{ ok: boolean; balance_credits: number }>(`/v1/admin/orgs/${orgId}/credits`, token, { method: 'POST', body: JSON.stringify(body) }),
  adminGift: (token: string, orgId: string, body: { credits: number; note: string; idempotency_key: string }) =>
    request<{ ok: boolean; balance_credits: number; credits: number; note: string }>(
      `/v1/admin/orgs/${orgId}/gift`,
      token,
      {
        method: 'POST',
        headers: { 'Idempotency-Key': body.idempotency_key },
        body: JSON.stringify(body),
      },
    ),
  adminReferrals: (token: string, limit = 50) =>
    request<AdminReferrals>(`/v1/admin/referrals?limit=${limit}`, token),
  relayKeys: (token: string, orgId: string) =>
    request<{ tokens: RelayKey[] }>(`/v1/orgs/${orgId}/products/relay/tokens`, token),
  createRelayKey: (token: string, orgId: string) =>
    request<{ token: string; token_hash: string }>(`/v1/orgs/${orgId}/products/relay/token`, token, { method: 'POST' }),
  revokeRelayKey: (token: string, orgId: string, hash: string) =>
    request<{ ok: boolean }>(`/v1/orgs/${orgId}/products/relay/tokens/${hash}`, token, { method: 'DELETE' }),
  issueCausalToken: (token: string, orgId: string) =>
    request<{ token: string; mcp_url: string; org_id: string }>(`/v1/orgs/${orgId}/products/causal-memory/token`, token, { method: 'POST' }),
  authorizeOAuth: (token: string, body: Record<string, string>) =>
    request<{ redirect_url: string }>('/v1/oauth/authorize', token, { method: 'POST', body: JSON.stringify(body) }),
};
