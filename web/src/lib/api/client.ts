// 薄 fetch 封装：token 注入、401 归一化、类型化返回（schema.d.ts 生成）。
// 响应字段为 Go 大写风格（ID/Name/...），前端按此使用，不做 camelCase 转换。
import type { components } from './schema.d.ts'
import { userAuth } from '@/lib/auth'

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

// —— 用户端日志/统计查询参数 ——
export interface UserLogParams {
  limit?: number
  offset?: number
  group_id?: number
  account_id?: number
  model?: string
  status_code?: number
  error_type?: string
  from?: string
  to?: string
}
export interface UserStatParams {
  from?: string
  to?: string
  granularity?: 'hour' | 'day'
  group_id?: number
  account_id?: number
  model?: string
}

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

// base：管理端 '/admin' / 用户端 '/user'；token 统一由 userAuth 注入（一套登录态）。
export class ApiClient {
  private base: string
  private getToken: () => string | null
  constructor(getToken: () => string | null, base: string = '/admin') {
    this.getToken = getToken
    this.base = base
  }

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
    // DELETE /rules/{id} 等返回 204 无 body，不能 res.json()
    if (res.status === 204) return undefined as T
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
  createGroup = (b: components['schemas']['GroupCreate']) => this.request<components['schemas']['Group']>('/groups', { method: 'POST', body: JSON.stringify(b) })
  updateGroup = (id: number, b: components['schemas']['GroupCreate']) => this.request<components['schemas']['Group']>(`/groups/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteGroup = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/groups/${id}`, { method: 'DELETE' })
  deleteGroupsBatch = (ids: number[]) => this.request<components['schemas']['BatchDeleteResponse']>('/groups/batch-delete', { method: 'POST', body: JSON.stringify({ ids }) })
  updateGroupsBatch = (ids: number[], fields: components['schemas']['GroupPatch']) => this.request<components['schemas']['BatchUpdateResponse']>('/groups/batch-update', { method: 'POST', body: JSON.stringify({ ids, fields }) })
  getAccountGroups = (id: number) => this.request<components['schemas']['AccountGroupsResponse']>(`/accounts/${id}/groups`)
  // —— 规则 ——
  listRules = (p?: { enabled?: boolean }) => this.request<components['schemas']['RuleListResponse']>('/rules', { params: toQuery(p) })
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
  // —— 用户管理 ——
  listUsers = (p?: { limit?: number; offset?: number; email?: string; sort?: string; order?: 'asc' | 'desc' }) => this.request<components['schemas']['UserListResponse']>('/users', { params: toQuery(p) })
  createUser = (b: components['schemas']['UserCreate']) => this.request<components['schemas']['User']>('/users', { method: 'POST', body: JSON.stringify(b) })
  updateUser = (id: number, b: components['schemas']['UserUpdate']) => this.request<components['schemas']['User']>(`/users/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  setGroupAssignments = (id: number, b: components['schemas']['GroupAssignmentsBody']) => this.request<components['schemas']['GroupAssignmentsResponse']>(`/groups/${id}/assignments`, { method: 'PUT', body: JSON.stringify(b) })
  getGroupAssignments = (id: number) => this.request<components['schemas']['GroupAssignmentsResponse']>(`/groups/${id}/assignments`)
  getUserGroups = (id: number) => this.request<components['schemas']['UserGroupsResponse']>(`/users/${id}/groups`)
  setUserGroups = (id: number, b: components['schemas']['UserGroupsBody']) => this.request<components['schemas']['UserGroupsResponse']>(`/users/${id}/groups`, { method: 'PUT', body: JSON.stringify(b) })
  // —— 设置 ——
  getSettings = () => this.request<components['schemas']['Setting'][]>('/settings')
  updateSetting = (b: components['schemas']['SettingUpdate']) => this.request<components['schemas']['Setting'][]>('/settings', { method: 'PUT', body: JSON.stringify(b) })
  // —— 兑换码 ——
  listRedemptionCodes = (p?: { page?: number; page_size?: number; type?: string; status?: string; sort?: string; order?: 'asc' | 'desc' }) => this.request<components['schemas']['RedemptionCodeListResponse']>('/redemption-codes', { params: toQuery(p) })
  generateRedemptionCodes = (b: components['schemas']['GenerateRequest']) => this.request<components['schemas']['GenerateResponse']>('/redemption-codes', { method: 'POST', body: JSON.stringify(b) })
  deactivateRedemptionCode = (id: number) => this.request<components['schemas']['DeactivateResponse']>(`/redemption-codes/${id}/deactivate`, { method: 'POST' })
  deactivateRedemptionCodesBatch = (ids: number[]) => this.request<components['schemas']['BatchDeactivateResponse']>('/redemption-codes/batch-deactivate', { method: 'POST', body: JSON.stringify({ ids }) })
  getRedemptionCodeUses = (id: number) => this.request<components['schemas']['RedemptionUseListResponse']>(`/redemption-codes/${id}/uses`)
  // —— 定价 ——
  listPricing = (p?: { page?: number; page_size?: number; source?: string; model?: string; sort?: string; order?: 'asc' | 'desc' }) => this.request<components['schemas']['PricingListResponse']>('/pricing', { params: toQuery(p) })
  syncPricing = () => this.request<components['schemas']['PricingSyncResponse']>('/pricing/sync', { method: 'POST' })
  upsertPricing = (model: string, b: components['schemas']['PricingUpsert']) => this.request<components['schemas']['Pricing']>(`/pricing/${model}`, { method: 'PUT', body: JSON.stringify(b) })
  deletePricing = (model: string) => this.request<components['schemas']['DeletedResponse']>(`/pricing/${model}`, { method: 'DELETE' })
  // —— 用户端（userApi 专属；token 用 userAuth）——
  register = (b: components['schemas']['UserAuthRegister']) => this.request<components['schemas']['UserAuthResponse']>('/auth/register', { method: 'POST', body: JSON.stringify(b) })
  login = (b: components['schemas']['UserAuthLogin']) => this.request<components['schemas']['UserAuthResponse']>('/auth/login', { method: 'POST', body: JSON.stringify(b) })
  me = () => this.request<components['schemas']['User']>('/auth/me')
  listUserGroups = () => this.request<components['schemas']['Group'][]>('/groups')
  listUserKeys = (p?: ListParams) => this.request<components['schemas']['KeyListResponse']>('/keys', { params: toQuery(p) })
  createUserKey = (b: components['schemas']['KeyCreate']) => this.request<components['schemas']['KeyWithSecret']>('/keys', { method: 'POST', body: JSON.stringify(b) })
  updateUserKey = (id: number, b: components['schemas']['KeyUpdate']) => this.request<components['schemas']['Key']>(`/keys/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteUserKey = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/keys/${id}`, { method: 'DELETE' })
  rotateUserKey = (id: number) => this.request<components['schemas']['KeyWithSecret']>(`/keys/${id}/rotate`, { method: 'POST' })
  getUserLogs = (params?: UserLogParams) => this.request<components['schemas']['LogsResponse']>('/logs', { params: toQuery(params) })
  getUserStats = (params?: UserStatParams) => this.request<components['schemas']['StatBucket'][]>('/stats', { params: toQuery(params) })
  redeem = (code: string) => this.request<components['schemas']['RedeemResponse']>('/redemptions', { method: 'POST', body: JSON.stringify({ code }) })
  listUserRedemptions = (p?: { page?: number; page_size?: number; sort?: string; order?: 'asc' | 'desc' }) => this.request<components['schemas']['RedemptionRecordListResponse']>('/redemptions', { params: toQuery(p) })
}

export class ApiUnauthorized extends Error {
  constructor() { super('unauthorized'); this.name = 'ApiUnauthorized' }
}

// 用户端实例：base '/user'，Authorization 走 userAuth（gpm_user_token）。
export const userApi = new ApiClient(userAuth.getToken, '/user')
