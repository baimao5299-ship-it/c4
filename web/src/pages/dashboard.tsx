import { useQuery } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Activity, AlertTriangle, Gauge, PowerOff, Boxes, FolderOpen, Users } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Bar, BarChart, CartesianGrid, XAxis, YAxis } from 'recharts'
import { ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig } from '@/components/ui/chart'
import { api } from '@/App'
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Skeleton } from '@/components/ui/skeleton'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { StatusBadge } from '@/components/status-badge'
import { formatPercent, truncate } from '@/components/fmt'

const fadeUp = {
  initial: { opacity: 0, y: 12 },
  animate: { opacity: 1, y: 0 },
}

// 统计卡 grid 的官方 dashboard-01 处理：浅色卡片带顶部 primary 微渐变 + 细阴影，深色回退纯 card。
const cardGrid = 'grid grid-cols-1 gap-5 *:data-[slot=card]:bg-linear-to-t *:data-[slot=card]:from-primary/5 *:data-[slot=card]:to-card *:data-[slot=card]:shadow-xs dark:*:data-[slot=card]:bg-card'

// errTop 柱图数据行。
type ErrRow = { name: string; err_rate: number; err_count: number }

export default function Dashboard() {
  const { t } = useTranslation()
  // 账号运行时视图 10s 轮询；模板/分组仅加载一次（数量统计）。
  const accountsQ = useQuery({ queryKey: ['accounts'], queryFn: () => api.listAccounts(), refetchInterval: 10_000 })
  const templatesQ = useQuery({ queryKey: ['templates'], queryFn: () => api.listTemplates() })
  const groupsQ = useQuery({ queryKey: ['groups'], queryFn: () => api.listGroups() })

  const accounts = accountsQ.data?.rows ?? []
  const statusCounts = {
    active: accounts.filter(a => a.Status === 'active').length,
    unhealthy: accounts.filter(a => a.Status === 'unhealthy').length,
    '429': accounts.filter(a => a.Status === '429').length,
    disabled: accounts.filter(a => a.Status === 'disabled').length,
  } as const

  // 并发水位：concurrency 求和 / max_concurrency 求和。
  const totalCur = accounts.reduce((s, a) => s + (a.concurrency ?? 0), 0)
  const totalMax = accounts.reduce((s, a) => s + (a.MaxConcurrency ?? 0), 0)
  const water = totalMax > 0 ? Math.min(totalCur / totalMax, 1) : 0

  // err_rate Top 5：err_rate > 0 降序。
  const errTop = accounts
    .filter(a => (a.err_rate ?? 0) > 0)
    .sort((x, y) => (y.err_rate ?? 0) - (x.err_rate ?? 0))
    .slice(0, 5)
  const errData: ErrRow[] = errTop.map(a => ({
    name: a.Name ?? '—',
    err_rate: a.err_rate ?? 0,
    err_count: a.err_count ?? 0,
  }))

  const loading = accountsQ.isLoading || templatesQ.isLoading || groupsQ.isLoading

  // 图表配置（ChartContainer 注入 --color-err_rate，随主题翻转）。
  const chartConfig = {
    err_rate: { label: t('dashboard.tableErrRate'), color: 'var(--primary)' },
  } satisfies ChartConfig

  const statusCards: { key: keyof typeof statusCounts; icon: typeof Activity; descKey: string }[] = [
    { key: 'active', icon: Activity, descKey: 'dashboard.statusCards.active' },
    { key: 'unhealthy', icon: AlertTriangle, descKey: 'dashboard.statusCards.unhealthy' },
    { key: '429', icon: Gauge, descKey: 'dashboard.statusCards.429' },
    { key: 'disabled', icon: PowerOff, descKey: 'dashboard.statusCards.disabled' },
  ]

  const totalCards = [
    { key: 'accounts', labelKey: 'dashboard.totalCards.accounts', value: accounts.length, icon: Users },
    { key: 'templates', labelKey: 'dashboard.totalCards.templates', value: templatesQ.data?.rows.length ?? 0, icon: Boxes },
    { key: 'groups', labelKey: 'dashboard.totalCards.groups', value: groupsQ.data?.rows.length ?? 0, icon: FolderOpen },
  ] as const

  if (accountsQ.isError) {
    return (
      <Alert variant="destructive">
        <AlertTitle>{t('dashboard.loadFailedTitle')}</AlertTitle>
        <AlertDescription>{(accountsQ.error as Error).message}</AlertDescription>
      </Alert>
    )
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('dashboard.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('dashboard.subtitle')}</p>
      </div>

      {loading ? (
        <div className="grid grid-cols-2 gap-5 xl:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-28" />
          ))}
        </div>
      ) : (
        <>
          {/* 状态计数卡片（dashboard-01 section-cards 结构：描述 + 大数字 + 状态徽章） */}
          <div className={`${cardGrid} sm:grid-cols-2 xl:grid-cols-4`}>
            {statusCards.map(({ key, icon: Icon, descKey }, i) => (
              <motion.div key={key} {...fadeUp} transition={{ duration: 0.25, delay: i * 0.06 }}>
                <Card className="@container/card h-full">
                  <CardHeader>
                    <CardDescription className="flex items-center gap-1.5">
                      <Icon className="size-4" /> {t(descKey)}
                    </CardDescription>
                    <CardTitle className="text-2xl font-semibold tabular-nums @[250px]/card:text-3xl">
                      {statusCounts[key]}
                    </CardTitle>
                    <CardAction>
                      <StatusBadge status={key} />
                    </CardAction>
                  </CardHeader>
                </Card>
              </motion.div>
            ))}
          </div>

          {/* 资源总数卡片（同款结构，无徽章位） */}
          <div className={`${cardGrid} sm:grid-cols-3`}>
            {totalCards.map(({ key, labelKey, value, icon: Icon }, i) => (
              <motion.div key={key} {...fadeUp} transition={{ duration: 0.25, delay: 0.26 + i * 0.06 }}>
                <Card className="@container/card h-full">
                  <CardHeader>
                    <CardDescription className="flex items-center gap-1.5">
                      <Icon className="size-4" /> {t(labelKey)}
                    </CardDescription>
                    <CardTitle className="text-2xl font-semibold tabular-nums @[250px]/card:text-3xl">
                      {value}
                    </CardTitle>
                  </CardHeader>
                </Card>
              </motion.div>
            ))}
          </div>

          <div className="grid grid-cols-1 gap-5 xl:grid-cols-3">
            {/* err_rate Top 5 柱图（单序列，颜色走语义 primary，深浅色自适应） */}
            <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.44 }} className="xl:col-span-2">
              <Card className="h-full">
                <CardHeader>
                  <CardTitle>{t('dashboard.errTopTitle')}</CardTitle>
                  <CardDescription>{t('dashboard.errTopDesc')}</CardDescription>
                </CardHeader>
                <CardContent className="px-2 pt-4 sm:px-6 sm:pt-6">
                  {errData.length === 0 ? (
                    <p className="flex h-[320px] items-center justify-center text-sm text-muted-foreground">
                      {t('dashboard.errTopEmpty')}
                    </p>
                  ) : (
                    <ChartContainer config={chartConfig} className="aspect-auto h-[320px] w-full">
                      <BarChart accessibilityLayer data={errData} margin={{ left: 0, right: 8 }}>
                        <defs>
                          <linearGradient id="gpm-errbar-fill" x1="0" y1="0" x2="0" y2="1">
                            <stop offset="0%" stopColor="var(--color-err_rate)" stopOpacity={0.9} />
                            <stop offset="100%" stopColor="var(--color-err_rate)" stopOpacity={0.35} />
                          </linearGradient>
                        </defs>
                        <CartesianGrid vertical={false} />
                        <XAxis
                          dataKey="name"
                          tickLine={false}
                          axisLine={false}
                          tickMargin={8}
                          interval={0}
                          tickFormatter={(v: string) => truncate(v, 8)}
                        />
                        <YAxis
                          width={44}
                          tickLine={false}
                          axisLine={false}
                          tickFormatter={(v: number) => formatPercent(v)}
                        />
                        <ChartTooltip
                          content={
                            <ChartTooltipContent
                              indicator="dot"
                              labelFormatter={(label, payload) => {
                                const row = payload?.[0]?.payload as ErrRow | undefined
                                return row?.name ?? String(label ?? '')
                              }}
                              formatter={(value, _name, item) => {
                                const row = item?.payload as ErrRow | undefined
                                return (
                                  <>
                                    <span className="text-muted-foreground">{t('dashboard.tableErrRate')}</span>
                                    <span className="font-mono font-medium text-foreground tabular-nums">
                                      {formatPercent(Number(value))}
                                    </span>
                                    {row && (
                                      <span className="flex w-full justify-between text-muted-foreground">
                                        <span>{t('dashboard.tableErrCount')}</span>
                                        <span className="font-mono font-medium text-foreground tabular-nums">
                                          {row.err_count}
                                        </span>
                                      </span>
                                    )}
                                  </>
                                )
                              }}
                            />
                          }
                        />
                        <Bar dataKey="err_rate" fill="url(#gpm-errbar-fill)" radius={[4, 4, 0, 0]} maxBarSize={36} />
                      </BarChart>
                    </ChartContainer>
                  )}
                </CardContent>
              </Card>
            </motion.div>

            {/* 并发水位（聚合标量：单值进度条） */}
            <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.5 }}>
              <Card className="h-full">
                <CardHeader>
                  <CardTitle>{t('dashboard.waterTitle')}</CardTitle>
                  <CardDescription>{t('dashboard.waterDesc', { cur: totalCur, max: totalMax })}</CardDescription>
                </CardHeader>
                <CardContent className="flex h-[320px] flex-col justify-center">
                  <div className="h-2.5 w-full overflow-hidden rounded-full bg-muted">
                    <motion.div
                      className="h-full rounded-full bg-primary"
                      initial={{ width: 0 }}
                      animate={{ width: `${Math.max(water * 100, water > 0 ? 2 : 0)}%` }}
                      transition={{ duration: 0.5 }}
                    />
                  </div>
                  <p className="mt-2 text-xs text-muted-foreground">
                    {t('dashboard.waterValue', { percent: formatPercent(water) })} {water > 0.8 && t('dashboard.waterWarning')}
                  </p>
                </CardContent>
              </Card>
            </motion.div>
          </div>

          {/* 最近错误账号明细（dashboard-01 data-table 容器样式：圆角边框 + muted 表头） */}
          <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.56 }}>
            <Card>
              <CardHeader>
                <CardTitle>{t('dashboard.errTopDetailTitle')}</CardTitle>
                <CardDescription>{t('dashboard.errTopDetailDesc')}</CardDescription>
              </CardHeader>
              <CardContent>
                {errTop.length === 0 ? (
                  <p className="py-6 text-center text-sm text-muted-foreground">{t('dashboard.errTopEmpty')}</p>
                ) : (
                  <div className="overflow-hidden rounded-lg border">
                    <Table>
                      <TableHeader className="bg-muted">
                        <TableRow>
                          <TableHead>{t('dashboard.tableAccount')}</TableHead>
                          <TableHead className="text-right">{t('dashboard.tableErrRate')}</TableHead>
                          <TableHead className="text-right">{t('dashboard.tableErrCount')}</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {errTop.map(a => (
                          <TableRow key={a.ID}>
                            <TableCell className="max-w-40 truncate" title={a.Name}>{truncate(a.Name, 14)}</TableCell>
                            <TableCell className="text-right">
                              <Badge variant="destructive">{formatPercent(a.err_rate)}</Badge>
                            </TableCell>
                            <TableCell className="text-right tabular-nums">{a.err_count ?? 0}</TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  </div>
                )}
              </CardContent>
            </Card>
          </motion.div>
        </>
      )}
    </div>
  )
}
