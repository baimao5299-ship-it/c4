import { useQuery } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Activity, AlertTriangle, Gauge, PowerOff, Boxes, FolderOpen, Users } from 'lucide-react'
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
  // 账号运行时视图 10s 轮询；模板/分组仅加载一次（数量统计）。
  const accountsQ = useQuery({ queryKey: ['accounts'], queryFn: api.listAccounts, refetchInterval: 10_000 })
  const templatesQ = useQuery({ queryKey: ['templates'], queryFn: api.listTemplates })
  const groupsQ = useQuery({ queryKey: ['groups'], queryFn: api.listGroups })

  const accounts = accountsQ.data ?? []
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

  const statusCards: { key: keyof typeof statusCounts; icon: typeof Activity; desc: string }[] = [
    { key: 'active', icon: Activity, desc: '正常服务' },
    { key: 'unhealthy', icon: AlertTriangle, desc: '上游异常' },
    { key: '429', icon: Gauge, desc: '被上游限流' },
    { key: 'disabled', icon: PowerOff, desc: '手动停用' },
  ]

  const totalCards = [
    { key: 'accounts', label: '账号总数', value: accounts.length, icon: Users },
    { key: 'templates', label: '模板总数', value: templatesQ.data?.length ?? 0, icon: Boxes },
    { key: 'groups', label: '分组总数', value: groupsQ.data?.length ?? 0, icon: FolderOpen },
  ] as const

  if (accountsQ.isError) {
    return (
      <Alert variant="destructive">
        <AlertTitle>数据加载失败</AlertTitle>
        <AlertDescription>{(accountsQ.error as Error).message}</AlertDescription>
      </Alert>
    )
  }

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-lg font-semibold">总览</h1>
        <p className="text-sm text-muted-foreground">账号运行时视图每 10 秒自动刷新</p>
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
            {statusCards.map(({ key, icon: Icon, desc }, i) => (
              <motion.div key={key} {...fadeUp} transition={{ duration: 0.25, delay: i * 0.06 }}>
                <Card>
                  <CardHeader className="pb-2">
                    <CardTitle className="flex items-center justify-between text-sm text-muted-foreground">
                      <span className="flex items-center gap-1.5">
                        <Icon className="size-4" /> {statusLabel(key)}
                      </span>
                      <StatusBadge status={key} />
                    </CardTitle>
                  </CardHeader>
                  <CardContent>
                    <div className="text-2xl font-semibold tabular-nums">{statusCounts[key]}</div>
                    <p className="text-xs text-muted-foreground">{desc}</p>
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
                  <CardTitle>并发水位</CardTitle>
                  <CardDescription>当前并发 {totalCur} / 总上限 {totalMax}</CardDescription>
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
                    水位 {formatPercent(water)} {water > 0.8 && '— 接近上限，建议扩容'}
                  </p>
                </CardContent>
              </Card>
            </motion.div>

            {/* 总数统计 */}
            <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.34 }}>
              <Card className="h-full">
                <CardHeader>
                  <CardTitle>资源总数</CardTitle>
                </CardHeader>
                <CardContent className="space-y-3">
                  {totalCards.map(({ key, label, value, icon: Icon }) => (
                    <div key={key} className="flex items-center justify-between rounded-lg border p-2.5">
                      <span className="flex items-center gap-2 text-sm text-muted-foreground">
                        <Icon className="size-4" /> {label}
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
                  <CardTitle>错误率排行 Top 5</CardTitle>
                  <CardDescription>err_rate 最高的账号</CardDescription>
                </CardHeader>
                <CardContent>
                  {errTop.length === 0 ? (
                    <p className="py-6 text-center text-sm text-muted-foreground">暂无错误率大于 0 的账号</p>
                  ) : (
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>账号</TableHead>
                          <TableHead className="text-right">错误率</TableHead>
                          <TableHead className="text-right">错误数</TableHead>
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
