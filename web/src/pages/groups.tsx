// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useMemo, useRef, useState } from 'react'
import type { Dispatch, SetStateAction } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, FolderOpen, Filter, UserPlus, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { BatchBar } from '@/components/batch-bar'
import { ListToolbar } from '@/components/list-toolbar'
import { Pagination } from '@/components/pagination'
import { SortableHeader, type SortOrder } from '@/components/sortable-header'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Badge } from '@/components/ui/badge'
import { toast } from '@/components/ui/toast'
import { formatDateTime } from '@/components/fmt'
import { cn } from '@/lib/utils'
import { sortModelsLatestFirst } from '@/lib/model-sort'
import { formatMultiplierValue, isStorableMultiplier } from '@/lib/multiplier'
import { ModelValidationProgress } from '@/components/model-validation-progress'
import type { TFunction } from 'i18next'
import type { components } from '@/lib/api/schema'

type Group = components['schemas']['Group']
type GroupVisibility = components['schemas']['GroupVisibility']
type GroupPublicStatus = components['schemas']['GroupPublicStatus']
type GroupRoutingMode = components['schemas']['GroupRoutingMode']
type GroupUpstreamInput = components['schemas']['GroupUpstreamInput']
type Upstream = components['schemas']['Upstream']
type GroupProtocolConvert = components['schemas']['GroupProtocolConvert']
type GroupAssignmentsBody = components['schemas']['GroupAssignmentsBody']

interface UpstreamMemberDraft {
  upstream_id: number
  priority: string
  weight: string
  max_concurrency: string
  enabled: boolean
}

const defaultUpstreamMember = (upstream_id: number): UpstreamMemberDraft => ({
  upstream_id,
  priority: '0',
  weight: '100',
  max_concurrency: '8',
  enabled: true,
})

function makeAutoGroupName(prefix: string): string {
  const uuid = globalThis.crypto?.randomUUID?.()
  const suffix = uuid
    ? uuid.slice(0, 8)
    : `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`
  return `${prefix}-${suffix}`
}

const draftFromUpstream = (member: components['schemas']['GroupUpstream']): UpstreamMemberDraft => ({
  upstream_id: member.UpstreamID,
  priority: String(member.Priority),
  weight: String(member.Weight),
  max_concurrency: String(member.MaxConcurrency),
  enabled: member.Enabled,
})

function serializeUpstreamMembers(drafts: UpstreamMemberDraft[]): GroupUpstreamInput[] {
  return drafts.map(draft => {
    const priority = Number(draft.priority)
    const weight = Number(draft.weight)
    const maxConcurrency = Number(draft.max_concurrency)
    if (!Number.isInteger(priority) || priority < 0 || priority > 100_000) throw new Error('Priority must be an integer from 0 to 100000')
    if (!Number.isInteger(weight) || weight < 1 || weight > 10_000) throw new Error('Weight must be an integer from 1 to 10000')
    if (!Number.isInteger(maxConcurrency) || maxConcurrency < 1 || maxConcurrency > 100_000) throw new Error('Max concurrency must be an integer from 1 to 100000')
    return { upstream_id: draft.upstream_id, priority, weight, max_concurrency: maxConcurrency, enabled: draft.enabled }
  })
}

function hasUsableUpstreamModelSnapshot(upstream: Upstream): boolean {
  return upstream.ModelsCheckedAt != null && Array.isArray(upstream.Models) && upstream.Models.length > 0
}

function UpstreamPoolFields({
  mode,
  showMode = true,
  showMemberOptions = true,
  onModeChange,
  upstreams,
  upstreamsLoading,
  upstreamsError,
  onRetryUpstreams,
  members,
  onToggle,
  onUpdate,
  models,
  modelsLoading,
  modelsError,
  onRetryModels,
  configError,
  allowedModels,
  onToggleModel,
  onSelectAllModels,
}: {
  mode: GroupRoutingMode
  showMode?: boolean
  showMemberOptions?: boolean
  onModeChange: (mode: GroupRoutingMode) => void
  upstreams: Upstream[]
  upstreamsLoading: boolean
  upstreamsError: boolean
  onRetryUpstreams: () => void
  members: UpstreamMemberDraft[]
  onToggle: (id: number, checked: boolean) => void
  onUpdate: (id: number, patch: Partial<UpstreamMemberDraft>) => void
  models: string[]
  modelsLoading: boolean
  modelsError: boolean
  onRetryModels?: () => void
  configError?: boolean
  allowedModels: string[]
  onToggleModel: (model: string, checked: boolean) => void
  onSelectAllModels: () => void
}) {
  const { t } = useTranslation()
  const selected = new Set(members.map(member => member.upstream_id))
  // Only show models confirmed by the current upstream catalogue. Keeping
  // stale values visible made it possible to save a model after replacing a
  // member, even though no selected upstream advertised it anymore.
  const options = models
  return (
    <div className="space-y-3 rounded-lg border bg-muted/20 p-3">
      {!showMode && (
        <div className="flex items-center justify-between gap-3 rounded-md border border-primary/20 bg-primary/5 px-3 py-2 text-sm">
          <span className="font-medium">{t('groups.routingModeLabel')}</span>
          <Badge variant="secondary">{t('groups.routingModes.upstreams')}</Badge>
        </div>
      )}
      {showMode && <div className="space-y-1.5">
          <Label>{t('groups.routingModeLabel')}</Label>
          <Select
            items={Object.fromEntries((['accounts', 'upstreams'] as GroupRoutingMode[]).map(value => [value, t(`groups.routingModes.${value}`)]))}
            value={mode}
            onValueChange={value => onModeChange(value as GroupRoutingMode)}
          >
            <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="accounts" label={t('groups.routingModes.accounts')}>{t('groups.routingModes.accounts')}</SelectItem>
              <SelectItem value="upstreams" label={t('groups.routingModes.upstreams')}>{t('groups.routingModes.upstreams')}</SelectItem>
            </SelectContent>
          </Select>
          <p className="text-xs text-muted-foreground">{t(`groups.routingModeHint.${mode}`)}</p>
        </div>}
      {mode !== 'upstreams' ? null : (
        <>
          <div className="space-y-1.5">
            <div className="flex items-center justify-between gap-2">
              <Label>{t('groups.upstreamMembersLabel')}</Label>
              <span className="text-xs text-muted-foreground">{t('groups.upstreamMembersCount', { count: members.length })}</span>
            </div>
            {upstreamsLoading ? (
              <p className="rounded-md border border-dashed px-3 py-2 text-sm text-muted-foreground">{t('groups.loadingUpstreams')}</p>
            ) : upstreamsError ? (
              <div className="flex items-center justify-between gap-2 rounded-md border border-dashed border-destructive/50 px-3 py-2 text-sm text-destructive">
                <span>{t('groups.upstreamsLoadFailed')}</span>
                <Button type="button" variant="ghost" size="sm" onClick={onRetryUpstreams}>{t('groups.retryUpstreams')}</Button>
              </div>
            ) : upstreams.length === 0 ? (
              <p className="rounded-md border border-dashed px-3 py-2 text-sm text-muted-foreground">{t('groups.noUpstreams')}</p>
            ) : (
              <div className="max-h-52 space-y-2 overflow-y-auto pr-1">
                {upstreams.map(upstream => {
                  const id = upstream.ID!
                  const member = members.find(item => item.upstream_id === id)
                  return (
                    <div key={id} className="space-y-2 rounded-md border bg-background p-2">
                      <div className="flex items-center gap-2 text-sm">
                        <Checkbox checked={selected.has(id)} disabled={!selected.has(id) && upstream.Status !== 'active'} onCheckedChange={checked => onToggle(id, checked === true)} />
                        <span className="min-w-0 flex-1 truncate font-medium" title={upstream.Name}>{upstream.Name || `#${id}`}</span>
                        <span className="text-xs text-muted-foreground">#{id}</span>
                        {upstream.Status !== 'active' && <Badge variant="outline" className="text-xs">{t('groups.upstreamDisabled')}</Badge>}
                        {member && showMemberOptions && <Switch checked={member.enabled} onCheckedChange={enabled => onUpdate(id, { enabled })} aria-label={t(member.enabled ? 'groups.disableMember' : 'groups.enableMember')} />}
                      </div>
                      {member && showMemberOptions && (
                        <div className="grid grid-cols-3 gap-2 pl-6">
                          <div className="space-y-1"><Label className="text-xs">{t('groups.priorityLabel')}</Label><Input type="number" min={0} max={100000} value={member.priority} onChange={event => onUpdate(id, { priority: event.target.value })} className="h-8" /></div>
                          <div className="space-y-1"><Label className="text-xs">{t('groups.weightLabel')}</Label><Input type="number" min={1} max={10000} value={member.weight} onChange={event => onUpdate(id, { weight: event.target.value })} className="h-8" /></div>
                          <div className="space-y-1"><Label className="text-xs">{t('groups.maxConcurrencyLabel')}</Label><Input type="number" min={1} max={100000} value={member.max_concurrency} onChange={event => onUpdate(id, { max_concurrency: event.target.value })} className="h-8" /></div>
                        </div>
                      )}
                    </div>
                  )
                })}
              </div>
            )}
            {showMemberOptions && selected.size > 0 && (
              <p className="text-xs text-muted-foreground">{t('groups.memberOptionsHint')}</p>
            )}
          </div>
          <div className="space-y-1.5">
            <div className="flex items-center justify-between gap-2">
              <Label>{t('groups.allowedModelsLabel')}</Label>
              <Button type="button" variant="ghost" size="sm" onClick={onSelectAllModels} disabled={modelsLoading || options.length === 0}>{t('groups.selectAllModels')}</Button>
            </div>
            {modelsLoading ? <ModelValidationProgress /> : modelsError ? (
              <div className="flex items-center justify-between gap-2 text-xs text-amber-700 dark:text-amber-400">
                <span>{t('groups.modelsReadPartial')}</span>
                {onRetryModels && <Button type="button" variant="ghost" size="sm" className="h-7 px-2 text-xs" onClick={onRetryModels}>{t('groups.retryModels')}</Button>}
              </div>
            ) : configError ? (
              <div className="flex items-center justify-between gap-2 text-xs text-destructive">
                <span>{t('groups.upstreamConfigReadFailed')}</span>
                {onRetryModels && <Button type="button" variant="ghost" size="sm" className="h-7 px-2 text-xs" onClick={onRetryModels}>{t('groups.retryModels')}</Button>}
              </div>
            ) : null}
            {options.length === 0 ? (
              <p className="rounded-md border border-dashed px-3 py-2 text-xs text-muted-foreground">{t('groups.noModels')}</p>
            ) : (
              <div className="grid max-h-44 gap-1.5 overflow-y-auto sm:grid-cols-2">
                {options.map(model => (
                  <label key={model} className="flex min-w-0 cursor-pointer items-center gap-2 rounded-md border bg-background px-2 py-1.5 text-xs">
                    <Checkbox checked={allowedModels.includes(model)} onCheckedChange={checked => onToggleModel(model, checked === true)} />
                    <span className="truncate font-mono" title={model}>{model}</span>
                  </label>
                ))}
              </div>
            )}
            <p className="text-xs text-muted-foreground">{t('groups.allowedModelsHint')}</p>
          </div>
        </>
      )}
    </div>
  )
}

// 协议转换模式（W5 网关 internal/protoconv 消费）：自动协商或手动方向。
const PROTOCOL_CONVERTS: GroupProtocolConvert[] = ['auto', 'chat_to_resp', 'mess_to_resp', 'resp_to_mess', 'chat_to_mess']

// 协议转换多选切换：勾选 = 加入方向集合（同方向去重），取消 = 移除。
const toggleConvert = (on: boolean, v: GroupProtocolConvert, cur: GroupProtocolConvert[]) =>
  on ? (cur.includes(v) ? cur : [...cur, v]) : cur.filter(x => x !== v)

// 协议转换选择（创建/编辑共用）：默认自动协商；选择手动方向时仅保留
// 与客户端协议匹配的方向，避免同一请求出现多个候选。
function ProtocolConvertCheckboxes({ value, onChange }: {
  value: GroupProtocolConvert[]
  onChange: (v: GroupProtocolConvert[]) => void
}) {
  const { t } = useTranslation()
  return (
    <div className="space-y-1">
      {PROTOCOL_CONVERTS.map(v => (
        <label key={v} className="flex cursor-pointer items-center gap-2.5 rounded-md border px-2 py-1.5 text-sm">
          <Checkbox checked={value.includes(v)} onCheckedChange={c => {
            if (v === 'auto') {
              onChange(c === true ? ['auto'] : [])
              return
            }
            const next = toggleConvert(c === true, v, value.filter(x => x !== 'auto'))
            onChange(next)
          }} />
          {t(`groups.protocolConvert.${v}`)}
        </label>
      ))}
    </div>
  )
}

// 授予弹窗行内专属倍率态：mult = 输入框文本（'' = 未填）；cleared = 用户显式点过
// 「清除为未设置」（提交 null）；勾选留空且未清除 = 省略键（沿用当前值）。
interface AssignRowMult { mult: string; cleared: boolean }

// 价格倍率（正常值，API 边界已换算）→ 展示：null = 未设置（—）；0 = 免费；
// 其余 ×N（1 = ×1.0，1.5 = ×1.5）。
const formatMultiplier = (m: number | null | undefined, t: TFunction): string => {
  if (m == null) return '—'
  if (m === 0) return t('groups.free')
  return formatMultiplierValue(m)
}

// 倍率输入归一（创建/编辑共用）：空 = undefined（省略键）；非数字/越界抛错；
// 正常值返回数字（0 = 免费组，1 = ×1，上限 10）。
const normalizeMultiplierInput = (v: string, invalidMsg: string): number | undefined => {
  const m = v.trim()
  if (m === '') return undefined
  const n = Number(m)
  if (!isStorableMultiplier(n, 10)) throw new Error(invalidMsg)
  return n
}

// 可见性徽章：public 绿点 / private 灰点（与 StatusBadge 同风格）。
function VisibilityBadge({ visibility }: { visibility?: GroupVisibility }) {
  const { t } = useTranslation()
  const isPublic = visibility === 'public'
  return (
    <Badge variant="secondary" className={cn('gap-1.5', isPublic ? 'text-emerald-700 dark:text-emerald-400' : 'text-muted-foreground')}>
      <span className={cn('size-1.5 shrink-0 rounded-full', isPublic ? 'bg-emerald-500' : 'bg-muted-foreground/60')} />
      {t(isPublic ? 'groups.visibilityPublic' : 'groups.visibilityPrivate')}
    </Badge>
  )
}

function PublicStatusBadge({ status }: { status?: GroupPublicStatus }) {
  const { t } = useTranslation()
  const value = status ?? 'available'
  const tone = value === 'maintenance' ? 'text-amber-700 dark:text-amber-400' : value === 'paused' ? 'text-muted-foreground' : 'text-emerald-700 dark:text-emerald-400'
  return <Badge variant="secondary" className={cn('gap-1.5', tone)}><span className={cn('size-1.5 shrink-0 rounded-full', value === 'maintenance' ? 'bg-amber-500' : value === 'paused' ? 'bg-muted-foreground/60' : 'bg-emerald-500')} />{t(`groups.publicStatuses.${value}`)}</Badge>
}

// 授予弹窗用户行：勾选 + 用户标识 + 专属倍率三态输入（public 默认列表与搜索列表共用）。
function AssignUserRow({ uid, label, checked, row, onToggle, onMult, onClear, t }: {
  uid: number
  label: string
  checked: boolean
  row: AssignRowMult | undefined
  onToggle: (uid: number, on: boolean) => void
  onMult: (uid: number, v: string) => void
  onClear: (uid: number) => void
  t: TFunction
}) {
  return (
    <div className="flex items-center gap-2.5 rounded-md border px-2 py-1.5">
      <Checkbox checked={checked} onCheckedChange={c => onToggle(uid, c === true)} />
      <span className="min-w-0 flex-1 truncate text-sm" title={label}>{label}</span>
      {checked && (
        <>
          <Input
            type="number"
            min={0}
            max={10}
            step={0.0001}
            value={row?.mult ?? ''}
            placeholder={t('groups.assignMultiplierPlaceholder')}
            onChange={e => onMult(uid, e.target.value)}
            className="h-7 w-24 text-xs"
          />
          <Button
            variant="ghost"
            size="icon-sm"
            title={t('groups.assignMultiplierClear')}
            disabled={!row?.mult && !row?.cleared}
            onClick={() => onClear(uid)}
          >
            <X />
          </Button>
          {row?.cleared && (
            <span className="w-24 text-xs text-muted-foreground">{t('groups.assignMultiplierUnset')}</span>
          )}
        </>
      )}
    </div>
  )
}

export default function Groups() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 列表：筛选/分页状态归 queryKey ——
  const [name, setName] = useState('')
  const [activeSort, setActiveSort] = useState<string | null>(null) // null = 无主动排序（默认 id desc）
  const [order, setOrder] = useState<SortOrder>('desc')
  const [offset, setOffset] = useState(0)
  const [limit, setLimit] = useState(20)

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['groups', { limit, offset, name, sort: activeSort ?? 'id', order }],
    queryFn: () => api.listGroups({ limit, offset, name: name || undefined, sort: activeSort ?? 'id', order }),
  })
  const rows = data?.rows ?? []

  // —— 行勾选（跨页保留，筛选/翻页后清空）——
  const [selected, setSelected] = useState<number[]>([])
  const pageIds = rows.map(r => r.ID!)
  const allChecked = rows.length > 0 && pageIds.every(id => selected.includes(id))
  const someChecked = pageIds.some(id => selected.includes(id))
  const toggleRow = (id: number) => setSelected(s => (s.includes(id) ? s.filter(x => x !== id) : [...s, id]))
  const toggleAll = (c: boolean) =>
    setSelected(s => (c ? Array.from(new Set([...s, ...pageIds])) : s.filter(x => !pageIds.includes(x))))

  const resetPage = () => {
    setOffset(0)
    setSelected([])
  }
  // 每页条数变化 → 重置 offset 并清勾选。
  const changeLimit = (l: number) => { setLimit(l); resetPage() }
  const changeName = (v: string) => { setName(v); resetPage() }
  // 列头三态：新列 → 降序；同列降序 → 升序；同列升序 → 取消（回默认 id desc）
  const onColumnToggle = (col: string) => {
    resetPage()
    if (activeSort !== col) {
      setActiveSort(col)
      setOrder('desc')
    } else if (order === 'desc') {
      setOrder('asc')
    } else {
      setActiveSort(null)
      setOrder('desc')
    }
  }
  const hasFilters = name !== ''
  const clearFilters = () => {
    setName('')
    resetPage()
  }

  // —— 批量删除/重命名 ——
  const batchDelete = useMutation({
    mutationFn: (ids: number[]) => api.deleteGroupsBatch(ids),
    onSuccess: (_res, ids) => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setSelected([])
      // 当前页被删空时回到最后有效页（templates 同款守卫，不再一律回第 1 页）
      const after = (data?.total ?? 0) - ids.length
      if (offset > 0 && offset >= after) setOffset(Math.max(0, after - (after % limit)))
    },
  })
  const batchRename = useMutation({
    mutationFn: (p: { ids: number[]; name: string }) => api.updateGroupsBatch(p.ids, { name: p.name }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setSelected([])
      closeBatchRename('submitted')
    },
  })
  // BatchBar 的 onUpdate 返回 promise：对话框关闭（提交成功/取消）时 resolve。
  const [batchRenameOpen, setBatchRenameOpen] = useState(false)
  const [batchRenameValue, setBatchRenameValue] = useState('')
  const [batchRenameErr, setBatchRenameErr] = useState<string | null>(null)
  const batchResolve = useRef<((r: 'cancelled' | 'submitted') => void) | null>(null)
  const closeBatchRename = (r: 'cancelled' | 'submitted' = 'cancelled') => {
    setBatchRenameOpen(false)
    batchResolve.current?.(r)
    batchResolve.current = null
  }
  const openBatchRename = () => {
    batchRename.reset()
    setBatchRenameValue('')
    setBatchRenameErr(null)
    setBatchRenameOpen(true)
  }
  const submitBatchRename = () => {
    if (!batchRenameValue.trim()) {
      setBatchRenameErr(t('groups.batchUpdateEmpty'))
      return
    }
    batchRename.mutate({ ids: selected, name: batchRenameValue.trim() })
  }

  // —— 授予用户（替换语义：勾选 = 授予，未勾选 = 撤销；打开时并行预填充当前授予）——
  const [assignTarget, setAssignTarget] = useState<Group | null>(null)
  const [assignChecked, setAssignChecked] = useState<number[]>([])
  const [assignMult, setAssignMult] = useState<Record<number, AssignRowMult>>({})
  const [assignQuery, setAssignQuery] = useState('')
  const [assignOffset, setAssignOffset] = useState(0)
  const [assignLimit, setAssignLimit] = useState(20)
  // 预填充态：prefilled = 已成功回显当前授予；prefillFailed = 读端点不可用（空预填充 + toast，不阻塞弹窗）
  const [assignPrefilled, setAssignPrefilled] = useState(false)
  const [assignPrefillFailed, setAssignPrefillFailed] = useState(false)
  // 批量倍率输入值（勾选 2+ 用户时显示批量行）
  const [assignBatchMult, setAssignBatchMult] = useState('')
  // public 组预填充的「已配置专属倍率」用户（multipliers 非 null 键）：默认列表数据源
  const [assignPrefillUids, setAssignPrefillUids] = useState<number[]>([])
  // 预填充请求代际：弹窗关闭/换组后丢弃过期响应，防止旧组数据串入新组
  const assignFetchId = useRef(0)

  const openAssign = (g: Group) => {
    const isPublic = g.Visibility === 'public'
    setAssignTarget(g)
    setAssignChecked([])
    setAssignMult({})
    setAssignPrefillUids([])
    setAssignQuery('')
    setAssignOffset(0)
    setAssignPrefilled(false)
    setAssignPrefillFailed(false)
    setAssignBatchMult('')
    assign.reset() // 清掉上次提交失败的就地错误，换组重开不残留
    // 预填充：并行读当前授予 → 勾选态 = 已授予 user_ids，倍率输入框初值 = multipliers 正常值。
    // public 分支：公开组天然所有用户可用，无「授予」概念 → 初始勾选 = multipliers 非 null
    // 键（已配置专属倍率的用户），不用 user_ids（可能含清除残留行，不代表配置态）。
    // 读端点不可用（后端未实现 404/网络）→ 优雅降级：空预填充 + toast，弹窗正常打开。
    const gid = g.ID!
    const fetchId = ++assignFetchId.current
    api.getGroupAssignments(gid)
      .then(resp => {
        if (assignFetchId.current !== fetchId) return
        const muls: Record<number, AssignRowMult> = {}
        const prefilledUids: number[] = []
        for (const [uid, m] of Object.entries(resp.multipliers ?? {})) {
          const id = Number(uid)
          if (isPublic) {
            // public：只取非 null 键（null = 已清除 → 不勾选不显示）
            if (typeof m === 'number') {
              prefilledUids.push(id)
              muls[id] = { mult: String(m), cleared: false }
            }
          } else if (typeof m === 'number' && resp.user_ids.includes(id)) {
            // private：仅回显已授予用户的数值倍率；null = 未设置 → 留空（省略键沿用当前值，语义不变）
            muls[id] = { mult: String(m), cleared: false }
          }
        }
        prefilledUids.sort((a, b) => a - b)
        setAssignChecked(isPublic ? prefilledUids : resp.user_ids)
        setAssignMult(muls)
        // 默认列表数据源：public = 已配置专属倍率的用户；private = 已授予权限的用户全量
        setAssignPrefillUids(isPublic ? prefilledUids : resp.user_ids)
        setAssignPrefilled(true)
      })
      .catch(() => {
        if (assignFetchId.current !== fetchId) return
        setAssignPrefillFailed(true)
        toast.add({ title: t('groups.assignPrefillFailed'), type: 'error' })
      })
  }
  const toggleAssignUser = (id: number, on: boolean) =>
    setAssignChecked(s => (on ? (s.includes(id) ? s : [...s, id]) : s.filter(x => x !== id)))
  const assignIsPublic = assignTarget?.Visibility === 'public'
  // 空搜索时默认列表 = 预填充的已授予/已配置用户 ∪ 当前勾选（搜索新增的也保留）：
  // public = 已配置专属倍率的用户；private = 已授予权限的用户。不拉全量用户列表，
  // 只有搜索才显示用户列表（勾选 = 新增）。
  const assignDefaultIds = Array.from(new Set([...assignPrefillUids, ...assignChecked])).sort((a, b) => a - b)
  // 默认列表的邮箱尽力解析：预填充响应只有 uid（读端点不返回邮箱），
  // 用全量用户查询按 id 匹配（accounts 弹窗 listGroups(100) 同款先例）；未命中回退 #uid。
  const assignUsersLookup = useQuery({
    queryKey: ['users', 'assign-lookup'],
    queryFn: () => api.listUsers({ limit: 100 }),
    enabled: !!assignTarget && assignPrefillUids.length > 0,
  })
  const assignEmailMap = useMemo(() => {
    const m = new Map<number, string>()
    for (const u of assignUsersLookup.data?.rows ?? []) m.set(u.ID!, u.Email ?? '')
    return m
  }, [assignUsersLookup.data])
  const assignUsers = useQuery({
    queryKey: ['users', 'assign', { limit: assignLimit, offset: assignOffset, email: assignQuery }],
    queryFn: () => api.listUsers({ limit: assignLimit, offset: assignOffset, email: assignQuery || undefined }),
    // 空搜索时不显示用户列表（默认列表用预填充），跳过无谓的全量拉取（输入搜索词后自动启用）
    enabled: !!assignTarget && assignQuery !== '',
  })
  const assignRows = assignUsers.data?.rows ?? []
  const assignTotal = assignUsers.data?.total ?? 0

  // multipliers 三态：勾选且填值 → 数字；勾选留空（未清除）→ 省略键（沿用当前值）；
  // 勾选且显式清除 → null（回退组倍率）。
  const assign = useMutation({
    mutationFn: () => {
      const body: GroupAssignmentsBody = { user_ids: assignChecked }
      const muls: Record<string, number | null> = {}
      for (const uid of assignChecked) {
        const row = assignMult[uid]
        const v = row?.mult.trim()
        if (v !== undefined && v !== '') {
          const n = Number(v)
          if (!isStorableMultiplier(n, 10)) throw new Error(t('groups.multiplierInvalid'))
          muls[String(uid)] = n
        } else if (row?.cleared) {
          muls[String(uid)] = null
        }
      }
      if (Object.keys(muls).length > 0) body.multipliers = muls
      return api.setGroupAssignments(assignTarget!.ID!, body)
    },
    onSuccess: (resp) => {
      setAssignTarget(null)
      // 空勾选提交 = 清空（契约语义）：toast 用清空文案，避免「已授予 0 个用户」歧义。
      // public 组文案用「配置专属倍率」语义，private 组用「授予」语义。
      toast.add({
        title: t(assignIsPublic ? 'groups.assignPublicSuccess' : 'groups.assignSuccess'),
        description: resp.user_ids.length > 0
          ? t(assignIsPublic ? 'groups.assignConfiguredCount' : 'groups.assignSuccessDesc', { count: resp.user_ids.length })
          : t(assignIsPublic ? 'groups.assignConfiguredClearedDesc' : 'groups.assignClearedDesc'),
        type: 'success',
      })
    },
  })
  const setRowMult = (uid: number, mult: string) => setAssignMult(m => ({ ...m, [uid]: { mult, cleared: false } }))
  const clearRowMult = (uid: number) => setAssignMult(m => ({ ...m, [uid]: { mult: '', cleared: true } }))
  // 批量倍率：勾选 2+ 用户时整行统一设置/清除（对所有勾选用户生效，覆盖其行内值）
  const applyBatchMult = () => {
    const v = assignBatchMult.trim()
    if (v === '' || assignChecked.length < 2) return
    const n = Number(v)
    if (!isStorableMultiplier(n, 10)) {
      toast.add({ title: t('groups.multiplierInvalid'), type: 'error' })
      return
    }
    setAssignMult(m => {
      const next = { ...m }
      for (const uid of assignChecked) next[uid] = { mult: String(n), cleared: false }
      return next
    })
  }
  const clearBatchMult = () => {
    setAssignMult(m => {
      const next = { ...m }
      for (const uid of assignChecked) next[uid] = { mult: '', cleared: true }
      return next
    })
  }

  // —— 创建（名称 + 上游 + 模型；其余策略使用安全默认值）——
  const [createOpen, setCreateOpen] = useState(false)
  const [createName, setCreateName] = useState('')
  const [createRemark, setCreateRemark] = useState('')
  // The defaults stay opinionated -- public, x1, automatic negotiation -- but
  // each one is editable here. Hiding them forced a create-then-reopen-and-edit
  // round trip for the common cases of a private group or a non-x1 multiplier,
  // and a group briefly existed as public x1 before the correction landed.
  const [createVisibility, setCreateVisibility] = useState<GroupVisibility>('public')
  const [createProtocols, setCreateProtocols] = useState<GroupProtocolConvert[]>(['auto'])
  const [createMultiplier, setCreateMultiplier] = useState('')
  // A new group is always backed by the upstream pool. Existing account groups
  // remain supported in the edit flow, but a fresh group can never be created
  // empty and then look available while routing nothing.
  const createRoutingMode: GroupRoutingMode = 'upstreams'
  const [createAllowedModels, setCreateAllowedModels] = useState<string[]>([])
  const [createMembers, setCreateMembers] = useState<UpstreamMemberDraft[]>([])
  const createAutoModelKey = useRef('')
  // Keep the first upstream catalogue as a useful default, but never replace
  // a model selection the operator has edited when the pool changes.
  const createModelsTouched = useRef(false)
  const create = useMutation({
    mutationFn: async (n: string) => {
      const body: components['schemas']['GroupCreate'] = {
        name: n,
        remark: createRemark.trim() || undefined,
        visibility: createVisibility,
        routing_mode: createRoutingMode,
        allowed_models: createAllowedModels,
        protocol_convert: createProtocols,
      }
      if (createRoutingMode === 'upstreams') {
        body.upstream_members = serializeUpstreamMembers(createMembers)
      }
      const m = normalizeMultiplierInput(createMultiplier, t('groups.multiplierInvalid'))
      if (m !== undefined) body.price_multiplier = m // 正常值直接提交；输入为空则省略键（后端按 ×1）
      return api.createGroup(body)
    },
    onSuccess: (_g, name) => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      qc.invalidateQueries({ queryKey: ['group-upstreams'] })
      setCreateOpen(false)
      toast.add({ title: t('groups.createdSuccess'), description: name, type: 'success' })
    },
  })
  const openCreate = () => {
    // Reset a previous failed submission so an old error does not look like a
    // new request failure when the dialog is opened again.
    create.reset()
    // A usable unique name is generated up front so the operator never has to
    // invent metadata, but the field itself is editable in the dialog.
    setCreateName(makeAutoGroupName(t('groups.autoNamePrefix')))
    setCreateRemark('')
    setCreateVisibility('public')
    setCreateProtocols(['auto'])
    setCreateMultiplier('')
    setCreateAllowedModels([])
    setCreateMembers([])
    createAutoModelKey.current = ''
    createModelsTouched.current = false
    setCreateOpen(true)
  }
  // —— 编辑（name + visibility + protocol_convert 多选；PUT 缺省字段保持原值，
  //      此处总是显式提交——空数组 = 清空既有方向）——
  const [editTarget, setEditTarget] = useState<Group | null>(null)
  const [editName, setEditName] = useState('')
  const [editRemark, setEditRemark] = useState('')
  const [editVisibility, setEditVisibility] = useState<GroupVisibility>('public')
  const [editPublicStatus, setEditPublicStatus] = useState<GroupPublicStatus>('available')
  const [editProtocols, setEditProtocols] = useState<GroupProtocolConvert[]>([])
  // 倍率用字符串态：空 = 不修改（PUT 省略键，后端保持原值）
  const [editMultiplier, setEditMultiplier] = useState('')
  const [editRoutingMode, setEditRoutingMode] = useState<GroupRoutingMode>('accounts')
  const [editAllowedModels, setEditAllowedModels] = useState<string[]>([])
  const [editMembers, setEditMembers] = useState<UpstreamMemberDraft[]>([])
  const editPoolLoadedKey = useRef<string | null>(null)
  const editAutoModelKey = useRef<string | null>(null)
  const { data: upstreamRowsData, isLoading: upstreamRowsLoading, isError: upstreamRowsError, refetch: refetchUpstreams } = useQuery({
    queryKey: ['groups', 'upstream-options'],
    queryFn: () => api.listUpstreams({ limit: 200, offset: 0 }),
    enabled: createOpen || editTarget != null,
    staleTime: 30_000,
  })
  const upstreamRows = upstreamRowsData?.items ?? []
  const editUpstreamConfig = useQuery({
    queryKey: ['group-upstreams', editTarget?.ID],
    queryFn: () => api.getGroupUpstreams(editTarget!.ID!),
    enabled: editTarget?.ID != null,
    // The edit dialog is a management write surface. Always revalidate on
    // open so a second admin window cannot overwrite a newer member set from a
    // still-fresh client cache.
    staleTime: 0,
    refetchOnMount: 'always',
  })
  useEffect(() => {
    const id = editTarget?.ID
    const config = editUpstreamConfig.data
    const key = id == null || config == null ? null : `${id}:${editUpstreamConfig.dataUpdatedAt}`
    if (id == null || config == null || key == null || editPoolLoadedKey.current === key) return
    // The detail response is fetched on every open and is authoritative. The
    // list row may come from another window and be stale.
    setEditRoutingMode(config.routing_mode ?? editTarget.RoutingMode ?? 'accounts')
    setEditAllowedModels(config.allowed_models ?? editTarget.AllowedModels ?? [])
    setEditMembers((config.members ?? []).map(draftFromUpstream))
    editPoolLoadedKey.current = key
  }, [editTarget, editUpstreamConfig.data, editUpstreamConfig.dataUpdatedAt])
  const activeMemberIDs = (createOpen ? createMembers : editTarget ? editMembers : []).map(member => member.upstream_id).sort((a, b) => a - b)
  const activeMemberKey = activeMemberIDs.join(',')
  const upstreamModelSnapshotKey = upstreamRows.map(upstream => `${upstream.ID}:${upstream.ModelsCheckedAt ?? ''}:${upstream.ModelsError ?? ''}:${(upstream.Models ?? []).join('|')}`).join(';')
  const groupModels = useQuery({
    queryKey: ['group-upstream-models', activeMemberIDs, upstreamModelSnapshotKey],
    queryFn: () => {
      const selected = activeMemberIDs.map(id => upstreamRows.find(upstream => upstream.ID === id)).filter((upstream): upstream is Upstream => upstream != null)
      const union = sortModelsLatestFirst(Array.from(new Set(selected.flatMap(upstream => upstream.Models ?? []))))
      // A saved snapshot remains usable when the latest check is incomplete.
      // The previous successful model set stays routable while the operator
      // retries; only a missing snapshot blocks group setup.
      const incomplete = selected.length !== activeMemberIDs.length || selected.some(upstream => upstream.ModelsCheckedAt == null || (upstream.ModelsError != null && upstream.ModelsError !== 'model_unavailable'))
      const usable = selected.length === activeMemberIDs.length && selected.every(hasUsableUpstreamModelSnapshot)
      const degraded = selected.some(upstream => upstream.ModelsError === 'model_unavailable')
      // Upstream routing selects any healthy member that advertises the
      // requested model. Keep the catalogue as a union so the UI exposes the
      // same routes the scheduler can actually serve; an intersection here
      // silently hid models supported by only one selected upstream.
      return { models: union, partial: incomplete, degraded, usable }
    },
    enabled: (createOpen || editTarget != null) && activeMemberIDs.length > 0,
    // This query derives its result from the already-fetched upstream rows;
    // changing the snapshot key is the explicit invalidation mechanism.
    staleTime: Infinity,
  })
  const activeModels = useMemo(() => groupModels.data?.models ?? [], [groupModels.data])
  const activeModelsPartial = groupModels.data?.partial ?? false
  const activeModelsDegraded = groupModels.data?.degraded ?? false
  const activeModelsUsable = groupModels.data?.usable ?? false
  const canSubmitCreate = createName.trim().length > 0 &&
    createMembers.length > 0 &&
    createAllowedModels.length > 0 &&
    activeModels.length > 0 &&
    !upstreamRowsLoading &&
    !upstreamRowsError &&
    !groupModels.isLoading &&
    !groupModels.isFetching &&
    !groupModels.isError &&
    activeModelsUsable &&
    !create.isPending
  useEffect(() => {
    // A member replacement invalidates any allowlist entry that a complete
    // catalogue no longer confirms. During a partial check, keep the prior
    // choices visible until the retry succeeds.
    if (!editTarget || editRoutingMode !== 'upstreams' || activeMemberIDs.length === 0 || activeModelsPartial || activeModels.length === 0) return
    // Include the capability snapshot in the guard. A model refresh can keep
    // the same member IDs while removing a previously selected model; in that
    // case the allowlist must be pruned before the next save.
    const key = `${editTarget.ID}:${activeMemberKey}:${upstreamModelSnapshotKey}`
    if (editAutoModelKey.current === key) return
    editAutoModelKey.current = key
    setEditAllowedModels(current => current.filter(model => activeModels.includes(model)))
  }, [activeMemberKey, activeMemberIDs.length, activeModels, activeModelsPartial, editRoutingMode, editTarget, upstreamModelSnapshotKey])
  useEffect(() => {
    // A new upstream pool starts with the models that were actually read. This
    // keeps an empty allowlist intentional (all common models) while avoiding
    // the dangerous interpretation of an unread/empty catalogue as success.
    if (!createOpen || createRoutingMode !== 'upstreams' || activeModelsPartial || activeModels.length === 0) return
    // A refreshed catalogue is a meaningful pool change even when the
    // selected upstream IDs stay identical.
    const key = `${activeMemberKey}:${upstreamModelSnapshotKey}`
    if (createAutoModelKey.current === key) return
    createAutoModelKey.current = key
    setCreateAllowedModels(current => {
      if (!createModelsTouched.current) return activeModels
      // A pool edit can remove models that are no longer advertised. Keep
      // explicit user choices that remain valid instead of silently selecting
      // newly discovered models.
      return current.filter(model => activeModels.includes(model))
    })
  }, [activeMemberKey, activeModels, activeModelsPartial, createOpen, createRoutingMode, upstreamModelSnapshotKey])
  const updateCreateMember = (id: number, patch: Partial<UpstreamMemberDraft>) => setCreateMembers(current => current.map(member => member.upstream_id === id ? { ...member, ...patch } : member))
  const toggleCreateMember = (id: number, checked: boolean) => setCreateMembers(current => checked ? (current.some(member => member.upstream_id === id) ? current : [...current, defaultUpstreamMember(id)]) : current.filter(member => member.upstream_id !== id))
  const updateEditMember = (id: number, patch: Partial<UpstreamMemberDraft>) => setEditMembers(current => current.map(member => member.upstream_id === id ? { ...member, ...patch } : member))
  const toggleEditMember = (id: number, checked: boolean) => setEditMembers(current => checked ? (current.some(member => member.upstream_id === id) ? current : [...current, defaultUpstreamMember(id)]) : current.filter(member => member.upstream_id !== id))
  const toggleModel = (setter: Dispatch<SetStateAction<string[]>>, model: string, checked: boolean) => setter(current => checked ? (current.includes(model) ? current : [...current, model]) : current.filter(value => value !== model))
  const selectAllModels = (setter: Dispatch<SetStateAction<string[]>>) => setter(activeModels)
  const toggleCreateModel = (model: string, checked: boolean) => {
    createModelsTouched.current = true
    toggleModel(setCreateAllowedModels, model, checked)
  }
  const selectAllCreateModels = () => {
    createModelsTouched.current = true
    selectAllModels(setCreateAllowedModels)
  }
  const openEdit = (group: Group) => {
    editPoolLoadedKey.current = null
    editAutoModelKey.current = null
    setEditTarget(group)
    setEditName(group.Name ?? '')
    setEditRemark(group.Remark ?? '')
    setEditVisibility(group.Visibility ?? 'public')
    setEditPublicStatus(group.PublicStatus ?? 'available')
    setEditRoutingMode(group.RoutingMode ?? 'accounts')
    setEditAllowedModels(group.AllowedModels ?? [])
    setEditMembers([])
    setEditProtocols(group.ProtocolConvert ?? [])
    setEditMultiplier(group.PriceMultiplier != null ? String(group.PriceMultiplier) : '')
    rename.reset()
  }
  // —— 删除 ——
  const [deleting, setDeleting] = useState<Group | null>(null)

  const rename = useMutation({
    mutationFn: () => {
      const body: components['schemas']['GroupCreate'] = { name: editName.trim(), remark: editRemark.trim(), visibility: editVisibility, public_status: editPublicStatus, routing_mode: editRoutingMode, allowed_models: editAllowedModels, protocol_convert: editProtocols }
      const m = normalizeMultiplierInput(editMultiplier, t('groups.multiplierInvalid'))
      if (m !== undefined) body.price_multiplier = m // 正常值直接提交；输入为空则省略键（后端保持原值）
      // The API commits policy and the complete member set in one database
      // transaction. An empty array also removes stale members when switching
      // back to account routing.
      body.upstream_members = editRoutingMode === 'upstreams' ? serializeUpstreamMembers(editMembers) : []
      return api.updateGroup(editTarget!.ID!, body)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      qc.invalidateQueries({ queryKey: ['group-upstreams'] })
      setEditTarget(null)
    },
  })
  const remove = useMutation({
    mutationFn: (id: number) => api.deleteGroup(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setDeleting(null)
      // 删除的是当前页最后一行时回退一页（templates 同款「最后有效页」守卫）
      if (rows.length === 1 && offset > 0) setOffset(offset - limit)
    },
  })

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('groups.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('groups.subtitle')}</p>
        </div>
        <Button onClick={openCreate}><Plus /> {t('groups.new')}</Button>
      </div>

      <ListToolbar
        name={name}
        onNameChange={changeName}
      />

      <BatchBar
        selected={selected}
        onClear={() => setSelected([])}
        onDelete={async () => {
          await batchDelete.mutateAsync(selected)
        }}
        onUpdate={() => new Promise<'cancelled' | 'submitted'>(resolve => {
          batchResolve.current = resolve
          openBatchRename()
        })}
      />

      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <FolderOpen className="size-10" />
            <p className="font-medium">{hasFilters ? t('groups.filterEmpty') : t('groups.emptyTitle')}</p>
            {!hasFilters && <p className="text-sm">{t('groups.emptyDesc')}</p>}
            {hasFilters ? (
              <Button className="mt-2" variant="outline" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
            ) : (
              <Button className="mt-2" onClick={openCreate}><Plus /> {t('groups.new')}</Button>
            )}
          </Card>
        </motion.div>
      ) : (
        <>
          <div className="overflow-hidden rounded-lg">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-10">
                    <Checkbox
                      checked={allChecked}
                      indeterminate={someChecked && !allChecked}
                      onCheckedChange={c => toggleAll(c === true)}
                    />
                  </TableHead>
                  <SortableHeader field="id" label="ID" active={activeSort === 'id'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="name" label={t('groups.table.name')} active={activeSort === 'name'} order={order} onToggle={onColumnToggle} />
                  <TableHead>{t('groups.table.remark')}</TableHead>
                  <TableHead>{t('groups.table.visibility')}</TableHead>
                  <TableHead>{t('groups.table.publicStatus')}</TableHead>
                  <TableHead>{t('groups.table.routing')}</TableHead>
                  <TableHead>{t('groups.table.priceMultiplier')}</TableHead>
                  <TableHead>{t('groups.table.protocolConvert')}</TableHead>
                  <SortableHeader field="created_at" label={t('groups.table.createdAt')} active={activeSort === 'created_at'} order={order} onToggle={onColumnToggle} />
                  <TableHead className="text-right">{t('groups.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody className="[&_td]:py-3">
                {rows.map(g => (
                  <TableRow key={g.ID} data-state={selected.includes(g.ID!) ? 'selected' : undefined}>
                    <TableCell>
                      <Checkbox checked={selected.includes(g.ID!)} onCheckedChange={() => toggleRow(g.ID!)} />
                    </TableCell>
                    <TableCell className="tabular-nums">{g.ID}</TableCell>
                    <TableCell className="max-w-36 truncate" title={g.Name}>{g.Name}</TableCell>
                    <TableCell className="max-w-48 truncate text-sm text-muted-foreground" title={g.Remark ?? undefined}>{g.Remark || '—'}</TableCell>
                    <TableCell><VisibilityBadge visibility={g.Visibility} /></TableCell>
                    <TableCell><PublicStatusBadge status={g.PublicStatus} /></TableCell>
                    <TableCell>
                      <Badge variant="outline">{t(`groups.routingModes.${g.RoutingMode ?? 'accounts'}`)}</Badge>
                      {g.RoutingMode === 'upstreams' && <div className="mt-1 text-xs text-muted-foreground">{g.AllowedModels?.length ? t('groups.modelCount', { count: g.AllowedModels.length }) : t('groups.autoModels')}</div>}
                    </TableCell>
                    <TableCell className="tabular-nums">{formatMultiplier(g.PriceMultiplier, t)}</TableCell>
                    <TableCell>
                      {g.ProtocolConvert && g.ProtocolConvert.length > 0 ? (
                        <div className="flex flex-wrap gap-1">
                          {g.ProtocolConvert.map(pc => (
                            <Badge key={pc} variant="secondary" className="font-mono text-xs">{t(`groups.protocolConvertShort.${pc}`)}</Badge>
                          ))}
                        </div>
                      ) : (
                        <span className="text-xs text-muted-foreground">{t('groups.protocolConvertShort.off')}</span>
                      )}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">{formatDateTime(g.CreatedAt)}</TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button variant="ghost" size="icon-sm" title={t('groups.assignButton')} onClick={() => openAssign(g)}><UserPlus /></Button>
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(g)}><Pencil /></Button>
                        <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => { remove.reset(); setDeleting(g) }}><Trash2 /></Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
          <Pagination total={data?.total ?? 0} limit={limit} offset={offset} onOffsetChange={setOffset} onLimitChange={changeLimit} />
        </>
      )}

      {/* —— 创建分组：只需选择上游和模型；其余策略由安全默认值处理 —— */}
      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="top-4 max-h-[calc(100dvh-2rem)] translate-y-0 overflow-y-auto sm:top-1/2 sm:max-w-2xl sm:-translate-y-1/2">
          <DialogHeader>
            <DialogTitle>{t('groups.newTitle')}</DialogTitle>
            <DialogDescription>{t('groups.newDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            {/* Prefilled with a unique generated name so the flow still works
                without inventing metadata, but editable: renaming afterwards
                meant the group existed under a throwaway name in the meantime. */}
            <div className="space-y-1.5">
              <Label htmlFor="grp-create-name">{t('groups.nameLabel')}</Label>
              <Input id="grp-create-name" value={createName} onChange={e => setCreateName(e.target.value)} />
              <p className="text-xs text-muted-foreground">{t('groups.autoNameHint')}</p>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-create-remark">{t('groups.remarkLabel')}</Label>
              <Input id="grp-create-remark" value={createRemark} maxLength={500} placeholder={t('groups.remarkPlaceholder')} onChange={e => setCreateRemark(e.target.value)} />
              <p className="text-xs text-muted-foreground">{t('groups.remarkHint')}</p>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-create-visibility">{t('groups.visibilityLabel')}</Label>
              <Select
                items={Object.fromEntries([['public', t('groups.visibilityPublic')], ['private', t('groups.visibilityPrivate')]])}
                value={createVisibility}
                onValueChange={v => setCreateVisibility(v as GroupVisibility)}
              >
                <SelectTrigger id="grp-create-visibility" className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="public" label={t('groups.visibilityPublic')}>{t('groups.visibilityPublic')}</SelectItem>
                  <SelectItem value="private" label={t('groups.visibilityPrivate')}>{t('groups.visibilityPrivate')}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            {/* Member options stay visible at creation: hiding them silently gave
                every member priority 0, which puts the whole pool in one tier and
                makes routing look like it ignores priority. Routing mode is still
                fixed to the upstream pool so a new group cannot be created empty
                and then look available while routing nothing. */}
            <UpstreamPoolFields
              mode={createRoutingMode}
              showMode={false}
              onModeChange={() => {}}
              upstreams={upstreamRows}
              upstreamsLoading={upstreamRowsLoading}
              upstreamsError={upstreamRowsError}
              onRetryUpstreams={() => { void refetchUpstreams() }}
              members={createMembers}
              onToggle={toggleCreateMember}
              onUpdate={updateCreateMember}
              models={activeModels}
              modelsLoading={groupModels.isLoading || groupModels.isFetching}
              modelsError={activeModelsPartial || activeModelsDegraded}
              onRetryModels={() => { void refetchUpstreams() }}
              configError={false}
              allowedModels={createAllowedModels}
              onToggleModel={toggleCreateModel}
              onSelectAllModels={selectAllCreateModels}
            />
            <div className="space-y-1.5">
              <Label>{t('groups.protocolConvertLabel')}</Label>
              <ProtocolConvertCheckboxes value={createProtocols} onChange={setCreateProtocols} />
              <p className="text-xs text-muted-foreground">{t('groups.protocolConvertHint')}</p>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-create-multiplier">{t('groups.multiplierLabel')}</Label>
              <Input
                id="grp-create-multiplier"
                type="number"
                min={0}
                max={10}
                step={0.0001}
                value={createMultiplier}
                onChange={e => setCreateMultiplier(e.target.value)}
              />
              <p className="text-xs text-muted-foreground">{t('groups.multiplierHint')}</p>
            </div>
            {create.isError && errMsg(create.error) && (
              <p className="text-sm text-destructive">{errMsg(create.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)} disabled={create.isPending}>{t('common.cancel')}</Button>
            <Button onClick={() => create.mutate(createName.trim())} disabled={!canSubmitCreate}>
              {create.isPending ? t('common.creating') : t('common.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 编辑（name + visibility） —— */}
      <Dialog open={!!editTarget} onOpenChange={o => { if (!o && !rename.isPending) setEditTarget(null) }}>
        <DialogContent className="top-4 max-h-[calc(100dvh-2rem)] translate-y-0 overflow-y-auto sm:top-1/2 sm:max-w-2xl sm:-translate-y-1/2">
          <DialogHeader>
            <DialogTitle>{t('groups.editTitle', { id: editTarget?.ID })}</DialogTitle>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="grp-edit-name">{t('groups.nameLabel')}</Label>
              <Input id="grp-edit-name" value={editName} onChange={e => setEditName(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-edit-remark">{t('groups.remarkLabel')}</Label>
              <Input id="grp-edit-remark" value={editRemark} maxLength={500} placeholder={t('groups.remarkPlaceholder')} onChange={e => setEditRemark(e.target.value)} />
              <p className="text-xs text-muted-foreground">{t('groups.remarkHint')}</p>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-edit-visibility">{t('groups.visibilityLabel')}</Label>
              <Select
                items={Object.fromEntries([['public', t('groups.visibilityPublic')], ['private', t('groups.visibilityPrivate')]])}
                value={editVisibility}
                onValueChange={v => setEditVisibility(v as GroupVisibility)}
              >
                <SelectTrigger id="grp-edit-visibility" className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="public" label={t('groups.visibilityPublic')}>{t('groups.visibilityPublic')}</SelectItem>
                  <SelectItem value="private" label={t('groups.visibilityPrivate')}>{t('groups.visibilityPrivate')}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-edit-public-status">{t('groups.publicStatusLabel')}</Label>
              <Select
                items={Object.fromEntries((['available', 'maintenance', 'paused'] as GroupPublicStatus[]).map(value => [value, t(`groups.publicStatuses.${value}`)]))}
                value={editPublicStatus}
                onValueChange={v => setEditPublicStatus(v as GroupPublicStatus)}
              >
                <SelectTrigger id="grp-edit-public-status" className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="available" label={t('groups.publicStatuses.available')}>{t('groups.publicStatuses.available')}</SelectItem>
                  <SelectItem value="maintenance" label={t('groups.publicStatuses.maintenance')}>{t('groups.publicStatuses.maintenance')}</SelectItem>
                  <SelectItem value="paused" label={t('groups.publicStatuses.paused')}>{t('groups.publicStatuses.paused')}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <UpstreamPoolFields
              mode={editRoutingMode}
              onModeChange={mode => { setEditRoutingMode(mode); if (mode === 'accounts') { setEditMembers([]); setEditAllowedModels([]) } }}
              upstreams={upstreamRows}
              upstreamsLoading={upstreamRowsLoading}
              upstreamsError={upstreamRowsError}
              onRetryUpstreams={() => { void refetchUpstreams() }}
              members={editMembers}
              onToggle={toggleEditMember}
              onUpdate={updateEditMember}
              models={activeModels}
              modelsLoading={groupModels.isLoading || groupModels.isFetching || editUpstreamConfig.isLoading}
              modelsError={activeModelsPartial || activeModelsDegraded}
              onRetryModels={() => { void refetchUpstreams() }}
              configError={editUpstreamConfig.isError}
              allowedModels={editAllowedModels}
              onToggleModel={(model, checked) => toggleModel(setEditAllowedModels, model, checked)}
              onSelectAllModels={() => selectAllModels(setEditAllowedModels)}
            />
            <div className="space-y-1.5">
              <Label>{t('groups.protocolConvertLabel')}</Label>
              <ProtocolConvertCheckboxes value={editProtocols} onChange={setEditProtocols} />
              <p className="text-xs text-muted-foreground">{t('groups.protocolConvertHint')}</p>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-edit-multiplier">{t('groups.multiplierLabel')}</Label>
              <Input
                id="grp-edit-multiplier"
                type="number"
                min={0}
                max={10}
                step={0.0001}
                value={editMultiplier}
                onChange={e => setEditMultiplier(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter' && editName.trim() && !rename.isPending) rename.mutate() }}
              />
              <p className="text-xs text-muted-foreground">{t('groups.multiplierHint')}</p>
            </div>
            {rename.isError && errMsg(rename.error) && (
              <p className="text-sm text-destructive">{errMsg(rename.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditTarget(null)} disabled={rename.isPending}>{t('common.cancel')}</Button>
            <Button onClick={() => rename.mutate()} disabled={rename.isPending || !editName.trim() || (editRoutingMode === 'upstreams' && (editMembers.length === 0 || editAllowedModels.length === 0 || !activeModelsUsable || activeModels.length === 0 || groupModels.isLoading || editUpstreamConfig.isLoading || editUpstreamConfig.isError))}>
              {rename.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认（单行） —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) { remove.reset(); setDeleting(null) } }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('groups.deleteTitle')}</DialogTitle>
            <DialogDescription>
              {t('groups.deleteDesc', { name: deleting?.Name })}
            </DialogDescription>
          </DialogHeader>
          {remove.isError && errMsg(remove.error) && (
            <p className="text-sm text-destructive">{errMsg(remove.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => { remove.reset(); setDeleting(null) }} disabled={remove.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.ID!)} disabled={remove.isPending}>
              {remove.isPending ? t('common.deleting') : t('common.confirmDelete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 批量更新对话框：仅 name —— */}
      <Dialog open={batchRenameOpen} onOpenChange={o => { if (!o) closeBatchRename() }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('groups.batchUpdateTitle')}</DialogTitle>
            <DialogDescription>{t('groups.batchUpdateDesc', { count: selected.length })}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="grp-batch-name">{t('groups.nameLabel')}</Label>
              <Input
                id="grp-batch-name"
                value={batchRenameValue}
                placeholder={t('groups.namePlaceholder')}
                onChange={e => setBatchRenameValue(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter' && batchRenameValue.trim() && !batchRename.isPending) submitBatchRename() }}
              />
            </div>
            {batchRenameErr && <p className="text-sm text-destructive">{batchRenameErr}</p>}
            {batchRename.isError && errMsg(batchRename.error) && (
              <p className="text-sm text-destructive">{errMsg(batchRename.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => closeBatchRename()} disabled={batchRename.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submitBatchRename} disabled={batchRename.isPending || !batchRenameValue.trim()}>
              {batchRename.isPending ? t('common.saving') : t('list.batchUpdate')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 授予用户：替换语义（勾选 = 授予，未勾选 = 撤销）+ 专属倍率三态；
             public 组 = 专属倍率管理（默认列表只显示已配置用户，新增只能走搜索） —— */}
      <Dialog open={!!assignTarget} onOpenChange={o => { if (!o && !assign.isPending) setAssignTarget(null) }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            {/* public：公开组无「授予」概念，弹窗 = 专属倍率管理；private：授予权限语义 */}
            <DialogTitle>{t(assignIsPublic ? 'groups.assignPublicTitle' : 'groups.assignTitle', { name: assignTarget?.Name })}</DialogTitle>
            <DialogDescription>{t(assignIsPublic ? 'groups.assignPublicDesc' : 'groups.assignDesc')}</DialogDescription>
            <p className="text-xs text-muted-foreground">{t('groups.assignMultiplierHint')}</p>
            {assignPrefilled && assignChecked.length > 0 && (
              <p className="text-xs text-muted-foreground">
                {t(assignIsPublic ? 'groups.assignConfiguredCount' : 'groups.assignCount', { count: assignChecked.length })}
              </p>
            )}
            {/* 私有组：勾选 = 授予访问权（user_ids 全量替换天然含授权语义） */}
            {assignTarget?.Visibility === 'private' && (
              <p className="text-xs text-muted-foreground">{t('groups.assignPrivateHint')}</p>
            )}
            {/* 读端点不可用时才显示「无法回显」提示：正常路径已由预填充替代 */}
            {assignPrefillFailed && (
              <p className="text-xs text-amber-600 dark:text-amber-400">{t('groups.assignEchoNote')}</p>
            )}
          </DialogHeader>
          <div className="space-y-3">
            <Input
              value={assignQuery}
              placeholder={t('groups.assignSearchPlaceholder')}
              onChange={e => { setAssignQuery(e.target.value); setAssignOffset(0) }}
            />
            {assignChecked.length >= 2 && (
              <div className="flex items-center gap-2">
                <Input
                  type="number"
                  min={0}
                  max={10}
                  step={0.0001}
                  value={assignBatchMult}
                  placeholder={t('groups.assignBatchPlaceholder')}
                  onChange={e => setAssignBatchMult(e.target.value)}
                  onKeyDown={e => { if (e.key === 'Enter') applyBatchMult() }}
                  className="h-7 w-40 text-xs"
                />
                <Button variant="outline" size="sm" onClick={applyBatchMult} disabled={!assignBatchMult.trim()}>
                  {t('groups.assignBatchApply')}
                </Button>
                <Button variant="ghost" size="sm" onClick={clearBatchMult}>
                  <X /> {t('groups.assignBatchClear')}
                </Button>
              </div>
            )}
            {assignQuery === '' ? (
              /* 空搜索默认列表：只显示预填充的已授予/已配置用户（∪ 搜索新增勾选）；
                 取消勾选 = 移除（行保留显示未勾选态，提交后消失）；新增只能走搜索 */
              <ScrollArea className="max-h-72 pr-1">
                <div className="space-y-1.5">
                {assignDefaultIds.length === 0 ? (
                  <p className="py-6 text-center text-sm text-muted-foreground">
                    {t(assignIsPublic ? 'groups.assignPublicSearchHint' : 'groups.assignPrivateSearchHint')}
                  </p>
                ) : (
                  assignDefaultIds.map(uid => (
                    <AssignUserRow
                      key={uid}
                      uid={uid}
                      label={assignEmailMap.get(uid) ?? `#${uid}`}
                      checked={assignChecked.includes(uid)}
                      row={assignMult[uid]}
                      onToggle={toggleAssignUser}
                      onMult={setRowMult}
                      onClear={clearRowMult}
                      t={t}
                    />
                  ))
                )}
                </div>
              </ScrollArea>
            ) : assignUsers.isLoading ? (
              <div className="space-y-1.5">
                {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-9" />)}
              </div>
            ) : assignUsers.isError ? (
              <p className="text-sm text-destructive">{t('common.loadFailed', { message: (assignUsers.error as Error).message })}</p>
            ) : assignRows.length === 0 ? (
              <p className="py-6 text-center text-sm text-muted-foreground">{t('groups.assignEmpty')}</p>
            ) : (
              <>
                <ScrollArea className="max-h-72 pr-1">
                  <div className="space-y-1.5">
                  {assignRows.map(u => (
                    <AssignUserRow
                      key={u.ID}
                      uid={u.ID!}
                      label={u.Email ?? ''}
                      checked={assignChecked.includes(u.ID!)}
                      row={assignMult[u.ID!]}
                      onToggle={toggleAssignUser}
                      onMult={setRowMult}
                      onClear={clearRowMult}
                      t={t}
                    />
                  ))}
                  </div>
                </ScrollArea>
                <Pagination total={assignTotal} limit={assignLimit} offset={assignOffset} onOffsetChange={setAssignOffset} onLimitChange={l => { setAssignLimit(l); setAssignOffset(0) }} />
              </>
            )}
            {assign.isError && errMsg(assign.error) && (
              <p className="text-sm text-destructive">{errMsg(assign.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAssignTarget(null)} disabled={assign.isPending}>{t('common.cancel')}</Button>
            <Button onClick={() => assign.mutate()} disabled={assign.isPending}>
              {assign.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
