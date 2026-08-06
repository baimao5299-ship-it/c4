import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { BarChart3 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatDateTime, toRFC3339 } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type StatBucket = components['schemas']['StatBucket']

type Granularity = 'hour' | 'day'
type Metric = 'requests' | 'tokens'

const pad2 = (n: number) => String(n).padStart(2, '0')

// 默认近 24h（组件挂载时固定一次，避免渲染期时间漂移）。
function defaultRange() {
  const to = new Date()
  const from = new Date(to.getTime() - 24 * 3600 * 1000)
  const local = (d: Date) =>
    `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`
  return { from: local(from), to: local(to) }
}

// 后端按 (bucket_time, group, account, template, model, is_error) 返回多行，
// 同一时间桶可能有多行 → 前端按 BucketTime 合并求和（图表与表格共用）。
interface BucketRow {
  time: string
  label: string
  RequestCount: number
  ErrorCount: number
  PromptTokens: number
  CompletionTokens: number
  TotalTokens: number
  TotalLatencyMS: number
}

function mergeBuckets(rows: StatBucket[], granularity: Granularity): BucketRow[] {
  const map = new Map<string, BucketRow>()
  for (const r of rows) {
    if (!r.BucketTime) continue
    let b = map.get(r.BucketTime)
    if (!b) {
      const d = new Date(r.BucketTime)
      b = {
        time: r.BucketTime,
        label: granularity === 'hour'
          ? `${pad2(d.getHours())}:${pad2(d.getMinutes())}`
          : `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`,
        RequestCount: 0, ErrorCount: 0, PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0, TotalLatencyMS: 0,
      }
      map.set(r.BucketTime, b)
    }
    b.RequestCount += r.RequestCount ?? 0
    b.ErrorCount += r.ErrorCount ?? 0
    b.PromptTokens += r.PromptTokens ?? 0
    b.CompletionTokens += r.CompletionTokens ?? 0
    b.TotalTokens += r.TotalTokens ?? 0
    b.TotalLatencyMS += r.TotalLatencyMS ?? 0
  }
  return [...map.values()].sort((a, b) => a.time.localeCompare(b.time))
}

// 轻量自绘 SVG 柱状图（无第三方图表依赖）：rect 高度按最大值缩放，
// 0/50%/100% 网格线 + 稀疏横轴标签（约每 n/8 个桶一个）。
function BarChart({ rows, metric, ariaLabel }: { rows: BucketRow[]; metric: Metric; ariaLabel: string }) {
  const W = 760, H = 200, PL = 12, PR = 12, PT = 16, PB = 30
  const plotW = W - PL - PR
  const plotH = H - PT - PB
  const n = rows.length
  const values = rows.map(r => (metric === 'requests' ? r.RequestCount : r.TotalTokens))
  const max = Math.max(1, ...values)
  const step = Math.max(1, Math.ceil(n / 8))
  const bw = plotW / n
  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="w-full text-foreground" role="img" aria-label={ariaLabel}>
      {[0, 0.5, 1].map(f => (
        <line
          key={f}
          x1={PL} x2={W - PR}
          y1={PT + plotH * (1 - f)} y2={PT + plotH * (1 - f)}
          stroke="currentColor" strokeOpacity={f === 0 ? 0.35 : 0.12} strokeWidth={1}
        />
      ))}
      <text x={PL} y={PT - 4} fontSize={10} fill="currentColor" opacity={0.6}>{max}</text>
      {rows.map((r, i) => {
        const h = (values[i] / max) * plotH
        return (
          <rect
            key={r.time}
            x={PL + i * bw + bw * 0.18}
            y={PT + plotH - h}
            width={bw * 0.64}
            height={values[i] === 0 ? 0 : Math.max(h, 2)}
            rx={2}
            fill="currentColor" opacity={0.85}
          />
        )
      })}
      {rows.map((r, i) =>
        i % step === 0 ? (
          <text key={r.time} x={PL + i * bw + bw / 2} y={H - 10} textAnchor="middle" fontSize={10} fill="currentColor" opacity={0.6}>
            {r.label}
          </text>
        ) : null
      )}
    </svg>
  )
}

export default function Stats() {
  const { t } = useTranslation()
  const [range, setRange] = useState(defaultRange)
  const [granularity, setGranularity] = useState<Granularity>('hour')
  const [metric, setMetric] = useState<Metric>('requests')

  const params = useMemo(
    () => ({ from: toRFC3339(range.from), to: toRFC3339(range.to), granularity }),
    [range, granularity]
  )
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['stats', params],
    queryFn: () => api.getStats(params),
  })
  const rows = useMemo(() => mergeBuckets(data ?? [], granularity), [data, granularity])

  const avgLatency = (r: BucketRow) =>
    r.RequestCount > 0 ? `${Math.round(r.TotalLatencyMS / r.RequestCount)} ms` : '—'

  const metricLabel = t(metric === 'requests' ? 'stats.metricRequests' : 'stats.metricTokens')

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-lg font-semibold">{t('stats.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('stats.subtitle')}</p>
      </div>

      {/* 控制：时间范围 + 粒度 + 指标 */}
      <Card className="p-4">
        <div className="flex flex-wrap items-end gap-4">
          <div className="space-y-1.5">
            <Label htmlFor="st-from">{t('stats.from')}</Label>
            <Input id="st-from" type="datetime-local" value={range.from} onChange={e => setRange(r => ({ ...r, from: e.target.value }))} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="st-to">{t('stats.to')}</Label>
            <Input id="st-to" type="datetime-local" value={range.to} onChange={e => setRange(r => ({ ...r, to: e.target.value }))} />
          </div>
          <div className="space-y-1.5">
            <Label>{t('stats.granularity')}</Label>
            <Tabs value={granularity} onValueChange={v => v && setGranularity(v as Granularity)}>
              <TabsList>
                <TabsTrigger value="hour">{t('stats.granularityHour')}</TabsTrigger>
                <TabsTrigger value="day">{t('stats.granularityDay')}</TabsTrigger>
              </TabsList>
            </Tabs>
          </div>
          <div className="space-y-1.5">
            <Label>{t('stats.metric')}</Label>
            <Tabs value={metric} onValueChange={v => v && setMetric(v as Metric)}>
              <TabsList>
                <TabsTrigger value="requests">{t('stats.metricRequests')}</TabsTrigger>
                <TabsTrigger value="tokens">{t('stats.metricTokens')}</TabsTrigger>
              </TabsList>
            </Tabs>
          </div>
        </div>
      </Card>

      {/* 图表 */}
      <Card>
        <CardHeader>
          <CardTitle>{metric === 'requests' ? t('stats.chartRequestsTitle') : t('stats.chartTokensTitle')}</CardTitle>
          <CardDescription>{t('stats.chartDesc')}</CardDescription>
        </CardHeader>
        <CardContent>
          {isError ? (
            <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
          ) : isLoading ? (
            <Skeleton className="h-52 w-full" />
          ) : rows.length === 0 ? (
            <div className="flex flex-col items-center gap-2 py-10 text-muted-foreground">
              <BarChart3 className="size-10" />
              <p className="font-medium">{t('stats.emptyTitle')}</p>
              <p className="text-sm">{t('stats.emptyDesc')}</p>
            </div>
          ) : (
            <BarChart rows={rows} metric={metric} ariaLabel={metricLabel} />
          )}
        </CardContent>
      </Card>

      {/* 明细表 */}
      <Card className="overflow-hidden">
        {isError ? (
          <p className="p-4 text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t('stats.table.time')}</TableHead>
                <TableHead className="text-right">{t('stats.table.requests')}</TableHead>
                <TableHead className="text-right">{t('stats.table.errors')}</TableHead>
                <TableHead className="text-right">{t('stats.table.promptTokens')}</TableHead>
                <TableHead className="text-right">{t('stats.table.completionTokens')}</TableHead>
                <TableHead className="text-right">{t('stats.table.totalTokens')}</TableHead>
                <TableHead className="text-right">{t('stats.table.avgLatency')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {isLoading
                ? Array.from({ length: 5 }).map((_, i) => (
                    <TableRow key={i}>
                      {Array.from({ length: 7 }).map((_, j) => (
                        <TableCell key={j}><Skeleton className="h-4" /></TableCell>
                      ))}
                    </TableRow>
                  ))
                : rows.length === 0
                  ? (
                    <TableRow>
                      <TableCell colSpan={7} className="py-10 text-center text-muted-foreground">{t('stats.emptyTitle')}</TableCell>
                    </TableRow>
                  )
                  : rows.map(r => (
                    <TableRow key={r.time}>
                      <TableCell className="text-xs text-muted-foreground whitespace-nowrap tabular-nums">{formatDateTime(r.time)}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.RequestCount}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.ErrorCount}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.PromptTokens}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.CompletionTokens}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.TotalTokens}</TableCell>
                      <TableCell className="text-right tabular-nums">{avgLatency(r)}</TableCell>
                    </TableRow>
                  ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
