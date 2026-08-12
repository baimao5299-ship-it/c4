// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { BarChart3 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Area, AreaChart, Bar, BarChart, CartesianGrid, XAxis, YAxis } from 'recharts'
import { api } from '@/App'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig } from '@/components/ui/chart'
import { DateRangePicker } from '@/components/date-range-picker'
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
  InputTokens: number
  OutputTokens: number
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
        RequestCount: 0, ErrorCount: 0, InputTokens: 0, OutputTokens: 0, TotalTokens: 0, TotalLatencyMS: 0,
      }
      map.set(r.BucketTime, b)
    }
    b.RequestCount += r.RequestCount ?? 0
    b.ErrorCount += r.ErrorCount ?? 0
    b.InputTokens += r.InputTokens ?? 0
    b.OutputTokens += r.OutputTokens ?? 0
    b.TotalTokens += r.TotalTokens ?? 0
    b.TotalLatencyMS += r.TotalLatencyMS ?? 0
  }
  return [...map.values()].sort((a, b) => a.time.localeCompare(b.time))
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

  // 主题色走 --chart-* 变量（ChartStyle 注入 --color-requests / --color-tokens）。
  const chartConfig = {
    requests: { label: t('stats.metricRequests'), color: 'var(--chart-1)' },
    tokens: { label: t('stats.metricTokens'), color: 'var(--chart-2)' },
  } satisfies ChartConfig

  // 图表只消费 label + 指标值两列，dataKey 与 chartConfig 键对齐（官方示例写法）。
  const chartData = useMemo(
    () => rows.map(r => ({ label: r.label, requests: r.RequestCount, tokens: r.TotalTokens })),
    [rows]
  )

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('stats.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('stats.subtitle')}</p>
      </div>

      {/* 控制：时间范围 + 粒度 + 指标 */}
      <Card className="p-4">
        {/* 控制：时间范围 + 粒度 + 指标。
            flex-nowrap + 子项 shrink-0 + overflow-x-auto：窄窗口横向滚动而非换行（用户反馈 2）。
            items-start 顶对齐（结构性防下沉，用户反馈 1 两次）：items-end 底对齐下，左列
            （日期区）打开日历后高度变化 → 右列随底部整体下沉——这是唯一垂直下沉路径，与
            换行无关；顶对齐后各列高度互不影响，右列位置固定。Label 顶部对齐视觉也更自然。
            日期区 w-[14rem] 保持。 */}
        <div className="flex flex-nowrap items-start gap-5 overflow-x-auto">
          <div className="w-[14rem] shrink-0 space-y-1.5">
            <Label>{t('dateRange.label')}</Label>
            <DateRangePicker value={range} onChange={setRange} />
          </div>
          <div className="shrink-0 space-y-1.5">
            <Label>{t('stats.granularity')}</Label>
            <Tabs value={granularity} onValueChange={v => v && setGranularity(v as Granularity)}>
              <TabsList>
                <TabsTrigger value="hour">{t('stats.granularityHour')}</TabsTrigger>
                <TabsTrigger value="day">{t('stats.granularityDay')}</TabsTrigger>
              </TabsList>
            </Tabs>
          </div>
          <div className="shrink-0 space-y-1.5">
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
            <Skeleton className="h-[320px] w-full" />
          ) : rows.length === 0 ? (
            <div className="flex flex-col items-center gap-2 py-10 text-muted-foreground">
              <BarChart3 className="size-10" />
              <p className="font-medium">{t('stats.emptyTitle')}</p>
              <p className="text-sm">{t('stats.emptyDesc')}</p>
            </div>
          ) : (
            <ChartContainer config={chartConfig} className="h-[320px] w-full">
              {metric === 'requests' ? (
                <BarChart accessibilityLayer data={chartData}>
                  <CartesianGrid vertical={false} />
                  <XAxis dataKey="label" tickLine={false} tickMargin={10} axisLine={false} fontSize={12} />
                  <YAxis tickLine={false} axisLine={false} tickMargin={8} fontSize={12} allowDecimals={false} />
                  <ChartTooltip content={<ChartTooltipContent />} />
                  <Bar dataKey="requests" fill="var(--color-requests)" radius={4} />
                </BarChart>
              ) : (
                <AreaChart accessibilityLayer data={chartData}>
                  <defs>
                    <linearGradient id="fillTokens" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="5%" stopColor="var(--color-tokens)" stopOpacity={0.8} />
                      <stop offset="95%" stopColor="var(--color-tokens)" stopOpacity={0.1} />
                    </linearGradient>
                  </defs>
                  <CartesianGrid vertical={false} />
                  <XAxis dataKey="label" tickLine={false} tickMargin={10} axisLine={false} fontSize={12} />
                  <YAxis tickLine={false} axisLine={false} tickMargin={8} fontSize={12} allowDecimals={false} />
                  <ChartTooltip content={<ChartTooltipContent />} />
                  <Area dataKey="tokens" type="natural" fill="url(#fillTokens)" stroke="var(--color-tokens)" strokeWidth={2} />
                </AreaChart>
              )}
            </ChartContainer>
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
            <TableBody className="[&_td]:py-3">
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
                      <TableCell className="text-right tabular-nums">{r.InputTokens}</TableCell>
                      <TableCell className="text-right tabular-nums">{r.OutputTokens}</TableCell>
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
