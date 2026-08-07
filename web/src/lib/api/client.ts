// 薄 fetch 封装：token 注入、401 归一化、类型化返回（schema.d.ts 生成）。
// 响应字段为 Go 大写风格（ID/Name/...），前端按此使用，不做 camelCase 转换。
import type { components } from './schema.d.ts'

// 类实现（brief 原为 type 别名，但 throw new ApiError(...) 需要运行时值）
export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
    this.name = 'ApiError'
  }
}

// —— 列表查询参数（三页通用 + 专属）——
export interface ListParams {
  limit?: number
  offset?: number
  name?: string
  sort?: string
  order?: 'asc' | 'desc'
}
export type TemplateListParams = ListParams
export interface AccountListParams extends ListParams {
  status?: string
  template_id?: number
}
export type GroupListParams = ListParams

// 过滤 undefined/null/空串，返回 '' 或 '?k=v&...' 查询串。
export function toQuery(p?: object): string {
  if (!p) return ''
  const qs = new URLSearchParams()
  for (const [k, v] of Object.entries(p)) {
    if (v === undefined || v === null || v === '') continue
    qs.set(k, String(v))
  }
  const s = qs.toString()
  return s ? `?${s}` : ''
}

export class ApiClient {
  private base = '/admin'
  private getToken: () => string | null
  constructor(getToken: () => string | null) { this.getToken = getToken }

  // init.params 为 toQuery 产出的查询串（'' 或 '?k=v&...'），附加到 path 之后。
  private async request<T>(path: string, init?: RequestInit & { params?: string }): Promise<T> {
    const token = this.getToken()
    const { params, ...rest } = init ?? {}
    const headers: Record<string, string> = { 'Content-Type': 'application/json', ...(rest.headers as Record<string, string> | undefined) }
    if (token) headers['Authorization'] = `Bearer ${token}`
    const qs = params ?? ''
    const url = `${this.base}${path}${qs ? (qs.startsWith('?') ? qs : `?${qs}`) : ''}`
    const res = await fetch(url, { ...rest, headers })
    if (res.status === 401) throw new ApiUnauthorized()
    if (!res.ok) {
      const body = await res.json().catch(() => null)
      throw new ApiError(res.status, (body as { error?: string } | null)?.error ?? `HTTP ${res.status}`)
    }
    return res.json() as Promise<T>
  }
  // —— 模板 ——
  listTemplates = (p?: TemplateListParams) => this.request<components['schemas']['TemplateListResponse']>('/templates', { params: toQuery(p) })
  createTemplate = (b: components['schemas']['TemplateCreate']) => this.request<components['schemas']['Template']>('/templates', { method: 'POST', body: JSON.stringify(b) })
  updateTemplate = (id: number, b: components['schemas']['TemplateCreate']) => this.request<components['schemas']['Template']>(`/templates/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteTemplate = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/templates/${id}`, { method: 'DELETE' })
  deleteTemplatesBatch = (ids: number[]) => this.request<components['schemas']['BatchDeleteResponse']>('/templates/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) })
  updateTemplatesBatch = (ids: number[], fields: components['schemas']['TemplatePatch']) => this.request<components['schemas']['BatchUpdateResponse']>('/templates/batch-update', { method: 'POST', body: JSON.stringify({ ids, fields }) })
  // —— 账号 ——
  listAccounts = (p?: AccountListParams) => this.request<components['schemas']['AccountListResponse']>('/accounts', { params: toQuery(p) })
  createAccount = (b: components['schemas']['AccountCreate']) => this.request<components['schemas']['Account']>('/accounts', { method: 'POST', body: JSON.stringify(b) })
  updateAccount = (id: number, b: components['schemas']['AccountCreate']) => this.request<components['schemas']['Account']>(`/accounts/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteAccount = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/accounts/${id}`, { method: 'DELETE' })
  deleteAccountsBatch = (ids: number[]) => this.request<components['schemas']['BatchDeleteResponse']>('/accounts/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) })
  updateAccountsBatch = (ids: number[], fields: components['schemas']['AccountPatch']) => this.request<components['schemas']['BatchUpdateResponse']>('/accounts/batch-update', { method: 'POST', body: JSON.stringify({ ids, fields }) })
  // —— 分组 ——
  listGroups = (p?: GroupListParams) => this.request<components['schemas']['GroupListResponse']>('/groups', { params: toQuery(p) })
  createGroup = (b: components['schemas']['GroupCreate']) => this.request<components['schemas']['CreateGroupResponse']>('/groups', { method: 'POST', body: JSON.stringify(b) })
  updateGroup = (id: number, b: components['schemas']['GroupCreate']) => this.request<components['schemas']['Group']>(`/groups/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteGroup = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/groups/${id}`, { method: 'DELETE' })
  deleteGroupsBatch = (ids: number[]) => this.request<components['schemas']['BatchDeleteResponse']>('/groups/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) })
  updateGroupsBatch = (ids: number[], fields: components['schemas']['GroupPatch']) => this.request<components['schemas']['BatchUpdateResponse']>('/groups/batch-update', { method: 'POST', body: JSON.stringify({ ids, fields }) })
  setGroupAccounts = (id: number, accountIds: number[]) => this.request<components['schemas']['UpdatedResponse']>(`/groups/${id}/accounts`, { method: 'PUT', body: JSON.stringify({ account_ids: accountIds }) })
  rotateGroupKey = (id: number) => this.request<components['schemas']['RotateKeyResponse']>(`/groups/${id}/rotate-key`, { method: 'POST' })
  // —— 规则 ——
  listRules = (p?: ListParams & { enabled?: boolean }) => this.request<components['schemas']['RuleListResponse']>('/rules', { params: toQuery(p) })
  createRule = (b: components['schemas']['RuleCreate']) => this.request<components['schemas']['Rule']>('/rules', { method: 'POST', body: JSON.stringify(b) })
  updateRule = (id: number, b: components['schemas']['RulePatch']) => this.request<components['schemas']['Rule']>(`/rules/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteRule = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/rules/${id}`, { method: 'DELETE' })
  deleteRulesBatch = (ids: number[]) => this.request<components['schemas']['BatchDeleteResponse']>('/rules/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) })
  // —— 日志 / 统计 ——
  getLogs = (params: Record<string, string | number | undefined>) => {
    const qs = new URLSearchParams()
    for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== '') qs.set(k, String(v))
    const s = qs.toString()
    return this.request<components['schemas']['LogsResponse']>(`/logs${s ? `?${s}` : ''}`)
  }
  getStats = (params: Record<string, string | number | undefined>) => {
    const qs = new URLSearchParams()
    for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== '') qs.set(k, String(v))
    const s = qs.toString()
    return this.request<components['schemas']['StatBucket'][]>(`/stats${s ? `?${s}` : ''}`)
  }
}

export class ApiUnauthorized extends Error {
  constructor() { super('unauthorized'); this.name = 'ApiUnauthorized' }
}
