// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Ban, Check, Copy, Filter, History, Plus, Ticket, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { BatchBar } from '@/components/batch-bar'
import { PagePagination } from '@/components/page-pagination'
import { SortableHeader, type SortOrder } from '@/components/sortable-header'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { DateTimePicker } from '@/components/ui/date-picker'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { cn } from '@/lib/utils'
import { copyText } from '@/components/key-box'
import { formatDateTime, toRFC3339 } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type RedemptionCode = components['schemas']['RedemptionCode']
// The checked-in web schema can lag the server's activity-code addition. Keep
// the local union compatible with both the current generated types and the
// scoped_temp_balance contract exposed by the API.
type RedemptionType = components['schemas']['RedemptionType'] | 'scoped_temp_balance'
type RedemptionStatus = components['schemas']['RedemptionStatus']
type GenerateRequest = components['schemas']['GenerateRequest']
type GenerateRequestPayload = Omit<GenerateRequest, 'type'> & { type: RedemptionType; group_id?: number }
type RedemptionCodeView = RedemptionCode & { GroupID?: number | null }
type RedemptionHistoryView = components['schemas']['RedemptionHistory'] & { GroupID?: number | null }

const TYPES: RedemptionType[] = ['balance', 'concurrency', 'temp_balance', 'scoped_temp_balance']
const STATUSES: RedemptionStatus[] = ['active', 'disabled']

// 面值展示（2026-08-15 对齐修复）：Value = API 边界已换算的 USD——直接显示，
// 勿再 /1e5（此前误用 formatCost（毫分输入）→ 5.0 显示成 $0.00005）；concurrency → 并发数直出。
function formatValue(c: { Type: RedemptionType; Value: number }): string {
  return c.Type === 'concurrency' ? String(c.Value) : `$${c.Value.toFixed(2)}`
}

// 状态徽章：active 绿点 / disabled 灰点（与 StatusBadge 同风格；状态类型为
// RedemptionStatus，单独声明避免跨 schema 类型强转）。
function CodeStatusBadge({ status }: { status: RedemptionStatus }) {
  const { t } = useTranslation()
  const active = status === 'active'
  return (
    <Badge variant="secondary" className={cn('gap-1.5', active ? 'text-emerald-700 dark:text-emerald-400' : 'text-muted-foreground')}>
      <span className={cn('size-1.5 shrink-0 rounded-full', active ? 'bg-emerald-500' : 'bg-muted-foreground/60')} />
      {t(`redemptions.status.${status}`)}
    </Badge>
  )
}

// 生成表单态。value 口径 = openapi GenerateRequest（2026-08-15 对齐修复）：
// balance/temp_balance 按 USD 输入（1 USD = 100,000 毫分，可小数）；concurrency
// 为并发数（正整数）。码过期时间仍使用 DateTimePicker；临时余额改为从兑换时
// 起算的自然日数，提交时由前端换算成后端已有的 resource_expires_at。
interface GenForm {
  type: RedemptionType
  value: string
  remark: string
  expires_at: string
  temp_days: string // temp_balance/scoped_temp_balance 必填；从生成时刻起的自然日数
  group_id: string // scoped_temp_balance 必填；活动额度仅可用于该分组
  max_uses: string
  count: string
}

const emptyGenForm = (): GenForm => ({
  type: 'balance',
  value: '',
  remark: '',
  expires_at: '',
  temp_days: '7',
  group_id: '',
  max_uses: '1',
  count: '1',
})

const MAX_TEMP_DAYS = 3650

export default function RedemptionCodes() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 列表：增强分页范式（page/page_size，1-based）+ type/status 筛选 ——
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [activeSort, setActiveSort] = useState<string | null>(null) // null = 默认 id desc
  const [order, setOrder] = useState<SortOrder>('desc')
  const [typeFilter, setTypeFilter] = useState<'all' | RedemptionType>('all')
  const [statusFilter, setStatusFilter] = useState<'all' | RedemptionStatus>('all')

  const { data, isLoading, isError, error } = useQuery({
    queryKey: [
      'redemption-codes',
      { page, page_size: pageSize, type: typeFilter, status: statusFilter, sort: activeSort ?? 'id', order },
    ],
    queryFn: () =>
      api.listRedemptionCodes({
        page,
        page_size: pageSize,
        type: typeFilter === 'all' ? undefined : typeFilter,
        status: statusFilter === 'all' ? undefined : statusFilter,
        sort: activeSort ?? 'id',
        order,
      }),
  })
  const rows = (data?.rows ?? []) as RedemptionCodeView[]

  // 末页死胡同守卫：非首页的当前页数据被清空（如批量失效把末页清空）时回退到第 1 页，
  // 避免空态页无返回入口。页 1 本身为空（列表真正为空）时无需回退，不会成环。
  useEffect(() => {
    if (!isLoading && !isError && rows.length === 0 && page > 1) setPage(1)
  }, [isLoading, isError, rows.length, page])

  // —— 行勾选（跨页保留，筛选/翻页重置后清空） ——
  const [selected, setSelected] = useState<number[]>([])
  const pageIds = rows.map(r => r.ID)
  const allChecked = rows.length > 0 && pageIds.every(id => selected.includes(id))
  const someChecked = pageIds.some(id => selected.includes(id))
  const toggleRow = (id: number) => setSelected(s => (s.includes(id) ? s.filter(x => x !== id) : [...s, id]))
  const toggleAll = (c: boolean) =>
    setSelected(s => (c ? Array.from(new Set([...s, ...pageIds])) : s.filter(x => !pageIds.includes(x))))

  const resetPage = () => {
    setPage(1)
    setSelected([])
  }
  // 每页条数变化 → 重置页码并清勾选。
  const changePageSize = (s: number) => { setPageSize(s); resetPage() }
  // 列头三态：新列 → 降序；同列降序 → 升序；同列升序 → 取消（回默认 id desc）。
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
  const changeType = (v: string) => { setTypeFilter(v as 'all' | RedemptionType); resetPage() }
  const changeStatus = (v: string) => { setStatusFilter(v as 'all' | RedemptionStatus); resetPage() }
  const hasFilters = typeFilter !== 'all' || statusFilter !== 'all'
  const clearFilters = () => { setTypeFilter('all'); setStatusFilter('all'); resetPage() }

  // —— 单码失效 / 批量失效（失效后行保留，UsedCount 仍可见——审计语义） ——
  const [deactivating, setDeactivating] = useState<RedemptionCode | null>(null)
  const deactivate = useMutation({
    mutationFn: (id: number) => api.deactivateRedemptionCode(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['redemption-codes'] })
      setDeactivating(null)
    },
  })
  const batchDeactivate = useMutation({
    mutationFn: (ids: number[]) => api.deactivateRedemptionCodesBatch(ids),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['redemption-codes'] })
      setSelected([])
    },
  })

  // —— 生成对话框：form → 生成成功切 result（码列表 + 每行复制按钮） ——
  const [genOpen, setGenOpen] = useState(false)
  const [genForm, setGenForm] = useState<GenForm>(emptyGenForm())
  const [genErr, setGenErr] = useState<string | null>(null)
  const [generated, setGenerated] = useState<RedemptionCodeView[] | null>(null)
  const [copiedId, setCopiedId] = useState<number | null>(null) // 列表行内单码复制反馈
  const [copiedAll, setCopiedAll] = useState(false) // 生成结果「复制全部」反馈
  const groupsQ = useQuery({
    queryKey: ['redemption-groups'],
    queryFn: () => api.listGroups({ limit: 200, offset: 0, sort: 'id', order: 'asc' }),
    // Fetch once for both the scoped-code selector and the list labels. This
    // keeps existing activity codes readable before the generate dialog opens.
    staleTime: 60_000,
  })
  const groups = (groupsQ.data?.rows ?? []).filter(g => g.ID != null && g.DeletedAt == null)
  const groupNameById = new Map(groups.map(group => [group.ID!, group.Name?.trim() || `#${group.ID}`]))
  const groupLabel = (id: number | null | undefined) => id == null ? '—' : (groupNameById.get(id) ?? `#${id}`)
  const generate = useMutation({
    mutationFn: (b: GenerateRequestPayload) => api.generateRedemptionCodes(b as GenerateRequest),
    onSuccess: res => {
      setGenerated(res.codes as RedemptionCodeView[])
      qc.invalidateQueries({ queryKey: ['redemption-codes'] })
    },
  })
  const openGenerate = () => {
    setGenForm(emptyGenForm())
    setGenErr(null)
    setGenerated(null)
    setCopiedId(null)
    setCopiedAll(false)
    setGenOpen(true)
  }
  const copyCode = async (c: RedemptionCode) => {
    if (await copyText(c.Code)) {
      setCopiedId(c.ID)
      setTimeout(() => setCopiedId(null), 2000)
    }
  }
  // 复制全部：整段多行文本（每码一行 + 换行）一次写入剪贴板。
  const copyAllCodes = async () => {
    if (!generated) return
    if (await copyText(generated.map(c => c.Code).join('\n'))) {
      setCopiedAll(true)
      setTimeout(() => setCopiedAll(false), 2000)
    }
  }
  const updateGenForm = (patch: Partial<GenForm>) => {
    setGenForm(f => ({ ...f, ...patch }))
    setGenErr(null)
  }
  const submitGenerate = () => {
    const value = Number(genForm.value)
    const count = genForm.count === '' ? 1 : Number(genForm.count)
    const maxUses = genForm.max_uses === '' ? 1 : Number(genForm.max_uses)
    const tempDays = genForm.temp_days === '' ? NaN : Number(genForm.temp_days)
    const groupID = genForm.group_id === '' ? undefined : Number(genForm.group_id)
    if (
      !(value > 0) ||
      (genForm.type === 'concurrency' && !Number.isInteger(value)) || // USD 面值可小数；并发数必须整数
      !Number.isInteger(count) || count < 1 || count > 1000 ||
      !Number.isInteger(maxUses) || maxUses < 1 ||
      ((genForm.type === 'temp_balance' || genForm.type === 'scoped_temp_balance') && (!Number.isInteger(tempDays) || tempDays < 1 || tempDays > MAX_TEMP_DAYS)) ||
      (genForm.type === 'scoped_temp_balance' && (!Number.isInteger(groupID) || groupID! <= 0 || !groups.some(g => g.ID === groupID)))
    ) {
      setGenErr(t('redemptions.formInvalid', { max: MAX_TEMP_DAYS }))
      return
    }
    const body: GenerateRequestPayload = {
      type: genForm.type,
      value,
      remark: genForm.remark.trim() || undefined,
      max_uses: maxUses,
      count,
    }
    const expires = toRFC3339(genForm.expires_at)
    if (expires) body.expires_at = expires
    if (genForm.type === 'temp_balance' || genForm.type === 'scoped_temp_balance') {
      // 期限以服务端收到请求的当前时间为准，避免把用户机器时区/夏令时
      // 的日期字符串直接交给 API；后端已有 resource_expires_at 校验。
      body.resource_expires_at = new Date(Date.now() + tempDays * 24 * 60 * 60 * 1000).toISOString()
    }
    if (genForm.type === 'scoped_temp_balance') body.group_id = groupID
    generate.mutate(body)
  }
  const changeGenerateType = (type: RedemptionType) => updateGenForm({ type, group_id: type === 'scoped_temp_balance' ? genForm.group_id : '' })
  const typeItems = Object.fromEntries(TYPES.map(tp => [tp, t(`redemptions.type.${tp}`)]))
  const filterTypeItems = Object.fromEntries([['all', t('redemptions.all')], ...TYPES.map(tp => [tp, t(`redemptions.type.${tp}`)])])
  const filterStatusItems = Object.fromEntries([['all', t('redemptions.all')], ...STATUSES.map(s => [s, t(`redemptions.status.${s}`)])])

  // —— 全部兑换历史（管理审计）：服务端分页，避免把全部记录一次性拉到浏览器 ——
  const [historyOpen, setHistoryOpen] = useState(false)
  const [historyPage, setHistoryPage] = useState(1)
  const [historyPageSize, setHistoryPageSize] = useState(20)
  const [historyCodeID, setHistoryCodeID] = useState('')
  const [historyUserID, setHistoryUserID] = useState('')
  const [historyType, setHistoryType] = useState<'all' | RedemptionType>('all')
  const historyCodeIdValue = historyCodeID === '' ? undefined : Number(historyCodeID)
  const historyUserIdValue = historyUserID === '' ? undefined : Number(historyUserID)
  const historyCodeIdValid = historyCodeID === '' || (Number.isInteger(historyCodeIdValue) && historyCodeIdValue! > 0)
  const historyUserIdValid = historyUserID === '' || (Number.isInteger(historyUserIdValue) && historyUserIdValue! > 0)
  const historyQ = useQuery({
    queryKey: ['redemption-history', { page: historyPage, page_size: historyPageSize, code_id: historyCodeID, user_id: historyUserID, type: historyType }],
    queryFn: () => api.getRedemptionHistory({
      page: historyPage,
      page_size: historyPageSize,
      code_id: historyCodeIdValid ? historyCodeIdValue : undefined,
      user_id: historyUserIdValid ? historyUserIdValue : undefined,
      type: historyType === 'all' ? undefined : historyType,
      sort: 'id',
      order: 'desc',
    }),
    enabled: historyOpen && historyCodeIdValid && historyUserIdValid,
  })
  const historyRows = (historyQ.data?.rows ?? []) as RedemptionHistoryView[]
  const historyTypeItems = Object.fromEntries([['all', t('redemptions.all')], ...TYPES.map(tp => [tp, t(`redemptions.type.${tp}`)])])
  const resetHistory = () => {
    setHistoryPage(1)
    setHistoryCodeID('')
    setHistoryUserID('')
    setHistoryType('all')
  }

  // —— 使用明细（审计）：某码全部兑换记录（端点不分页，整表展示） ——
  const [usesFor, setUsesFor] = useState<RedemptionCode | null>(null)
  const usesQ = useQuery({
    queryKey: ['redemption-uses', usesFor?.ID],
    queryFn: () => api.getRedemptionCodeUses(usesFor!.ID),
    enabled: !!usesFor,
  })
  const usesRows = usesQ.data?.rows ?? []

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('redemptions.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('redemptions.subtitle')}</p>
        </div>
        <div className="flex flex-wrap justify-end gap-2">
          <Button variant="outline" onClick={() => setHistoryOpen(true)}><History /> {t('redemptions.historyOpen')}</Button>
          <Button onClick={openGenerate}><Plus /> {t('redemptions.new')}</Button>
        </div>
      </div>

      {/* 筛选工具栏（与 ListToolbar 同风格；本列表无名称搜索，仅 type/status 筛选） */}
      <div className="flex flex-wrap items-center gap-2 rounded-lg border p-3">
        <Select items={filterTypeItems} value={typeFilter} onValueChange={changeType}>
          <SelectTrigger size="default" className="w-40" aria-label={t('redemptions.filterType')}>
            <SelectValue placeholder={t('redemptions.filterType')} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all" label={t('redemptions.all')}>{t('redemptions.all')}</SelectItem>
            {TYPES.map(tp => <SelectItem key={tp} value={tp} label={t(`redemptions.type.${tp}`)}>{t(`redemptions.type.${tp}`)}</SelectItem>)}
          </SelectContent>
        </Select>
        <Select items={filterStatusItems} value={statusFilter} onValueChange={changeStatus}>
          <SelectTrigger size="default" className="w-40" aria-label={t('redemptions.filterStatus')}>
            <SelectValue placeholder={t('redemptions.filterStatus')} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all" label={t('redemptions.all')}>{t('redemptions.all')}</SelectItem>
            {STATUSES.map(s => <SelectItem key={s} value={s} label={t(`redemptions.status.${s}`)}>{t(`redemptions.status.${s}`)}</SelectItem>)}
          </SelectContent>
        </Select>
        {hasFilters && (
          <Button variant="ghost" size="lg" onClick={clearFilters}>
            <X /> {t('list.reset')}
          </Button>
        )}
      </div>

      <BatchBar
        selected={selected}
        onClear={() => setSelected([])}
        onDelete={async () => { await batchDeactivate.mutateAsync(selected) }}
        deleteLabel={t('redemptions.batchDeactivate')}
        confirmTitle={t('redemptions.batchDeactivateConfirmTitle')}
        confirmDesc={t('redemptions.batchDeactivateConfirm', { count: selected.length })}
        successTitle={t('redemptions.batchDeactivated', { count: selected.length })}
      />

      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <Ticket className="size-10" />
            <p className="font-medium">{hasFilters ? t('redemptions.filterEmpty') : t('redemptions.emptyTitle')}</p>
            {!hasFilters && <p className="text-sm">{t('redemptions.emptyDesc')}</p>}
            {hasFilters ? (
              <Button className="mt-2" variant="outline" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
            ) : (
              <Button className="mt-2" onClick={openGenerate}><Plus /> {t('redemptions.new')}</Button>
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
                  <SortableHeader field="code" label={t('redemptions.table.code')} active={activeSort === 'code'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="type" label={t('redemptions.table.type')} active={activeSort === 'type'} order={order} onToggle={onColumnToggle} />
                  <TableHead>{t('redemptions.table.group')}</TableHead>
                  <SortableHeader field="value" label={t('redemptions.table.value')} active={activeSort === 'value'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="used_count" label={t('redemptions.table.maxUses')} active={activeSort === 'used_count'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="status" label={t('redemptions.table.status')} active={activeSort === 'status'} order={order} onToggle={onColumnToggle} />
                  <TableHead>{t('redemptions.table.expiresAt')}</TableHead>
                  <TableHead>{t('redemptions.table.remark')}</TableHead>
                  <TableHead className="text-right">{t('redemptions.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody className="[&_td]:py-3">
                {rows.map(c => (
                  <TableRow key={c.ID} data-state={selected.includes(c.ID) ? 'selected' : undefined}>
                    <TableCell>
                      <Checkbox checked={selected.includes(c.ID)} onCheckedChange={() => toggleRow(c.ID)} />
                    </TableCell>
                    <TableCell className="tabular-nums">{c.ID}</TableCell>
                    <TableCell>
                      <div className="flex items-center gap-1">
                        <code className="font-mono text-sm">{c.Code}</code>
                        <Button variant="ghost" size="icon-sm" title={t('keybox.copy')} onClick={() => copyCode(c)}>
                          {copiedId === c.ID ? <Check /> : <Copy />}
                        </Button>
                      </div>
                    </TableCell>
                    <TableCell>{t(`redemptions.type.${c.Type}`)}</TableCell>
                    <TableCell className="max-w-36 truncate" title={c.GroupID == null ? undefined : groupLabel(c.GroupID)}>{groupLabel(c.GroupID)}</TableCell>
                    <TableCell className="tabular-nums">{formatValue(c)}</TableCell>
                    <TableCell className="tabular-nums">{c.UsedCount} / {c.MaxUses}</TableCell>
                    <TableCell><CodeStatusBadge status={c.Status} /></TableCell>
                    <TableCell>
                      <div className="text-sm">{formatDateTime(c.ExpiresAt)}</div>
                      {c.ResourceExpiresAt && (
                        <div className="text-xs text-muted-foreground">
                          {t('redemptions.table.resourceExpiresAt')}: {formatDateTime(c.ResourceExpiresAt)}
                        </div>
                      )}
                    </TableCell>
                    <TableCell className="max-w-40 truncate" title={c.Remark ?? undefined}>{c.Remark || '—'}</TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button variant="ghost" size="icon-sm" title={t('redemptions.uses')} onClick={() => setUsesFor(c)}>
                          <History />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          className="text-destructive"
                          title={t('redemptions.deactivate')}
                          onClick={() => setDeactivating(c)}
                          disabled={c.Status === 'disabled' || deactivate.isPending}
                        >
                          <Ban />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
          <PagePagination total={data?.total ?? 0} pageSize={pageSize} page={page} onPageChange={setPage} onPageSizeChange={changePageSize} />
        </>
      )}

      {/* —— 生成对话框（form → result 两阶段） —— */}
      <Dialog open={genOpen} onOpenChange={setGenOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>
              {generated ? t('redemptions.generatedTitle', { count: generated.length }) : t('redemptions.generateTitle')}
            </DialogTitle>
            <DialogDescription>{generated ? t('redemptions.generatedDesc') : t('redemptions.generateDesc')}</DialogDescription>
          </DialogHeader>
          {generated ? (
            <>
              {generated.length > 1 && (
                <p className="text-sm text-muted-foreground">
                  {t('redemptions.generatedCount', { count: generated.length })}
                </p>
              )}
              {/* 单框展示所有码（每行一个）：点击全选便于手动复制；超出 max-h 滚动 */}
              <ScrollArea className="max-h-48 rounded-lg border bg-muted/40 p-3">
                <code className="select-all whitespace-pre font-mono text-sm leading-6">
                  {generated.map(c => c.Code).join('\n')}
                </code>
              </ScrollArea>
              <DialogFooter className="sm:justify-between">
                <Button variant="outline" onClick={copyAllCodes}>
                  {copiedAll ? <Check /> : <Copy />}
                  {copiedAll ? t('redemptions.copiedAll') : t('redemptions.copyAll')}
                </Button>
                <Button onClick={() => setGenOpen(false)}>{t('common.done')}</Button>
              </DialogFooter>
            </>
          ) : (
            <>
              <div className="space-y-3">
                <div className="grid grid-cols-2 gap-3">
                  <div className="space-y-1.5">
                    <Label>{t('redemptions.typeLabel')}</Label>
                    <Select
                      items={typeItems}
                      value={genForm.type}
                      onValueChange={v => changeGenerateType(v as RedemptionType)}
                    >
                      <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                      <SelectContent>
                        {TYPES.map(tp => <SelectItem key={tp} value={tp} label={t(`redemptions.type.${tp}`)}>{t(`redemptions.type.${tp}`)}</SelectItem>)}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="rc-value">{t('redemptions.valueLabel')}</Label>
                    <Input id="rc-value" type="number" min={1} value={genForm.value} onChange={e => updateGenForm({ value: e.target.value })} />
                  </div>
                </div>
                <p className="-mt-2 text-xs text-muted-foreground">
                  {t(`redemptions.typeHint${genForm.type === 'balance' ? 'Balance' : genForm.type === 'temp_balance' ? 'TempBalance' : genForm.type === 'scoped_temp_balance' ? 'ScopedTempBalance' : 'Concurrency'}`)}
                </p>
                <p className="-mt-2 text-xs text-muted-foreground">
                  {genForm.type === 'concurrency' ? t('redemptions.valueHintConcurrency') : t('redemptions.valueHintBalance')}
                </p>
                <div className="space-y-1.5">
                  <Label htmlFor="rc-remark">{t('redemptions.remarkLabel')}</Label>
                  <Input id="rc-remark" value={genForm.remark} placeholder={t('redemptions.remarkPlaceholder')} onChange={e => updateGenForm({ remark: e.target.value })} />
                </div>
                <div className="grid grid-cols-2 gap-3">
                  <div className="space-y-1.5">
                    <Label htmlFor="rc-expires">{t('redemptions.expiresAtLabel')}</Label>
                    <DateTimePicker id="rc-expires" value={genForm.expires_at} onChange={v => updateGenForm({ expires_at: v })} />
                    <p className="text-xs text-muted-foreground">{t('redemptions.expiresAtHint')}</p>
                  </div>
                  <div className="space-y-1.5">
                    {genForm.type === 'temp_balance' || genForm.type === 'scoped_temp_balance' ? (
                      <>
                        <Label htmlFor="rc-temp-days">
                          {t('redemptions.tempDaysLabel')}<span className="ml-1 text-destructive">*</span>
                        </Label>
                        <Input id="rc-temp-days" type="number" min={1} max={MAX_TEMP_DAYS} step={1} value={genForm.temp_days} onChange={e => updateGenForm({ temp_days: e.target.value })} />
                        <p className="text-xs text-muted-foreground">{t('redemptions.tempDaysHint', { max: MAX_TEMP_DAYS })}</p>
                      </>
                    ) : (
                      <div className="rounded-md border border-dashed px-3 py-2 text-xs text-muted-foreground">
                        {genForm.type === 'balance' ? t('redemptions.permanentBalanceHint') : t('redemptions.noResourceExpiryHint')}
                      </div>
                    )}
                  </div>
                </div>
                {genForm.type === 'scoped_temp_balance' && (
                  <div className="space-y-1.5">
                    <Label htmlFor="rc-group">
                      {t('redemptions.groupLabel')}<span className="ml-1 text-destructive">*</span>
                    </Label>
                    <Select
                      items={Object.fromEntries(groups.map(g => [String(g.ID), g.Name ?? `#${g.ID}`]))}
                      value={genForm.group_id}
                      onValueChange={v => updateGenForm({ group_id: v ?? '' })}
                    >
                      <SelectTrigger id="rc-group" className="w-full">
                        <SelectValue placeholder={groupsQ.isLoading ? t('redemptions.groupLoading') : t('redemptions.groupPlaceholder')} />
                      </SelectTrigger>
                      <SelectContent>
                        {groups.map(g => <SelectItem key={g.ID} value={String(g.ID)} label={g.Name ?? `#${g.ID}`}>{g.Name ?? `#${g.ID}`}</SelectItem>)}
                      </SelectContent>
                    </Select>
                    {groupsQ.isError && <p className="text-xs text-destructive">{t('redemptions.groupLoadFailed')}</p>}
                    {!groupsQ.isLoading && !groupsQ.isError && groups.length === 0 && <p className="text-xs text-muted-foreground">{t('redemptions.groupEmpty')}</p>}
                  </div>
                )}
                <div className="grid grid-cols-2 gap-3">
                  <div className="space-y-1.5">
                    <Label htmlFor="rc-max">{t('redemptions.maxUsesLabel')}</Label>
                    <Input id="rc-max" type="number" min={1} value={genForm.max_uses} onChange={e => updateGenForm({ max_uses: e.target.value })} />
                    <p className="text-xs text-muted-foreground">{t('redemptions.maxUsesHint')}</p>
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="rc-count">{t('redemptions.countLabel')}</Label>
                    <Input id="rc-count" type="number" min={1} max={1000} value={genForm.count} onChange={e => updateGenForm({ count: e.target.value })} />
                    <p className="text-xs text-muted-foreground">{t('redemptions.countHint')}</p>
                  </div>
                </div>
                {genErr && <p className="text-sm text-destructive">{genErr}</p>}
                {generate.isError && errMsg(generate.error) && (
                  <p className="text-sm text-destructive">{errMsg(generate.error)}</p>
                )}
              </div>
              <DialogFooter>
                <Button variant="outline" onClick={() => setGenOpen(false)} disabled={generate.isPending}>{t('common.cancel')}</Button>
                <Button
                  onClick={submitGenerate}
                  disabled={generate.isPending || (genForm.type === 'scoped_temp_balance' && (groupsQ.isLoading || groupsQ.isError || groups.length === 0 || !genForm.group_id))}
                >
                  {generate.isPending ? t('common.creating') : t('redemptions.new')}
                </Button>
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>

      {/* —— 全部兑换历史（管理审计） —— */}
      <Dialog open={historyOpen} onOpenChange={setHistoryOpen}>
        <DialogContent className="sm:max-w-4xl">
          <DialogHeader>
            <DialogTitle>{t('redemptions.historyTitle')}</DialogTitle>
            <DialogDescription>{t('redemptions.historyDesc')}</DialogDescription>
          </DialogHeader>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
            <div className="space-y-1.5">
              <Label htmlFor="history-code-id">{t('redemptions.historyFilterCode')}</Label>
              <Input id="history-code-id" type="number" min={1} step={1} inputMode="numeric" value={historyCodeID} placeholder={t('redemptions.historyFilterHint')} onChange={e => { setHistoryCodeID(e.target.value); setHistoryPage(1) }} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="history-user-id">{t('redemptions.historyFilterUser')}</Label>
              <Input id="history-user-id" type="number" min={1} step={1} inputMode="numeric" value={historyUserID} placeholder={t('redemptions.historyFilterHint')} onChange={e => { setHistoryUserID(e.target.value); setHistoryPage(1) }} />
            </div>
            <div className="space-y-1.5">
              <Label>{t('redemptions.historyFilterType')}</Label>
              <Select items={historyTypeItems} value={historyType} onValueChange={v => { setHistoryType(v as 'all' | RedemptionType); setHistoryPage(1) }}>
                <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="all" label={t('redemptions.all')}>{t('redemptions.all')}</SelectItem>
                  {TYPES.map(tp => <SelectItem key={tp} value={tp} label={t(`redemptions.type.${tp}`)}>{t(`redemptions.type.${tp}`)}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
          </div>
          {(!historyCodeIdValid || !historyUserIdValid) && <p className="text-sm text-destructive">{t('redemptions.historyFilterInvalid')}</p>}
          {historyQ.isError ? (
            <p className="text-sm text-destructive">{t('redemptions.historyLoadFailed', { message: (historyQ.error as Error).message })}</p>
          ) : historyQ.isLoading ? (
            <div className="space-y-2">{Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-10" />)}</div>
          ) : historyRows.length === 0 ? (
            <p className="py-8 text-center text-sm text-muted-foreground">{t('redemptions.historyEmpty')}</p>
          ) : (
            <div className="overflow-hidden rounded-lg border">
              <ScrollArea showHorizontal className="max-h-[min(55vh,28rem)]">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>ID</TableHead>
                      <TableHead>{t('redemptions.historyTable.code')}</TableHead>
                      <TableHead>{t('redemptions.historyTable.userId')}</TableHead>
                      <TableHead>{t('redemptions.historyTable.type')}</TableHead>
                      <TableHead>{t('redemptions.historyTable.group')}</TableHead>
                      <TableHead>{t('redemptions.historyTable.value')}</TableHead>
                      <TableHead>{t('redemptions.historyTable.resourceExpiresAt')}</TableHead>
                      <TableHead>{t('redemptions.historyTable.createdAt')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody className="[&_td]:py-3">
                    {historyRows.map(row => (
                      <TableRow key={row.ID}>
                        <TableCell className="tabular-nums">{row.ID}</TableCell>
                        <TableCell>
                          <code className="font-mono text-xs">{row.Code}</code>
                          {row.Remark && <div className="max-w-36 truncate text-xs text-muted-foreground" title={row.Remark}>{row.Remark}</div>}
                        </TableCell>
                        <TableCell className="tabular-nums">{row.UserID}</TableCell>
                        <TableCell>{t(`redemptions.type.${row.CodeType}`)}</TableCell>
                        <TableCell>{groupLabel(row.GroupID)}</TableCell>
                        <TableCell className="tabular-nums">{formatValue({ Type: row.CodeType, Value: row.Value })}</TableCell>
                        <TableCell className="whitespace-nowrap text-sm">{formatDateTime(row.ResourceExpiresAt)}</TableCell>
                        <TableCell className="whitespace-nowrap text-sm">{formatDateTime(row.CreatedAt)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </ScrollArea>
            </div>
          )}
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="text-sm text-muted-foreground">{t('redemptions.historyTotal', { count: historyQ.data?.total ?? 0 })}</span>
            <div className="flex items-center gap-2">
              <Button variant="ghost" size="sm" onClick={resetHistory}>{t('redemptions.historyReset')}</Button>
              <PagePagination total={historyQ.data?.total ?? 0} pageSize={historyPageSize} page={historyPage} onPageChange={setHistoryPage} onPageSizeChange={s => { setHistoryPageSize(s); setHistoryPage(1) }} />
            </div>
          </div>
          <DialogFooter><Button variant="outline" onClick={() => setHistoryOpen(false)}>{t('common.done')}</Button></DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 单码失效确认 —— */}
      <Dialog open={!!deactivating} onOpenChange={o => { if (!o && !deactivate.isPending) setDeactivating(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('redemptions.deactivateTitle')}</DialogTitle>
            <DialogDescription>{t('redemptions.deactivateDesc', { code: deactivating?.Code })}</DialogDescription>
          </DialogHeader>
          {deactivate.isError && errMsg(deactivate.error) && (
            <p className="text-sm text-destructive">{errMsg(deactivate.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeactivating(null)} disabled={deactivate.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deactivating && deactivate.mutate(deactivating.ID)} disabled={deactivate.isPending}>
              {deactivate.isPending ? t('common.deleting') : t('redemptions.deactivateConfirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 使用明细（审计） —— */}
      <Dialog open={!!usesFor} onOpenChange={o => { if (!o) setUsesFor(null) }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{t('redemptions.usesTitle', { code: usesFor?.Code })}</DialogTitle>
            <DialogDescription>{t('redemptions.usesDesc')}</DialogDescription>
          </DialogHeader>
          {usesQ.isError ? (
            <p className="text-sm text-destructive">{errMsg(usesQ.error)}</p>
          ) : usesQ.isLoading ? (
            <div className="space-y-2">
              {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-10" />)}
            </div>
          ) : usesRows.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">{t('redemptions.usesEmpty')}</p>
          ) : (
            <ScrollArea className="max-h-80 rounded-lg border">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>ID</TableHead>
                    <TableHead>{t('redemptions.table.userId')}</TableHead>
                    <TableHead>{t('redemptions.table.value')}</TableHead>
                    <TableHead>{t('redemptions.table.createdAt')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody className="[&_td]:py-3">
                  {usesRows.map(u => (
                    <TableRow key={u.ID}>
                      <TableCell className="tabular-nums">{u.ID}</TableCell>
                      <TableCell className="tabular-nums">{u.UserID}</TableCell>
                      <TableCell className="tabular-nums">{usesFor && formatValue({ Type: usesFor.Type, Value: u.Value })}</TableCell>
                      <TableCell>{formatDateTime(u.CreatedAt)}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </ScrollArea>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setUsesFor(null)}>{t('common.done')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
