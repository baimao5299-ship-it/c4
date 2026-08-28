import { normalizeRow, type CredentialKind, type NormalizedRow } from './normalize'
import { parseRawText } from './parse'

type RecordValue = Record<string, unknown>
const MAX_IMPORT_NESTING = 64

const isRecord = (value: unknown): value is RecordValue => !!value && typeof value === 'object' && !Array.isArray(value)
const text = (value: unknown) => typeof value === 'string' ? value.trim() : value == null ? '' : String(value).trim()

// Sub2 exports have appeared with snake_case, camelCase, and occasionally
// hyphenated keys.  Comparing a canonical spelling keeps the adapter tolerant
// without having to duplicate every alias in every path.
function canonicalKey(key: string): string {
  return key.trim().toLowerCase().replace(/[\s_-]/g, '')
}

function member(obj: RecordValue, key: string): unknown {
  let exact: unknown
  if (key in obj) {
    exact = obj[key]
    // Prefer an explicitly named non-empty value, but keep looking when a
    // duplicate export contains an empty snake_case field and a populated
    // camelCase alias.
    if (exact !== '' && exact != null) return exact
  }
  const expected = canonicalKey(key)
  for (const [candidate, value] of Object.entries(obj)) {
    if (canonicalKey(candidate) === expected && value !== '' && value != null) return value
  }
  return exact
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

function looksLikeAccount(value: RecordValue): boolean {
  const containers = ['credentials', 'auth', 'tokens', 'credential']
  if (containers.some(key => isRecord(member(value, key)))) return true
  return [
    'access_token', 'accessToken', 'refresh_token', 'refreshToken', 'id_token', 'idToken',
    'session_token', 'sessionToken', 'api_key', 'apiKey', 'personal_access_token',
    'personalAccessToken', 'pat', 'pat_key', 'patKey', 'codex_pat_key', 'codex_oauth_token', 'codex_oauth_refresh_token',
  ].some(key => text(member(value, key)) !== '')
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
  const contexts = recordContexts(obj)
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

function flatten(value: unknown, depth = 0): unknown[] {
  if (depth > MAX_IMPORT_NESTING) return [{ __import_error: `导入嵌套层级超过 ${MAX_IMPORT_NESTING}` }]
  if (Array.isArray(value)) return value.flatMap(item => flatten(item, depth + 1))
  if (!isRecord(value)) return [value]
  // Sub2 exports use several wrappers depending on the endpoint/version.
  // Only unwrap an object-valued key when the parent is not already an account.
  for (const key of ['accounts', 'items', 'results', 'records', 'record', 'account', 'data']) {
    const nested = member(value, key)
    if (Array.isArray(nested)) return nested.flatMap(item => flatten(item, depth + 1))
    if (isRecord(nested) && !looksLikeAccount(value)) {
      const flattened = flatten(nested, depth + 1)
      if (flattened.length > 0) return flattened
    }
  }
  return [value]
}

function recordContexts(raw: RecordValue): RecordValue[] {
  const data = member(raw, 'data')
  const nested = [
    member(raw, 'credentials'), member(raw, 'auth'), member(raw, 'tokens'), member(raw, 'credential'),
    member(raw, 'profile'), member(raw, 'user'), member(raw, 'account'), member(raw, 'subscription'),
    member(raw, 'metadata'), member(raw, 'meta'), member(raw, 'provider_specific_data'),
    member(raw, 'providerSpecificData'), member(raw, 'identity'),
  ].filter(isRecord)
  return isRecord(data) ? [raw, data, ...nested] : [raw, ...nested]
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
  if (kind !== 'codex-oauth') return unsupported('裸 token 不以 at- 开头，Sub2 将其视为 OAuth access_token，不能作为 PAT 导入')

  // Sub2 accepts one accessToken per line. Decode the JWT payload when
  // available so C3's required identity fields are filled automatically;
  // opaque tokens still produce a precise missing-identity error in preview.
  const tokenClaims = claims(raw)
  const authClaim = member(tokenClaims, 'https://api.openai.com/auth')
  const authClaims = isRecord(authClaim) ? authClaim : {}
  const email = text(member(tokenClaims, 'email'))
  const accountID = text(member(authClaims, 'chatgpt_account_id')) || text(member(tokenClaims, 'chatgpt_account_id')) || text(member(tokenClaims, 'account_id'))
  const expires = member(tokenClaims, 'exp')
  return {
    access_token: raw,
    ...(email ? { email } : {}),
    ...(accountID ? { account_id: accountID } : {}),
    ...(expires != null ? { expired: expires } : {}),
  }
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
  if (!header.value && platform && !['openai', 'openai-codex', 'codex'].includes(platform)) {
    return unsupported(`Sub2 ${platform} 账号无法迁移：C3 此导入器仅支持 OpenAI Codex OAuth 和 PAT`)
  }
  if (!header.value && accountType && !['oauth', 'codex-oauth', 'pat', 'codex-pat'].includes(accountType)) {
    return unsupported(`Sub2 ${accountType} 账号无法迁移：C3 此导入器仅支持 Codex OAuth 和 PAT`)
  }
  const oauthToken = anyFirst(raw, [
    ['credentials', 'access_token'], ['credentials', 'accessToken'], ['tokens', 'access_token'], ['tokens', 'accessToken'], ['auth', 'access_token'], ['auth', 'accessToken'], ['credential', 'access_token'], ['oauth_token'], ['oauthToken'], ['access_token'], ['accessToken'], ['codex_oauth_token'], ['token'], ['at'], ['AT'],
  ])
  const refresh = anyFirst(raw, [
    ['credentials', 'refresh_token'], ['credentials', 'refreshToken'], ['tokens', 'refresh_token'], ['tokens', 'refreshToken'], ['auth', 'refresh_token'], ['auth', 'refreshToken'], ['credential', 'refresh_token'], ['oauth_refresh_token'], ['oauthRefreshToken'], ['refresh_token'], ['refreshToken'], ['codex_oauth_refresh_token'], ['rt'], ['RT'],
  ])
  const idToken = anyFirst(raw, [
    ['credentials', 'id_token'], ['credentials', 'idToken'], ['tokens', 'id_token'], ['tokens', 'idToken'], ['auth', 'id_token'], ['auth', 'idToken'], ['id_token'], ['idToken'], ['identity_token'], ['identityToken'],
  ])
  const pat = header.value || anyFirst(raw, [
    ['credentials', 'personal_access_token'], ['credentials', 'personalAccessToken'], ['credentials', 'codex_pat_key'], ['credentials', 'pat_key'], ['credential', 'personal_access_token'], ['credential', 'personalAccessToken'], ['personal_access_token'], ['personalAccessToken'], ['pat_key'], ['patKey'], ['pat'], ['codex_pat_key'],
  ])
  const identityToken = idToken || oauthToken
  const tokenClaims = claims(identityToken)
  const authClaim = member(tokenClaims, 'https://api.openai.com/auth')
  const authClaims = isRecord(authClaim) ? authClaim : {}
  const email = anyFirst(raw, [
    ['credentials', 'email'], ['auth', 'email'], ['tokens', 'email'], ['extra', 'email'], ['profile', 'email'], ['metadata', 'email'], ['email'], ['codex_email'], ['user', 'email'],
  ]) || text(member(tokenClaims, 'email'))
  const accountID = anyFirst(raw, [
    ['credentials', 'chatgpt_account_id'], ['credentials', 'chatgptAccountId'], ['credentials', 'account_id'], ['credentials', 'accountId'], ['chatgpt_account_id'], ['chatgptAccountId'], ['account_id'], ['accountId'], ['codex_account_id'], ['account', 'id'], ['account', 'account_id'], ['account', 'accountId'], ['account', 'chatgpt_account_id'], ['account', 'chatgptAccountId'],
  ]) || text(member(authClaims, 'chatgpt_account_id'))
  const expires = anyFirstValue(raw, [
    ['credentials', 'expires_at'], ['credentials', 'expiresAt'], ['credentials', 'codex_oauth_expires_at'], ['tokens', 'expires_at'], ['tokens', 'expiresAt'], ['auth', 'expires_at'], ['oauth_expires_at'], ['oauthExpiresAt'], ['codex_oauth_expires_at'], ['token_expires_at'], ['tokenExpiresAt'], ['expired'], ['expires'], ['expiresAt'],
  ]) ?? member(tokenClaims, 'exp')
  const concurrency = anyFirstValue(raw, [['concurrency'], ['max_concurrency'], ['maxConcurrency'], ['credentials', 'max_concurrency'], ['credentials', 'maxConcurrency']])
  const weight = anyFirstValue(raw, [['weight'], ['credentials', 'weight']])

  if (kind === 'codex-pat') {
    if (!pat) {
      if (oauthToken || refresh || idToken) return unsupported('检测到 OAuth 凭据；PAT 导入不会把 access_token 当作 PAT，请选择 Codex OAuth 类型')
      return unsupported('未找到 Sub2 PAT（personal_access_token、personalAccessToken 或 Bearer Authorization）')
    }
    // Sub2's full account export may include an expired accessToken next to a
    // PAT.  The PAT path intentionally ignores that OAuth metadata, matching
    // the mature importer and preventing a stale AT from replacing the PAT.
    return {
      email, account_id: accountID, codex_pat_key: pat,
      ...(concurrency === undefined ? {} : { max_concurrency: concurrency }),
      ...(weight === undefined ? {} : { weight }),
    }
  }

  if (pat) {
    if (header.value) return unsupported('检测到 HCPA PAT；请按 Codex PAT 类型导入')
    if (!oauthToken && !refresh && !idToken) return unsupported('检测到 Sub2 PAT；请按 Codex PAT 类型导入')
    return unsupported('同一导入项同时包含 PAT 和 OAuth 凭据；请拆分后选择正确的凭据类型')
  }
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
