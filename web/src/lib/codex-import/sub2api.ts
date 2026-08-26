import { normalizeRow, type CredentialKind, type NormalizedRow } from './normalize'
import { parseRawText } from './parse'

type RecordValue = Record<string, unknown>

const isRecord = (value: unknown): value is RecordValue => !!value && typeof value === 'object' && !Array.isArray(value)
const text = (value: unknown) => typeof value === 'string' ? value.trim() : value == null ? '' : String(value).trim()

function member(obj: RecordValue, key: string): unknown {
  if (key in obj) return obj[key]
  const expected = key.toLowerCase()
  for (const [candidate, value] of Object.entries(obj)) {
    if (candidate.trim().toLowerCase() === expected) return value
  }
  return undefined
}

function path(obj: RecordValue, segments: string[]): unknown {
  let current: unknown = obj
  for (const segment of segments) {
    if (!isRecord(current)) return undefined
    current = member(current, segment)
    if (current === undefined) return undefined
  }
  return current
}

function first(obj: RecordValue, paths: string[][]): string {
  for (const segments of paths) {
    const value = text(path(obj, segments))
    if (value) return value
  }
  return ''
}

function firstValue(obj: RecordValue, paths: string[][]): unknown {
  for (const segments of paths) {
    const value = path(obj, segments)
    if (value != null && value !== '') return value
  }
  return undefined
}

function claims(token: string): RecordValue {
  const part = token.split('.')[1]
  if (!part) return {}
  try {
    const base64 = part.replace(/-/g, '+').replace(/_/g, '/')
    const padded = base64.padEnd(Math.ceil(base64.length / 4) * 4, '=')
    const decoded = atob(padded)
    return JSON.parse(decoded) as RecordValue
  } catch { return {} }
}

function bearerAuthorization(obj: RecordValue): { value?: string; error?: string } {
  const contexts = [obj, member(obj, 'data')].filter(isRecord)
  for (const context of contexts) {
    const headers = member(context, 'headers')
    const authorization = isRecord(headers) ? text(member(headers, 'authorization')) : ''
    if (!authorization) continue
    const parts = authorization.split(/\s+/)
    if (parts.length !== 2 || parts[0].toLowerCase() !== 'bearer' || !parts[1]) return { error: 'HCPA headers.authorization 必须是 Bearer PAT' }
    return { value: parts[1] }
  }
  return {}
}

function flatten(value: unknown): unknown[] {
  if (Array.isArray(value)) return value.flatMap(flatten)
  if (!isRecord(value)) return [value]
  const accounts = member(value, 'accounts')
  if (Array.isArray(accounts)) return accounts.flatMap(flatten)
  const data = member(value, 'data')
  if (Array.isArray(data)) return data.flatMap(flatten)
  if (isRecord(data)) {
    const nestedAccounts = member(data, 'accounts')
    if (Array.isArray(nestedAccounts)) return nestedAccounts.flatMap(flatten)
  }
  return [value]
}

function recordContexts(raw: RecordValue): RecordValue[] {
  const data = member(raw, 'data')
  return isRecord(data) ? [raw, data] : [raw]
}

function anyFirst(raw: RecordValue, paths: string[][]): string {
  for (const context of recordContexts(raw)) {
    const value = first(context, paths)
    if (value) return value
  }
  return ''
}

function anyFirstValue(raw: RecordValue, paths: string[][]): unknown {
  for (const context of recordContexts(raw)) {
    const value = firstValue(context, paths)
    if (value != null) return value
  }
  return undefined
}

function hasAgentIdentity(raw: RecordValue) {
  return recordContexts(raw).some(context => {
    if (isRecord(member(context, 'agent_identity')) || isRecord(member(context, 'agentIdentity'))) return true
    return ['auth_mode', 'authMode', 'openai_auth_mode', 'openaiAuthMode'].some(key => text(member(context, key)).toLowerCase() === 'agent_identity')
  })
}

function unsupported(reason: string): RecordValue {
  return { __import_error: reason }
}

function rawTokenRow(raw: string, kind: CredentialKind): RecordValue {
  if (raw.startsWith('at-')) {
    return kind === 'codex-pat'
      ? { codex_pat_key: raw }
      : unsupported('检测到 Sub2 PAT（at- 开头）；请按 Codex PAT 类型导入')
  }
  return kind === 'codex-oauth'
    ? { access_token: raw }
    : unsupported('裸 token 不以 at- 开头，Sub2 将其视为 OAuth access_token，不能作为 PAT 导入')
}

function toC3Row(raw: unknown, kind: CredentialKind): unknown {
  if (typeof raw === 'string') return rawTokenRow(raw.trim(), kind)
  if (!isRecord(raw)) return raw
  if (text(raw.__import_error)) return raw
  if (hasAgentIdentity(raw)) return unsupported('Sub2 Agent Identity 无法迁移：C3 仅支持 Codex OAuth 和 Codex PAT')
  if (anyFirst(raw, [['api_key'], ['apiKey'], ['credentials', 'api_key'], ['credentials', 'apiKey']])) {
    return unsupported('Sub2 API Key 账号无法迁移：C3 此导入器仅支持 Codex OAuth 和 Codex PAT')
  }
  const authMode = anyFirst(raw, [['auth_mode'], ['authMode'], ['openai_auth_mode'], ['openaiAuthMode']]).toLowerCase()
  if (authMode === 'setup-token' || authMode === 'setup_token') {
    return unsupported('Sub2 setup-token 账号无法迁移：C3 此导入器仅支持 Codex OAuth 和 Codex PAT')
  }
  if (authMode === 'upstream') {
    return unsupported('Sub2 upstream 账号无法迁移：C3 此导入器仅支持 Codex OAuth 和 Codex PAT')
  }
  const sourceType = anyFirst(raw, [['platform'], ['provider'], ['account_type'], ['accountType'], ['type']]).toLowerCase()
  if (/(anthropic|claude|gemini|vertex|azure)/.test(sourceType)) {
    return unsupported(`Sub2 ${sourceType} 账号无法迁移：C3 此导入器仅支持 Codex OAuth 和 Codex PAT`)
  }

  const header = bearerAuthorization(raw)
  if (header.error) return unsupported(header.error)
  const platform = anyFirst(raw, [['platform']]).toLowerCase()
  const accountType = anyFirst(raw, [['type']]).toLowerCase()
  if (!header.value && platform && platform !== 'openai') {
    return unsupported(`Sub2 ${platform} 账号无法迁移：C3 此导入器仅支持 OpenAI Codex OAuth 和 PAT`)
  }
  if (!header.value && accountType && accountType !== 'oauth') {
    return unsupported(`Sub2 ${accountType} 账号无法迁移：C3 此导入器仅支持 Codex OAuth 和 PAT`)
  }
  const oauthToken = anyFirst(raw, [
    ['credentials', 'access_token'], ['credentials', 'accessToken'], ['tokens', 'access_token'], ['tokens', 'accessToken'], ['access_token'], ['accessToken'], ['token'], ['at'], ['AT'],
  ])
  const refresh = anyFirst(raw, [
    ['credentials', 'refresh_token'], ['credentials', 'refreshToken'], ['tokens', 'refresh_token'], ['tokens', 'refreshToken'], ['refresh_token'], ['refreshToken'], ['rt'], ['RT'],
  ])
  const idToken = anyFirst(raw, [
    ['credentials', 'id_token'], ['credentials', 'idToken'], ['tokens', 'id_token'], ['tokens', 'idToken'], ['id_token'], ['idToken'],
  ])
  const pat = header.value || anyFirst(raw, [
    ['credentials', 'personal_access_token'], ['credentials', 'personalAccessToken'], ['credentials', 'codex_pat_key'], ['personal_access_token'], ['personalAccessToken'], ['codex_pat_key'],
  ])
  const identityToken = idToken || oauthToken
  const tokenClaims = claims(identityToken)
  const authClaim = member(tokenClaims, 'https://api.openai.com/auth')
  const authClaims = isRecord(authClaim) ? authClaim : {}
  const email = anyFirst(raw, [
    ['credentials', 'email'], ['extra', 'email'], ['email'], ['user', 'email'],
  ]) || text(member(tokenClaims, 'email'))
  const accountID = anyFirst(raw, [
    ['credentials', 'chatgpt_account_id'], ['credentials', 'account_id'], ['chatgpt_account_id'], ['chatgptAccountId'], ['account_id'], ['accountId'], ['account', 'id'],
  ]) || text(member(authClaims, 'chatgpt_account_id'))
  const expires = anyFirstValue(raw, [
    ['credentials', 'expires_at'], ['credentials', 'expiresAt'], ['tokens', 'expires_at'], ['tokens', 'expiresAt'],
  ]) ?? member(tokenClaims, 'exp')
  const concurrency = anyFirstValue(raw, [['concurrency'], ['max_concurrency'], ['maxConcurrency'], ['credentials', 'max_concurrency'], ['credentials', 'maxConcurrency']])
  const weight = anyFirstValue(raw, [['weight'], ['credentials', 'weight']])

  if (kind === 'codex-pat') {
    if (!pat) {
      if (oauthToken || refresh || idToken) return unsupported('检测到 OAuth 凭据；PAT 导入不会把 access_token 当作 PAT，请选择 Codex OAuth 类型')
      return unsupported('未找到 Sub2 PAT（personal_access_token、personalAccessToken 或 Bearer Authorization）')
    }
    if (!pat.startsWith('at-')) return unsupported('Sub2 PAT 必须以 at- 开头')
    return {
      email, account_id: accountID, codex_pat_key: pat,
      ...(concurrency === undefined ? {} : { max_concurrency: concurrency }),
      ...(weight === undefined ? {} : { weight }),
    }
  }

  if (pat && header.value) return unsupported('检测到 HCPA PAT；请按 Codex PAT 类型导入')
  if (pat && !oauthToken && !refresh) return unsupported('检测到 Sub2 PAT；请按 Codex PAT 类型导入')
  if (!oauthToken && !refresh && !idToken) return unsupported('未找到可迁移的 Sub2 OAuth 凭据（access_token 与 refresh_token）')
  return {
    email, account_id: accountID, access_token: oauthToken, refresh_token: refresh, expired: expires,
    ...(concurrency === undefined ? {} : { max_concurrency: concurrency }),
    ...(weight === undefined ? {} : { weight }),
  }
}

export function parseSub2API(text: string, kind: CredentialKind): { rows: NormalizedRow[]; parseError?: string } {
  const parsed = parseRawText(text)
  if (parsed.error) return { rows: [], parseError: parsed.error }
  const sourceRows = parsed.rows.flatMap(flatten)
  return { rows: sourceRows.map((raw, index) => normalizeRow(toC3Row(raw, kind), kind, index)) }
}
