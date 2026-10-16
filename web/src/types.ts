export interface Model {
  id: string
  name: string
}

// Notion-native allowance (getCreditRateLimitStatus). Every number may be
// null: unknown is never rendered as 0% / unlimited / exhausted.
export interface CreditWindowView {
  scope?: string
  credit_type?: string
  window?: string
  cadence?: string
  used: number | null
  limit: number | null
  percent: number | null
  percent_rounded: number | null
  reset_at?: string
  resets_in_seconds?: number
  period_end?: string
  expired: boolean
  full: boolean
}

export type CreditState =
  | 'unchecked' | 'within_limit' | 'no_window' | 'not_applicable'
  | 'unknown_status' | 'incomplete' | 'failed' | 'expired'

export interface CreditView {
  state: CreditState
  status?: string
  observed_at?: string
  fetch_error?: string
  rolling?: CreditWindowView
  billing?: CreditWindowView
  recover_at?: string
  confirms_use: boolean
}

export interface CreditPool {
  kind: 'rolling' | 'billing'
  scope: string
  credit_type: string
  window?: string
  cadence?: string
  accounts: number
  summed: number
  period_end?: string
  max_percent: number | null
  used: number | null
  limit: number | null
  percent: number | null
}

export interface CreditSummary {
  by_state: Partial<Record<CreditState, number>>
  pools: CreditPool[]
}

export interface AccountInfo {
  account_id: string
  email: string
  name: string
  plan: string
  space: string
  space_id_short?: string
  exhausted: boolean
  permanent: boolean
  // no_workspace is true when the backend probed loadUserContent and found
  // that user_root.space_views is empty — the /ai SPA gets stuck on a
  // skeleton screen for these accounts. Dashboard treats them as
  // unusable (no click-through, sorted to the bottom, dedicated badge).
  no_workspace?: boolean
  // space_count is the raw probe result. Absent when the backend never
  // probed (fresh registration before the first refresh tick).
  space_count?: number
  workspace_checked_at?: string
  eligible?: boolean
  usage?: number
  limit?: number
  remaining?: number
  space_usage?: number
  space_limit?: number
  space_remaining?: number
  user_usage?: number
  user_limit?: number
  user_remaining?: number
  checked_at?: string
  exhausted_at?: string
  last_usage_at?: number
  models?: Model[]
  research_usage?: number
  has_premium?: boolean
  premium_balance?: number
  premium_usage?: number
  premium_limit?: number
  token_v2?: string
  registered_via?: string
  // Identity / role / health (never contains tokens or cookies)
  email_known?: boolean
  email_display?: string
  role?: 'primary' | 'education'
  role_set?: boolean
  current?: boolean
  status?: 'ok' | 'exhausted' | 'permanent' | 'no_workspace' | 'error'
  last_used_at?: string
  last_error?: string
  last_error_at?: string
  // Native allowance + legacy-source marker (legacy counters above are NOT the plan allowance)
  credit?: CreditView
  legacy_eligible?: boolean
  legacy_source?: string
  workspace_key?: string
  user_key?: string
}

export interface CurrentAccount {
  account_id: string
  email: string
  email_known: boolean
  email_display: string
  name?: string
  space?: string
  role: 'primary' | 'education'
  status: AccountInfo['status']
  last_used_at?: string
  last_error?: string
}

export interface RefreshStatus {
  refreshing: boolean
  done: number
  total: number
  last_refresh_at?: string
  error?: string
}

// AccountSummary mirrors Go's proxy.AccountSummary. The backend computes
// pool-wide aggregates (counts, quota sums) so the dashboard headline
// cards don't need to download the full account list to render. These
// fields reflect ALL accounts regardless of the current ?q= filter, so
// the headline numbers stay stable while the user searches.
export interface AccountSummary {
  total_accounts?: number
  available_accounts?: number
  credit?: CreditSummary
  exhausted_only: number
  no_workspace: number
  premium_accounts: number
  research_limited: number
  total_research_usage: number
  total_remaining: number
  total_space_usage: number
  total_space_limit: number
  total_space_remaining: number
  total_user_usage: number
  total_user_limit: number
  total_user_remaining: number
  total_premium_balance: number
  total_premium_limit: number
}

export interface DashboardData {
  total: number
  available: number
  models: Model[]
  accounts: AccountInfo[]
  // Account most recently used successfully for inference (all pages).
  current_account?: CurrentAccount
  refresh?: RefreshStatus
  // Present whenever the request used pagination params. `accounts` is
  // already the page slice; use `filtered_total` to compute pagination
  // controls on the client.
  page?: number
  page_size?: number
  filtered_total?: number
  // Pool-wide aggregates over ALL accounts (independent of ?q=).
  // The backend always emits this; the optional marker keeps older
  // dev builds tolerant.
  summary?: AccountSummary
}

export interface TokenBucket {
  input: number
  output: number
  total: number
  requests?: number
}

export interface TokenDayPoint {
  date: string
  input: number
  output: number
  total: number
}

export interface TokenModelRow {
  model: string
  input: number
  output: number
  total: number
  count: number
}

export interface TokenAccountRow {
  email: string
  input: number
  output: number
  total: number
  count: number
}

export interface TokenStats {
  total: TokenBucket
  today: TokenBucket
  last_24h: TokenBucket
  by_day: TokenDayPoint[]
  top_models: TokenModelRow[]
  top_accounts: TokenAccountRow[]
  last_record_at: number
}
