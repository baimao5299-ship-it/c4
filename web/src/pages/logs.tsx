import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ArrowDown, ArrowUp, ChevronLeft, ChevronRight, FileText, RotateCcw, SlidersHorizontal } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { DateRangePicker } from '@/components/date-range-picker'
import { DropdownMenu, DropdownMenuCheckboxItem, DropdownMenuContent, DropdownMenuGroup, DropdownMenuLabel, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { formatCost, formatDateTime, toRFC3339 } from '@/components/fmt'
import { cn } from '@/lib/utils'
import type { components } from '@/lib/api/schema'

type ErrorType = components['schemas']['ErrorType']
type RequestFormat = components['schemas']['RequestFormat']

const ERROR_TYPES: ErrorType[] = ['none', '429', '4xx', '5xx', 'network', 'auth', 'no_account', 'abort', 'billing']

// brief Step 1 色板：none 绿 / 4xx 黄 / 5xx、network、abort 红 / 429 橙 / auth、no_account 灰 / billing 紫。
const ERROR_META: Record<ErrorType, string> = {
  none: 'bg-emerald-500/10 text-emerald-600 dark:bg-emerald-400/10 dark:text-emerald-400',
  '4xx': 'bg-yellow-500/10 text-yellow-600 dark:bg-yellow-400/10 dark:text-yellow-400',
  '5xx': 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  network: 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  abort: 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  '429': 'bg-orange-500/10 text-orange-600 dark:bg-orange-400/10 dark:text-orange-400',
  auth: 'bg-muted text-muted-foreground',
  no_account: 'bg-muted text-muted-foreground',
  billing: 'bg-violet-500/10 text-violet-600 dark:bg-violet-400/10 dark:text-violet-400',
}

function ErrorTypeBadge({ type }: { type?: ErrorType }) {
  const { t } = useTranslation()
  if (!type) return <span className="text-xs text-muted-foreground">—</span>
  return <Badge className={ERROR_META[type]}>{t(`errorType.${type}`)}</Badge>
}

// 厂商/协议专有名词不翻译（与 templates.tsx 保持一致）。
const FORMAT_LABELS: Record<RequestFormat, string> = {
  'openai-chat': 'OpenAI Chat',
  'openai-responses': 'OpenAI Responses',
  anthropic: 'Anthropic',
}

// 表头样式（sub2api 配方）：uppercase 小字 + sticky（评审 Minor-1：必须位于
// 纵向滚动容器内——纯 overflow-x 容器中 sticky top 不生效）。
function Th({ className, ...props }: React.ComponentProps<typeof TableHead>) {
  return (
    <TableHead
      className={cn(
        'sticky top-0 z-10 bg-background text-xs uppercase tracking-wider text-muted-foreground',
        className
      )}
      {...props}
    />
  )
}

// 延迟健康色（sub2api 配方）：<1s 绿 / <5s 黄 / <15s 橙 / 以上红——色点与数字同色。
function latencyColor(ms: number): { dot: string; text: string } {
  if (ms < 1000) return { dot: 'bg-emerald-500', text: 'text-emerald-500' }
  if (ms < 5000) return { dot: 'bg-amber-500', text: 'text-amber-500' }
  if (ms < 15000) return { dot: 'bg-orange-500', text: 'text-orange-500' }
  return { dot: 'bg-red-500', text: 'text-red-500' }
}

// compact 千位缩写（官方仓库无内置函数，自己写——Intl 原生 API）：1234 → 1.2K。
function compactTokens(n: number, locale: string): string {
  return new Intl.NumberFormat(locale, { notation: 'compact', maximumFractionDigits: 1 }).format(n)
}

const LIMITS = [10, 20, 50, 100, 1000]
// base-ui Select 不接受空串值，用哨兵表示「全部」。
const ERROR_ALL = '__all__'

// 可隐藏列（时间/请求 ID 始终可见，参考 sub2api 使用明细的列设置模式）；
// BillingTier/AboveHit/Overdraft 已并入 Tokens 悬停窗（不再独立列）。
// 隐藏选择持久化到 localStorage（logs-hidden-columns）。
const HIDDEN_STORAGE_KEY = 'logs-hidden-columns'
const HIDDENABLE_COLS = ['user', 'key', 'group', 'account', 'model', 'format', 'statusCode', 'errorType', 'cost', 'latency', 'tokens'] as const

function loadHiddenCols(): Set<string> {
  try {
    const raw = localStorage.getItem(HIDDEN_STORAGE_KEY)
    if (raw) return new Set(JSON.parse(raw) as string[])
  } catch { /* 损坏数据忽略 */ }
  return new Set()
}

interface LogFilters {
  group_id: string
  account_id: string
  model: string
  status_code: string
  error_type: string
  from: string
  to: string
}

const emptyFilters = (): LogFilters => ({
  group_id: '', account_id: '', model: '', status_code: '', error_type: '', from: '', to: '',
})

export default function Logs() {
  const { t, i18n } = useTranslation()
  const [filters, setFilters] = useState<LogFilters>(emptyFilters())
  const [limit, setLimit] = useState(20)
  const [offset, setOffset] = useState(0)

  // 过滤条件 / 每页条数变化 → 回到第一页（同一事件内同步重置，避免双请求）。
  const set = (patch: Partial<LogFilters>) => {
    setFilters(f => ({ ...f, ...patch }))
    setOffset(0)
  }
  const changeLimit = (v: string) => {
    setLimit(Number(v))
    setOffset(0)
  }

  // 参数对象随 filter/limit/offset 派生 → queryKey 变化即触发新查询。
  const params = useMemo(
    () => ({
      group_id: filters.group_id ? Number(filters.group_id) : undefined,
      account_id: filters.account_id ? Number(filters.account_id) : undefined,
      model: filters.model || undefined,
      status_code: filters.status_code ? Number(filters.status_code) : undefined,
      error_type: filters.error_type || undefined,
      from: toRFC3339(filters.from),
      to: toRFC3339(filters.to),
      limit,
      offset,
    }),
    [filters, limit, offset]
  )

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['logs', params],
    queryFn: () => api.getLogs(params),
  })

  // —— 名称映射：日志行只存 ID，组/账号列显示名称（未命中回退 #id）——
  // 全量拉取（上限 1000，超出部分仅影响展示回退数字）；5 分钟缓存避免每页刷新重查。
  const { data: groupNameById } = useQuery({
    queryKey: ['groups', { limit: 1000 }],
    queryFn: () => api.listGroups({ limit: 1000 }),
    select: data => new Map(data.rows.map(g => [g.ID, g.Name])),
    staleTime: 5 * 60 * 1000,
  })
  const { data: accountNameById } = useQuery({
    queryKey: ['accounts', { limit: 1000 }],
    queryFn: () => api.listAccounts({ limit: 1000 }),
    select: data => new Map(data.rows.map(a => [a.ID, a.Name])),
    staleTime: 5 * 60 * 1000,
  })

  const total = data?.total ?? 0
  const rows = data?.rows ?? []
  const pages = Math.max(1, Math.ceil(total / limit))
  const page = total === 0 ? 1 : Math.floor(offset / limit) + 1

  // —— 列可见性（localStorage 持久化）——
  const [hiddenCols, setHiddenCols] = useState<Set<string>>(loadHiddenCols)
  const toggleCol = (key: string) => {
    setHiddenCols(prev => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      try { localStorage.setItem(HIDDEN_STORAGE_KEY, JSON.stringify([...next])) } catch { /* 忽略 */ }
      return next
    })
  }
  const isColVisible = (key: string) => !hiddenCols.has(key)

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('logs.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('logs.subtitle')}</p>
      </div>

      {/* 过滤栏：分组/账号/模型/状态码/错误类型 + 时间范围 */}
      <Card className="p-4">
        <div className="grid grid-cols-2 gap-3 md:grid-cols-4 xl:grid-cols-8">
          <div className="space-y-1.5">
            <Label htmlFor="log-group">{t('logs.filter.groupId')}</Label>
            <Input id="log-group" type="number" min={0} placeholder="1" value={filters.group_id} onChange={e => set({ group_id: e.target.value })} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="log-account">{t('logs.filter.accountId')}</Label>
            <Input id="log-account" type="number" min={0} placeholder="1" value={filters.account_id} onChange={e => set({ account_id: e.target.value })} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="log-model">{t('logs.filter.model')}</Label>
            <Input id="log-model" placeholder="gpt-4o" value={filters.model} onChange={e => set({ model: e.target.value })} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="log-status">{t('logs.filter.statusCode')}</Label>
            <Input id="log-status" type="number" min={0} placeholder="200" value={filters.status_code} onChange={e => set({ status_code: e.target.value })} />
          </div>
          <div className="space-y-1.5">
            <Label>{t('logs.filter.errorType')}</Label>
            <Select
              items={Object.fromEntries([[ERROR_ALL, t('logs.filter.all')], ...ERROR_TYPES.map(et => [et, t(`errorType.${et}`)])])}
              value={filters.error_type || ERROR_ALL}
              onValueChange={v => set({ error_type: v === ERROR_ALL ? '' : v })}
            >
              <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value={ERROR_ALL} label={t('logs.filter.all')}>{t('logs.filter.all')}</SelectItem>
                {ERROR_TYPES.map(et => <SelectItem key={et} value={et} label={t(`errorType.${et}`)}>{t(`errorType.${et}`)}</SelectItem>)}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-1.5">
            <Label>{t('dateRange.label')}</Label>
            <DateRangePicker value={{ from: filters.from, to: filters.to }} onChange={v => set(v)} />
          </div>
          <div className="flex items-end">
            <Button variant="outline" className="w-full" onClick={() => { setFilters(emptyFilters()); setOffset(0) }}>
              <RotateCcw /> {t('logs.filter.reset')}
            </Button>
          </div>
        </div>
      </Card>

      {/* 列设置 + 表格 */}
      <div className="flex items-center justify-between gap-2">
        <h2 className="text-sm font-medium text-muted-foreground">{t('logs.table.title', { total })}</h2>
        <DropdownMenu>
          <DropdownMenuTrigger render={<Button variant="outline" size="sm"><SlidersHorizontal className="size-4" />{t('logs.columnSettings')}</Button>} />
          <DropdownMenuContent align="end" className="max-h-80 w-48 overflow-y-auto">
            <DropdownMenuGroup>
              <DropdownMenuLabel>{t('logs.columnSettings')}</DropdownMenuLabel>
              {HIDDENABLE_COLS.map(key => (
                <DropdownMenuCheckboxItem
                  key={key}
                  checked={isColVisible(key)}
                  onCheckedChange={() => toggleCol(key)}
                >
                  {t(`logs.table.${key}`)}
                </DropdownMenuCheckboxItem>
              ))}
            </DropdownMenuGroup>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <Card>
          <div className="space-y-2 p-4">
            {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-10" />)}
          </div>
        </Card>
      ) : rows.length === 0 ? (
        <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
          <FileText className="size-10" />
          <p className="font-medium">{t('logs.emptyTitle')}</p>
          <p className="text-sm">{t('logs.emptyDesc')}</p>
        </Card>
      ) : (
        <>
        <Card className="overflow-hidden">
          <Table containerClassName="max-h-[calc(100vh-16rem)] overflow-y-auto">
            <TableHeader>
              <TableRow>
                <Th>{t('logs.table.requestId')}</Th>
                <Th>{t('logs.table.createdAt')}</Th>
                {isColVisible('user') && <Th className="text-right">{t('logs.table.user')}</Th>}
                {isColVisible('key') && <Th className="text-right">{t('logs.table.key')}</Th>}
                {isColVisible('group') && <Th className="text-right">{t('logs.table.group')}</Th>}
                {isColVisible('account') && <Th className="text-right">{t('logs.table.account')}</Th>}
                {isColVisible('model') && <Th>{t('logs.table.model')}</Th>}
                {isColVisible('format') && <Th>{t('logs.table.format')}</Th>}
                {isColVisible('statusCode') && <Th className="text-right">{t('logs.table.statusCode')}</Th>}
                {isColVisible('errorType') && <Th>{t('logs.table.errorType')}</Th>}
                {isColVisible('cost') && <Th className="text-right">{t('logs.table.cost')}</Th>}
                {isColVisible('latency') && <Th className="text-right">{t('logs.table.latency')}</Th>}
                {isColVisible('tokens') && <Th className="text-right">{t('logs.table.tokens')}</Th>}
              </TableRow>
            </TableHeader>
            <TableBody className="[&_td]:py-3">
              {rows.map(l => (
                <TableRow key={l.ID}>
                  <TableCell className="max-w-36">
                    <span className="block truncate font-mono text-xs text-muted-foreground" title={l.RequestID}>{l.RequestID ?? '—'}</span>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(l.CreatedAt)}</TableCell>
                  {/* 鉴权归属：用户/Key（0 = 无鉴权） */}
                  {isColVisible('user') && <TableCell className="text-right tabular-nums">{l.UserID ? `#${l.UserID}` : '—'}</TableCell>}
                  {isColVisible('key') && <TableCell className="text-right tabular-nums">{l.KeyID ? `#${l.KeyID}` : '—'}</TableCell>}
                  {isColVisible('group') && (
                    <TableCell className="text-right">
                      {l.GroupID ? <span className="tabular-nums">{groupNameById?.get(l.GroupID) ?? `#${l.GroupID}`}</span> : '—'}
                    </TableCell>
                  )}
                  {isColVisible('account') && (
                    <TableCell className="text-right">
                      {l.AccountID ? <span className="tabular-nums">{accountNameById?.get(l.AccountID) ?? `#${l.AccountID}`}</span> : '—'}
                    </TableCell>
                  )}
                  {/* 模型链式（sub2api 纵向链）：请求模型加粗 + 映射模型缩进灰（有值才显示 ↳）；
                      break-all 换行（长模型名完整展示，不截断） */}
                  {isColVisible('model') && (
                  <TableCell>
                    <div className="space-y-0.5 text-xs">
                      <div className="max-w-40 break-all font-medium">{l.Model ?? '—'}</div>
                      {l.MappedModel && (
                        <div className="max-w-40 break-all pl-3 text-muted-foreground">↳{l.MappedModel}</div>
                      )}
                    </div>
                  </TableCell>
                  )}
                  {isColVisible('format') && (
                  <TableCell>
                    {l.Format ? <Badge variant="outline">{FORMAT_LABELS[l.Format]}</Badge> : <span className="text-xs text-muted-foreground">—</span>}
                  </TableCell>
                  )}
                  {isColVisible('statusCode') && <TableCell className="text-right tabular-nums">{l.StatusCode ?? '—'}</TableCell>}
                  {isColVisible('errorType') && <TableCell><ErrorTypeBadge type={l.ErrorType} /></TableCell>}
                  {/* 计费：Cost 毫分 → USD（0/空显示 —）；档位/超档/透支已并入 Tokens 悬停窗 */}
                  {isColVisible('cost') && <TableCell className="text-right tabular-nums">{formatCost(l.Cost)}</TableCell>}
                  {/* 延迟：健康色点 + 着色数字（<1s 绿 / <5s 黄 / <15s 橙 / 红） */}
                  {isColVisible('latency') && (
                  <TableCell className="text-right tabular-nums">
                    {l.LatencyMS != null ? (
                      <span className="inline-flex items-center justify-end gap-1.5">
                        <span className={cn('size-2 rounded-full', latencyColor(l.LatencyMS).dot)} />
                        <span className={latencyColor(l.LatencyMS).text}>{l.LatencyMS} ms</span>
                      </span>
                    ) : (
                      <span className="text-xs text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  )}
                  {/* token 列：↓绿 ↑紫 千分位 + cache 第二行 K/M 缩写 + ⓘ 悬停大卡
                      （tokens 明细 + 档位 BillingTier + 超档/透支徽章） */}
                  {isColVisible('tokens') && (
                  <TableCell className="text-right font-medium tabular-nums">
                    {l.InputTokens || l.OutputTokens || l.CacheReadTokens || l.CacheCreationTokens ? (
                      <span className="inline-flex items-center justify-end gap-1.5">
                        <span className="space-y-0.5 text-xs text-right">
                          <span className="inline-flex items-center gap-2">
                            <span className="inline-flex items-center gap-0.5 text-emerald-500">
                              <ArrowDown className="size-3" />{(l.InputTokens ?? 0).toLocaleString()}
                            </span>
                            <span className="inline-flex items-center gap-0.5 text-purple-500">
                              <ArrowUp className="size-3" />{(l.OutputTokens ?? 0).toLocaleString()}
                            </span>
                          </span>
                          {l.CacheReadTokens || l.CacheCreationTokens ? (
                            <div className="text-right">
                              <span className="text-blue-500">{t('logs.tokens.read')} {compactTokens(l.CacheReadTokens ?? 0, i18n.language)}</span>
                              <span className="mx-1 text-muted-foreground/50">·</span>
                              <span className="text-amber-500">{t('logs.tokens.write')} {compactTokens(l.CacheCreationTokens ?? 0, i18n.language)}</span>
                            </div>
                          ) : null}
                        </span>
                        <Tooltip>
                          <TooltipTrigger render={<span className="inline-flex size-4 shrink-0 cursor-help items-center justify-center rounded-full bg-muted text-muted-foreground text-[10px] leading-none" />}>
                            i
                          </TooltipTrigger>
                          <TooltipContent className="max-w-xs border bg-popover p-0 shadow-lg">
                            <div className="space-y-1.5 p-3 text-xs">
                              <div className="flex items-center justify-between gap-6">
                                <span className="text-muted-foreground">{t('logs.tokens.input')}</span>
                                <span className="font-medium tabular-nums">{(l.InputTokens ?? 0).toLocaleString()}</span>
                              </div>
                              <div className="flex items-center justify-between gap-6">
                                <span className="text-muted-foreground">{t('logs.tokens.output')}</span>
                                <span className="font-medium tabular-nums">{(l.OutputTokens ?? 0).toLocaleString()}</span>
                              </div>
                              {l.CacheReadTokens ? (
                                <div className="flex items-center justify-between gap-6">
                                  <span className="text-muted-foreground">{t('logs.tokens.cacheRead')}</span>
                                  <span className="font-medium tabular-nums">{l.CacheReadTokens.toLocaleString()}</span>
                                </div>
                              ) : null}
                              {l.CacheCreationTokens ? (
                                <div className="flex items-center justify-between gap-6">
                                  <span className="text-muted-foreground">{t('logs.tokens.cacheWrite')}</span>
                                  <span className="font-medium tabular-nums">{l.CacheCreationTokens.toLocaleString()}</span>
                                </div>
                              ) : null}
                              <div className="flex items-center justify-between gap-6 border-t pt-1.5">
                                <span className="text-muted-foreground">{t('logs.tokens.total')}</span>
                                <span className="font-semibold tabular-nums">{(l.TotalTokens ?? 0).toLocaleString()}</span>
                              </div>
                              {/* 计费信息并入：档位 + 超档/透支徽章 */}
                              <div className="flex items-center justify-between gap-6 border-t pt-1.5">
                                <span className="text-muted-foreground">{t('logs.table.billingTier')}</span>
                                {l.BillingTier ? (
                                  <Badge variant="outline">{l.BillingTier}</Badge>
                                ) : (
                                  <span className="text-muted-foreground">—</span>
                                )}
                              </div>
                              {(l.AboveHit || l.Overdraft) && (
                                <div className="flex items-center justify-end gap-1">
                                  {l.AboveHit && <Badge className="bg-sky-500/10 text-sky-600 dark:bg-sky-400/10 dark:text-sky-400">{t('logs.table.aboveHit')}</Badge>}
                                  {l.Overdraft && <Badge className="bg-rose-500/10 text-rose-600 dark:bg-rose-400/10 dark:text-rose-400">{t('logs.table.overdraft')}</Badge>}
                                </div>
                              )}
                            </div>
                          </TooltipContent>
                        </Tooltip>
                      </span>
                    ) : (
                      <span className="text-xs text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  )}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
        {/* 分页条：独立于表格卡片，对齐 pagination-demo（outline 按钮 + chevron 图标 + 移动端隐藏文字） */}
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="text-sm text-muted-foreground">
            {t('logs.pagination.total', { total })}
            <span className="mx-2">·</span>
            {t('logs.pagination.page', { page, pages })}
          </div>
          <div className="flex items-center gap-2">
            <Select
              items={Object.fromEntries(LIMITS.map(n => [String(n), String(n)]))}
              value={String(limit)}
              onValueChange={changeLimit}
            >
              <SelectTrigger size="sm" aria-label={`${t('list.pageSize')}: ${limit}`}>
                <span className="text-xs">{t('list.pageSize')}</span>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {LIMITS.map(n => <SelectItem key={n} value={String(n)} label={String(n)}>{n}</SelectItem>)}
              </SelectContent>
            </Select>
            <Button variant="outline" size="sm" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - limit))}>
              <ChevronLeft /> <span className="hidden sm:inline">{t('logs.pagination.prev')}</span>
            </Button>
            <Button variant="outline" size="sm" disabled={offset + limit >= total} onClick={() => setOffset(offset + limit)}>
              <span className="hidden sm:inline">{t('logs.pagination.next')}</span> <ChevronRight />
            </Button>
          </div>
        </div>
        </>
      )}
    </div>
  )
}
