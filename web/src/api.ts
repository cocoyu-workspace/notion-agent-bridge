import type { DashboardData, TokenStats } from './types'

async function readJson<T>(resp: Response, fallbackMessage: string): Promise<T> {
  const text = await resp.text()
  if (!text) {
    throw new Error(fallbackMessage)
  }
  try {
    return JSON.parse(text) as T
  } catch {
    throw new Error(fallbackMessage)
  }
}

// --- Dashboard API ---

export interface AccountListParams {
  page?: number
  pageSize?: number
  query?: string
}

export async function fetchDashboardData(params: AccountListParams = {}): Promise<DashboardData> {
  // Server-side pagination keeps the payload small for big pools — see
  // proxy.HandleAdminAccounts for the contract.
  const sp = new URLSearchParams()
  if (params.page !== undefined) sp.set('page', String(params.page))
  if (params.pageSize !== undefined) sp.set('page_size', String(params.pageSize))
  if (params.query && params.query.trim()) sp.set('q', params.query.trim())
  const qs = sp.toString()
  const url = qs ? `/admin/accounts?${qs}` : '/admin/accounts'
  const resp = await fetch(url)
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
  return resp.json()
}

export async function triggerRefresh(): Promise<{ started: boolean; message?: string }> {
  const resp = await fetch('/admin/refresh', { method: 'POST' })
  return resp.json()
}

export async function fetchTokenStats(): Promise<TokenStats> {
  const resp = await fetch('/admin/stats')
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
  return resp.json()
}

export function openProxy(accountId: string) {
  window.open(`/proxy/start?account_id=${encodeURIComponent(accountId)}`, '_blank')
}

export function openBestProxy() {
  window.open('/proxy/start?best=true', '_blank')
}

// --- Account Management API ---

export interface AddAccountResult {
  status?: string
  error?: string
  filename?: string
  account?: {
    name: string
    email: string
    space: string
    plan_type: string
  }
}

export async function addAccount(tokenV2: string): Promise<AddAccountResult> {
  const resp = await fetch('/admin/accounts/add', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    credentials: 'same-origin',
    body: JSON.stringify({ token_v2: tokenV2 }),
  })
  const data = await readJson<AddAccountResult>(resp, '添加账号接口返回了无效响应')
  if (!resp.ok) return { error: data.error || `HTTP ${resp.status}` }
  return data
}

// --- Settings API ---

export interface SearchSettings {
  enable_web_search: boolean
  enable_workspace_search: boolean
  // ask_mode_default flips Notion's workflow useReadOnlyMode flag for
  // every chat request — model answers but skips edits, matching the
  // frontend "Ask" toggle. Per-request `-ask` model suffix still
  // overrides this default for a single call.
  ask_mode_default: boolean
  disable_notion_prompt: boolean
  debug_logging: boolean
  // notion_proxy is the global upstream proxy applied to every Notion-bound
  // outbound connection. Empty string means "direct dial". Editing this
  // field via /admin/settings PUT immediately drops idle pooled
  // connections so subsequent requests pick up the new upstream.
  notion_proxy: string
}

export async function fetchSettings(): Promise<SearchSettings> {
  const resp = await fetch('/admin/settings')
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
  return resp.json()
}

export async function updateSettings(settings: Partial<Pick<SearchSettings, 'enable_web_search' | 'enable_workspace_search' | 'ask_mode_default' | 'debug_logging' | 'notion_proxy'>>): Promise<SearchSettings> {
  const resp = await fetch('/admin/settings', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(settings),
  })
  if (!resp.ok) {
    // Surface server-side validation errors (e.g. unsupported proxy
    // scheme) so the caller can show them in a toast and roll the input
    // back instead of silently saving.
    const text = await resp.text()
    let msg = `HTTP ${resp.status}`
    if (text) {
      try {
        const data = JSON.parse(text)
        if (data && typeof data.error === 'string') msg = data.error
      } catch { /* ignore */ }
    }
    throw new Error(msg)
  }
  return resp.json()
}

// --- Account management ---

async function jsonOrError(resp: Response): Promise<any> {
  const text = await resp.text()
  let data: any = null
  if (text) {
    try { data = JSON.parse(text) } catch { /* ignore */ }
  }
  if (!resp.ok) {
    const msg = (data && typeof data.error === 'string') ? data.error : `HTTP ${resp.status}`
    throw new Error(msg)
  }
  return data
}

export async function deleteAccount(accountId: string): Promise<void> {
  const resp = await fetch(`/admin/accounts/${encodeURIComponent(accountId)}`, {
    method: 'DELETE',
    credentials: 'same-origin',
  })
  await jsonOrError(resp)
}

export async function setAccountRole(accountId: string, role: 'primary' | 'education'): Promise<void> {
  const resp = await fetch('/admin/accounts/role', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ account_id: accountId, role }),
  })
  await jsonOrError(resp)
}

export async function switchAccount(accountId: string): Promise<void> {
  const resp = await fetch('/admin/accounts/switch', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ account_id: accountId }),
  })
  await jsonOrError(resp)
}

