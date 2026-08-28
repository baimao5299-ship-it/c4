import type { components } from '@/lib/api/schema'

export type CredentialKind = 'codex-oauth' | 'codex-pat'
export type OAuthItem = components['schemas']['CodexOAuthImportItem']
export type PATItem = components['schemas']['CodexPATImportItem']
export type NormalizedRow = { index: number; raw: unknown; item?: OAuthItem | PATItem; error?: string; duplicateOf?: number }

const emailRe = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
const str = (value: unknown) => typeof value === 'string' ? value.trim() : value == null ? '' : String(value).trim()
const firstText = (...values: unknown[]) => {
  for (const value of values) {
    const normalized = str(value)
    if (normalized) return normalized
  }
  return ''
}

function timestamp(value: unknown): number | undefined {
  const source = typeof value === 'number' ? value : typeof value === 'string' && /^-?\d+$/.test(value.trim()) ? Number(value.trim()) : undefined
  if (source == null) return undefined
  if (!Number.isSafeInteger(source)) throw new Error('invalidExpired')
  return Math.abs(source) >= 1_000_000_000_000 ? source : source * 1000
}

export function normalizeExpired(value: unknown): string | undefined {
  if (value == null || value === '') return undefined
  const milliseconds = timestamp(value)
  const date = milliseconds == null ? new Date(str(value)) : new Date(milliseconds)
  if (Number.isNaN(date.getTime())) throw new Error('invalidExpired')
  return date.toISOString()
}

function validateIdentity(email: string, accountId: string) {
  if (!email || !accountId) return '邮箱和账号 ID 为必填项'
  if (!emailRe.test(email) || email.includes('..')) return '邮箱格式无效'
  return undefined
}

function headerValue(headers: unknown, name: string): string {
  if (!headers || typeof headers !== 'object' || Array.isArray(headers)) return ''
  let empty = ''
  for (const [key, value] of Object.entries(headers as Record<string, unknown>)) {
    if (key.trim().toLowerCase() !== name.toLowerCase()) continue
    const normalized = str(value)
    if (normalized) return normalized
    empty = normalized
  }
  return empty
}

function integer(value: unknown, label: string, min: number): number | undefined {
  if (value == null || value === '') return undefined
  const source = typeof value === 'number' ? value : typeof value === 'string' && /^-?\d+$/.test(value.trim()) ? Number(value.trim()) : NaN
  if (!Number.isSafeInteger(source) || source < min) throw new Error(`${label}:${min}`)
  return source
}

export function normalizeRow(raw: unknown, kind: CredentialKind, index: number): NormalizedRow {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return { index, raw, error: '行必须是 JSON 对象' }
  const obj = raw as Record<string, unknown>
  const sourceError = str(obj.__import_error)
  if (sourceError) return { index, raw, error: sourceError }
  // Exports from different tools can contain both aliases; prefer the first
  // populated value so an empty snake_case field cannot hide a valid camelCase
  // field (the same rule is used by the Sub2 adapter).
  const email = firstText(obj.email, obj.codex_email)
  const accountId = firstText(obj.account_id, obj.codex_account_id)
  const identityError = validateIdentity(email, accountId)
  if (identityError) return { index, raw, error: identityError }
  try {
    const weight = integer(obj.weight, 'weight', 0)
    const concurrency = integer(obj.max_concurrency, 'max_concurrency', 1)
    if (kind === 'codex-oauth') {
      const token = firstText(obj.access_token, obj.codex_oauth_token)
      const refresh = firstText(obj.refresh_token, obj.codex_oauth_refresh_token)
      // Sub2 的 session 导出允许 accessToken-only；refresh 缺失时由后端
      // 保留已有 refresh（更新）或创建无自动续期账号（新建），不能把整行误判为无效。
      if (!token) return { index, raw, error: 'OAuth access_token 必填' }
      const item: OAuthItem = { codex_email: email, codex_account_id: accountId, codex_oauth_token: token, ...(refresh ? { codex_oauth_refresh_token: refresh } : {}) }
      const expired = normalizeExpired(firstText(obj.expired, obj.codex_oauth_expires_at) || undefined)
      if (expired) item.codex_oauth_expires_at = expired
      if (weight != null) item.weight = weight
      if (concurrency != null) item.max_concurrency = concurrency
      return { index, raw, item }
    }
    const key = (headerValue(obj.headers, 'authorization') || firstText(obj.codex_pat_key, obj.personal_access_token)).replace(/^Bearer\s+/i, '').trim()
    if (!key) return { index, raw, error: 'PAT 凭据不能为空；OAuth access_token 不能作为 PAT 导入' }
    const item: PATItem = { codex_email: email, codex_account_id: accountId, codex_pat_key: key }
    if (weight != null) item.weight = weight
    if (concurrency != null) item.max_concurrency = concurrency
    return { index, raw, item }
  } catch (error) {
    if (error instanceof Error && error.message === 'invalidExpired') return { index, raw, error: 'expired 时间格式无效' }
    if (error instanceof Error && error.message.startsWith('weight:')) return { index, raw, error: 'weight 必须是大于等于 0 的整数' }
    if (error instanceof Error && error.message.startsWith('max_concurrency:')) return { index, raw, error: 'max_concurrency 必须是大于等于 1 的整数' }
    return { index, raw, error: '行格式无效' }
  }
}

export function markDuplicateRows(rows: NormalizedRow[]): NormalizedRow[] {
  const firstByKey = new Map<string, number>()
  return rows.map(row => {
    if (!row.item || row.error) return row
    const item = row.item as Record<string, unknown>
    const accountId = str(item.codex_account_id).toLowerCase()
    const email = str(item.codex_email).toLowerCase()
    const credential = str(item.codex_oauth_refresh_token ?? item.codex_oauth_token ?? item.codex_pat_key)
    // The persisted identity is the composite (codex_email, codex_account_id).
    // Using account_id alone rejects legitimate accounts that share a workspace
    // id but belong to different emails.
    const key = email && accountId
      ? `identity:${email}\u0000${accountId}`
      : credential ? `credential:${credential}` : ''
    if (!key) return row
    const first = firstByKey.get(key)
    if (first != null) return { ...row, duplicateOf: first, error: `重复账号：与第 ${first + 1} 行相同` }
    firstByKey.set(key, row.index)
    return row
  })
}
