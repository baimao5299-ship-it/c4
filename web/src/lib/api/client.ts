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

export class ApiClient {
  private base = '/admin'
  private getToken: () => string | null
  constructor(getToken: () => string | null) { this.getToken = getToken }

  private async request<T>(path: string, init?: RequestInit): Promise<T> {
    const token = this.getToken()
    const headers: Record<string, string> = { 'Content-Type': 'application/json', ...(init?.headers as Record<string, string> | undefined) }
    if (token) headers['Authorization'] = `Bearer ${token}`
    const res = await fetch(`${this.base}${path}`, { ...init, headers })
    if (res.status === 401) throw new ApiUnauthorized()
    if (!res.ok) {
      const body = await res.json().catch(() => null)
      throw new ApiError(res.status, (body as { error?: string } | null)?.error ?? `HTTP ${res.status}`)
    }
    return res.json() as Promise<T>
  }
  // —— 模板 ——
  listTemplates = () => this.request<components['schemas']['Template'][]>('/templates')
  createTemplate = (b: components['schemas']['TemplateCreate']) => this.request<components['schemas']['Template']>('/templates', { method: 'POST', body: JSON.stringify(b) })
  updateTemplate = (id: number, b: components['schemas']['TemplateCreate']) => this.request<components['schemas']['Template']>(`/templates/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteTemplate = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/templates/${id}`, { method: 'DELETE' })
  // —— 账号 ——
  listAccounts = () => this.request<components['schemas']['AccountView'][]>('/accounts')
  createAccount = (b: components['schemas']['AccountCreate']) => this.request<components['schemas']['Account']>('/accounts', { method: 'POST', body: JSON.stringify(b) })
  updateAccount = (id: number, b: components['schemas']['AccountCreate']) => this.request<components['schemas']['Account']>(`/accounts/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteAccount = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/accounts/${id}`, { method: 'DELETE' })
  // —— 分组 ——
  listGroups = () => this.request<components['schemas']['Group'][]>('/groups')
  createGroup = (b: components['schemas']['GroupCreate']) => this.request<components['schemas']['CreateGroupResponse']>('/groups', { method: 'POST', body: JSON.stringify(b) })
  updateGroup = (id: number, b: components['schemas']['GroupCreate']) => this.request<components['schemas']['Group']>(`/groups/${id}`, { method: 'PUT', body: JSON.stringify(b) })
  deleteGroup = (id: number) => this.request<components['schemas']['DeletedResponse']>(`/groups/${id}`, { method: 'DELETE' })
  setGroupAccounts = (id: number, accountIds: number[]) => this.request<components['schemas']['UpdatedResponse']>(`/groups/${id}/accounts`, { method: 'PUT', body: JSON.stringify({ account_ids: accountIds }) })
  rotateGroupKey = (id: number) => this.request<components['schemas']['RotateKeyResponse']>(`/groups/${id}/rotate-key`, { method: 'POST' })
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
