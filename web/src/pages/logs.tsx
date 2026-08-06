import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ChevronLeft, ChevronRight, FileText, RotateCcw } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatDateTime } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type ErrorType = components['schemas']['ErrorType']
type RequestFormat = components['schemas']['RequestFormat']

const ERROR_TYPES: ErrorType[] = ['none', '429', '4xx', '5xx', 'network', 'auth', 'no_account', 'abort']

// brief Step 1 色板：none 绿 / 4xx 黄 / 5xx、network、abort 红 / 429 橙 / auth、no_account 灰。
const ERROR_META: Record<ErrorType, string> = {
  none: 'bg-emerald-500/10 text-emerald-600 dark:bg-emerald-400/10 dark:text-emerald-400',
  '4xx': 'bg-yellow-500/10 text-yellow-600 dark:bg-yellow-400/10 dark:text-yellow-400',
  '5xx': 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  network: 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  abort: 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400',
  '429': 'bg-orange-500/10 text-orange-600 dark:bg-orange-400/10 dark:text-orange-400',
  auth: 'bg-muted text-muted-foreground',
  no_account: 'bg-muted text-muted-foreground',
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

const LIMITS = [10, 20, 50]
// base-ui Select 不接受空串值，用哨兵表示「全部」。
const ERROR_ALL = '__all__'

// datetime-local → RFC3339（本地时区输入 → UTC ISO）。
function toRFC3339(v: string): string | undefined {
  if (!v) return undefined
  const d = new Date(v)
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString()
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
  const { t } = useTranslation()
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

  const total = data?.total ?? 0
  const rows = data?.rows ?? []
  const pages = Math.max(1, Math.ceil(total / limit))
  const page = total === 0 ? 1 : Math.floor(offset / limit) + 1

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-lg font-semibold">{t('logs.title')}</h1>
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
                <SelectItem value={ERROR_ALL}>{t('logs.filter.all')}</SelectItem>
                {ERROR_TYPES.map(et => <SelectItem key={et} value={et}>{t(`errorType.${et}`)}</SelectItem>)}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="log-from">{t('logs.filter.from')}</Label>
            <Input id="log-from" type="datetime-local" value={filters.from} onChange={e => set({ from: e.target.value })} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="log-to">{t('logs.filter.to')}</Label>
            <Input id="log-to" type="datetime-local" value={filters.to} onChange={e => set({ to: e.target.value })} />
          </div>
          <div className="flex items-end">
            <Button variant="outline" className="w-full" onClick={() => setFilters(emptyFilters())}>
              <RotateCcw /> {t('logs.filter.reset')}
            </Button>
          </div>
        </div>
      </Card>

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
        <Card className="overflow-hidden">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t('logs.table.requestId')}</TableHead>
                <TableHead>{t('logs.table.createdAt')}</TableHead>
                <TableHead className="text-right">{t('logs.table.group')}</TableHead>
                <TableHead className="text-right">{t('logs.table.account')}</TableHead>
                <TableHead>{t('logs.table.model')}</TableHead>
                <TableHead>{t('logs.table.mappedModel')}</TableHead>
                <TableHead>{t('logs.table.format')}</TableHead>
                <TableHead className="text-right">{t('logs.table.statusCode')}</TableHead>
                <TableHead>{t('logs.table.errorType')}</TableHead>
                <TableHead className="text-right">{t('logs.table.latency')}</TableHead>
                <TableHead className="text-right">{t('logs.table.promptTokens')}</TableHead>
                <TableHead className="text-right">{t('logs.table.completionTokens')}</TableHead>
                <TableHead className="text-right">{t('logs.table.totalTokens')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map(l => (
                <TableRow key={l.ID}>
                  <TableCell className="max-w-36">
                    <span className="block truncate font-mono text-xs text-muted-foreground" title={l.RequestID}>{l.RequestID ?? '—'}</span>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(l.CreatedAt)}</TableCell>
                  <TableCell className="text-right tabular-nums">{l.GroupID ? `#${l.GroupID}` : '—'}</TableCell>
                  <TableCell className="text-right tabular-nums">{l.AccountID ? `#${l.AccountID}` : '—'}</TableCell>
                  <TableCell className="max-w-32 truncate" title={l.Model}>{l.Model ?? '—'}</TableCell>
                  <TableCell className="max-w-32 truncate" title={l.MappedModel ?? ''}>{l.MappedModel ?? '—'}</TableCell>
                  <TableCell>
                    {l.Format ? <Badge variant="outline">{FORMAT_LABELS[l.Format]}</Badge> : <span className="text-xs text-muted-foreground">—</span>}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">{l.StatusCode ?? '—'}</TableCell>
                  <TableCell><ErrorTypeBadge type={l.ErrorType} /></TableCell>
                  <TableCell className="text-right tabular-nums">{l.LatencyMS != null ? `${l.LatencyMS} ms` : '—'}</TableCell>
                  <TableCell className="text-right tabular-nums">{l.PromptTokens ?? 0}</TableCell>
                  <TableCell className="text-right tabular-nums">{l.CompletionTokens ?? 0}</TableCell>
                  <TableCell className="text-right tabular-nums">{l.TotalTokens ?? 0}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <div className="flex flex-wrap items-center justify-between gap-3 border-t px-4 py-2.5">
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
                <SelectTrigger size="sm"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {LIMITS.map(n => <SelectItem key={n} value={String(n)}>{n}</SelectItem>)}
                </SelectContent>
              </Select>
              <Button variant="outline" size="sm" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - limit))}>
                <ChevronLeft /> {t('logs.pagination.prev')}
              </Button>
              <Button variant="outline" size="sm" disabled={offset + limit >= total} onClick={() => setOffset(offset + limit)}>
                {t('logs.pagination.next')} <ChevronRight />
              </Button>
            </div>
          </div>
        </Card>
      )}
    </div>
  )
}
