import type { components } from '@/lib/api/schema'

export type CredentialKind = 'codex-oauth' | 'codex-pat'
export type OAuthItem = components['schemas']['CodexOAuthImportItem']
export type PATItem = components['schemas']['CodexPATImportItem']
export type NormalizedRow = { index: number; raw: unknown; item?: OAuthItem | PATItem; error?: string; duplicateOf?: number }

const emailRe = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
const str = (value: unknown) => typeof value === 'string' ? value.trim() : value == null ? '' : String(value).trim()

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
  for (const [key, value] of Object.entries(headers as Record<string, unknown>)) {
    if (key.trim().toLowerCase() === name.toLowerCase()) return str(value)
  }
  return ''
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
  const email = str(obj.email ?? obj.codex_email)
  const accountId = str(obj.account_id ?? obj.codex_account_id)
  const identityError = validateIdentity(email, accountId)
  if (identityError) return { index, raw, error: identityError }
  try {
    const weight = integer(obj.weight, 'weight', 0)
    const concurrency = integer(obj.max_concurrency, 'max_concurrency', 1)
    if (kind === 'codex-oauth') {
      const token = str(obj.access_token ?? obj.codex_oauth_token)
      const refresh = str(obj.refresh_token ?? obj.codex_oauth_refresh_token)
      if (!token || !refresh) return { index, raw, error: 'OAuth access_token 与 refresh_token 必须成对填写' }
      const item: OAuthItem = { codex_email: email, codex_account_id: accountId, codex_oauth_token: token, codex_oauth_refresh_token: refresh }
      const expired = normalizeExpired(obj.expired ?? obj.codex_oauth_expires_at)
      if (expired) item.codex_oauth_expires_at = expired
      if (weight != null) item.weight = weight
      if (concurrency != null) item.max_concurrency = concurrency
      return { index, raw, item }
    }
    const key = (headerValue(obj.headers, 'authorization') || str(obj.codex_pat_key ?? obj.personal_access_token)).replace(/^Bearer\s+/i, '').trim()
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
    const key = accountId ? `account:${accountId}` : email ? `email:${email}` : credential ? `credential:${credential}` : ''
    if (!key) return row
    const first = firstByKey.get(key)
    if (first != null) return { ...row, duplicateOf: first, error: `重复账号：与第 ${first + 1} 行相同` }
    firstByKey.set(key, row.index)
    return row
  })
}
