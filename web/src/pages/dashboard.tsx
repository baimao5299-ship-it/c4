import { useQuery } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Activity, AlertTriangle, Gauge, PowerOff, Boxes, FolderOpen, Users } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Skeleton } from '@/components/ui/skeleton'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { StatusBadge, statusLabel } from '@/components/status-badge'
import { formatPercent, truncate } from '@/components/fmt'

const fadeUp = {
  initial: { opacity: 0, y: 12 },
  animate: { opacity: 1, y: 0 },
}

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

  const loading = accountsQ.isLoading || templatesQ.isLoading || groupsQ.isLoading

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
    <div className="space-y-4">
      <div>
        <h1 className="text-lg font-semibold">{t('dashboard.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('dashboard.subtitle')}</p>
      </div>

      {loading ? (
        <div className="grid grid-cols-2 gap-4 xl:grid-cols-5">
          {Array.from({ length: 5 }).map((_, i) => (
            <Skeleton key={i} className="h-28" />
          ))}
        </div>
      ) : (
        <>
          {/* 状态计数卡片（交错入场） */}
          <div className="grid grid-cols-2 gap-4 xl:grid-cols-4">
            {statusCards.map(({ key, icon: Icon, descKey }, i) => (
              <motion.div key={key} {...fadeUp} transition={{ duration: 0.25, delay: i * 0.06 }}>
                <Card>
                  <CardHeader className="pb-2">
                    <CardTitle className="flex items-center justify-between text-sm text-muted-foreground">
                      <span className="flex items-center gap-1.5">
                        <Icon className="size-4" /> {statusLabel(key, t)}
                      </span>
                      <StatusBadge status={key} />
                    </CardTitle>
                  </CardHeader>
                  <CardContent>
                    <div className="text-2xl font-semibold tabular-nums">{statusCounts[key]}</div>
                    <p className="text-xs text-muted-foreground">{t(descKey)}</p>
                  </CardContent>
                </Card>
              </motion.div>
            ))}
          </div>

          <div className="grid grid-cols-1 gap-4 xl:grid-cols-3">
            {/* 并发水位 */}
            <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.28 }} className="xl:col-span-1">
              <Card className="h-full">
                <CardHeader>
                  <CardTitle>{t('dashboard.waterTitle')}</CardTitle>
                  <CardDescription>{t('dashboard.waterDesc', { cur: totalCur, max: totalMax })}</CardDescription>
                </CardHeader>
                <CardContent>
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

            {/* 总数统计 */}
            <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.34 }}>
              <Card className="h-full">
                <CardHeader>
                  <CardTitle>{t('dashboard.resourcesTitle')}</CardTitle>
                </CardHeader>
                <CardContent className="space-y-3">
                  {totalCards.map(({ key, labelKey, value, icon: Icon }) => (
                    <div key={key} className="flex items-center justify-between rounded-lg border p-2.5">
                      <span className="flex items-center gap-2 text-sm text-muted-foreground">
                        <Icon className="size-4" /> {t(labelKey)}
                      </span>
                      <span className="text-xl font-semibold tabular-nums">{value}</span>
                    </div>
                  ))}
                </CardContent>
              </Card>
            </motion.div>

            {/* err_rate Top 5 */}
            <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.4 }} className="xl:col-span-1">
              <Card className="h-full">
                <CardHeader>
                  <CardTitle>{t('dashboard.errTopTitle')}</CardTitle>
                  <CardDescription>{t('dashboard.errTopDesc')}</CardDescription>
                </CardHeader>
                <CardContent>
                  {errTop.length === 0 ? (
                    <p className="py-6 text-center text-sm text-muted-foreground">{t('dashboard.errTopEmpty')}</p>
                  ) : (
                    <Table>
                      <TableHeader>
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
                              <Badge className="bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400">
                                {formatPercent(a.err_rate)}
                              </Badge>
                            </TableCell>
                            <TableCell className="text-right tabular-nums">{a.err_count ?? 0}</TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  )}
                </CardContent>
              </Card>
            </motion.div>
          </div>
        </>
      )}
    </div>
  )
}
