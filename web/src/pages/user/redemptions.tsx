// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 用户端兑换页：顶部兑换区（redeem → 成功展示 applied 回执），
// 下方兑换记录表（page/page_size 1-based 分页）。视觉对齐管理端 redemption-codes
// 卡片结构 + fadeUp 动画。单位语义（2026-08-15 对齐修复）：applied.value / Value =
// API 边界已换算的 USD（直接显示，勿再 /1e5）；concurrency 类型为并发数直出；
// temp_balance 到期时间用 resource_expires_at（RFC3339 → 本地时间）。
import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { CircleCheck, ExternalLink, Ticket, LoaderCircle, ShoppingBag } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import { ApiError, userApi } from '@/lib/api/client'
import { PagePagination } from '@/components/page-pagination'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { toast } from '@/components/ui/toast'
import { formatDateTime } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type RedemptionType = components['schemas']['RedemptionType']
type Applied = components['schemas']['RedeemResponse']['applied']

const fadeUp = {
  initial: { opacity: 0, y: 12 },
  animate: { opacity: 1, y: 0 },
}

// 面值展示（与管理端 redemption-codes 同规则，2026-08-15 对齐修复）：Value 已换算
// USD——直接显示（勿再 /1e5）；concurrency → 并发数直出。
function formatValue(type: RedemptionType, value: number): string {
  return type === 'concurrency' ? String(value) : `$${value.toFixed(2)}`
}

// applied 回执差异化文案：balance → 余额 +USD；concurrency → 并发上限 +N；
// temp_balance → 临时余额 +USD + 到期时间（resource_expires_at → 本地时间）。
function appliedText(a: Applied, t: TFunction): string {
  if (a.type === 'concurrency') {
    return t('user.redemptions.successConcurrency', { value: a.value })
  }
  const value = formatValue(a.type, a.value)
  if (a.type === 'balance') return t('user.redemptions.successBalance', { value })
  return `${t('user.redemptions.successTempBalance', { value })} · ${t('user.redemptions.successExpiresAt', {
    time: a.resource_expires_at ? formatDateTime(a.resource_expires_at) : '—',
  })}`
}

export default function UserRedemptions() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const [code, setCode] = useState('')
  const [applied, setApplied] = useState<Array<{ code: string; applied?: Applied; error?: string }>>([])

  // —— 兑换记录：增强分页范式（page/page_size，1-based）——
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['user', 'redemptions', { page, page_size: pageSize }],
    queryFn: () => userApi.listUserRedemptions({ page, page_size: pageSize }),
  })
  const rows = data?.rows ?? []
  const cardStoreURL = import.meta.env.VITE_CARD_STORE_URL?.trim() || ''

  // 末页死胡同守卫：非首页的当前页数据被清空（如兑换后 total 变化）时回退到第 1 页；
  // 页 1 本身为空（列表真正为空）时无需回退，不会成环。
  useEffect(() => {
    if (!isLoading && !isError && rows.length === 0 && page > 1) setPage(1)
  }, [isLoading, isError, rows.length, page])

  const redeem = useMutation({
    // 保持每个兑换码独立提交：服务端的单码事务和幂等语义不变，批量只是
    // 用户端一次输入、按顺序执行并汇总结果，因此中途失败不会吞掉已成功的码。
    mutationFn: async (codes: string[]) => {
      const results: Array<{ code: string; applied?: Applied; error?: string }> = []
      for (const value of codes) {
        try {
          const result = await userApi.redeem(value)
          results.push({ code: value, applied: result.applied })
        } catch (error) {
          results.push({ code: value, error: error instanceof ApiError ? error.message : t('user.common.error') })
        }
      }
      return results
    },
    onSuccess: results => {
      setApplied(results)
      setCode('')
      // 刷新记录 + 保持当前页（queryKey 前缀失效，当前页数据重新拉取）
      qc.invalidateQueries({ queryKey: ['user', 'redemptions'] })
      // 余额/并发上限可能变化 → 刷新总览数据（overview 余额卡）
      qc.invalidateQueries({ queryKey: ['user', 'me'] })
      const successCount = results.filter(result => result.applied).length
      const failedCount = results.length - successCount
      toast.add({
        title: failedCount ? t('user.redemptions.partialTitle') : t('user.redemptions.successTitle'),
        description: t('user.redemptions.batchSummary', { success: successCount, failed: failedCount }),
        type: failedCount ? 'error' : 'success',
      })
    },
  })

  const submit = () => {
    const values = Array.from(new Set(code.split(/[\s,，;；]+/).map(value => value.trim()).filter(Boolean)))
    if (!values.length || redeem.isPending) return
    if (values.length > 50) {
      toast.add({ title: t('user.redemptions.tooMany'), type: 'error' })
      return
    }
    setApplied([])
    redeem.mutate(values)
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('user.redemptions.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('user.redemptions.subtitle')}</p>
      </div>

      {/* 兑换区（卡片结构对齐管理端 redemption-codes） */}
      <motion.div {...fadeUp} transition={{ duration: 0.25 }}>
        <Card>
          <CardHeader>
            <CardTitle>{t('user.redemptions.codeLabel')}</CardTitle>
            <CardDescription>{t('user.redemptions.codeHint')}</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="flex flex-col gap-3 sm:flex-row">
              <textarea
                className="min-h-24 w-full resize-y rounded-md border border-input bg-background/70 px-3 py-2 text-sm outline-none ring-offset-background placeholder:text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring"
                value={code}
                onChange={e => setCode(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) submit() }}
                placeholder={t('user.redemptions.codePlaceholder')}
                autoComplete="off"
                autoCapitalize="none"
                autoCorrect="off"
                spellCheck={false}
                disabled={redeem.isPending}
              />
              <Button className="shrink-0 sm:self-end" onClick={submit} disabled={!code.trim() || redeem.isPending}>
                {redeem.isPending ? <><LoaderCircle className="animate-spin" />{t('user.redemptions.redeeming')}</> : <><Ticket />{t('user.redemptions.redeem')}</>}
              </Button>
            </div>
            <p className="text-xs text-muted-foreground">{t('user.redemptions.batchHint')}</p>
            {applied.length > 0 && (
              <div className="space-y-2">
                {applied.map(result => result.applied ? (
                  <Alert key={result.code}>
                    <CircleCheck className="size-4 text-emerald-700 dark:text-emerald-400" />
                    <AlertTitle>{result.code}</AlertTitle>
                    <AlertDescription>{appliedText(result.applied, t)}</AlertDescription>
                  </Alert>
                ) : (
                  <Alert key={result.code} variant="destructive"><AlertTitle>{result.code}</AlertTitle><AlertDescription>{result.error || t('user.redemptions.redeemFailed')}</AlertDescription></Alert>
                ))}
              </div>
            )}
          </CardContent>
        </Card>
      </motion.div>

      <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.05 }}>
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2"><ShoppingBag className="size-5 text-primary" />{t('user.redemptions.storeTitle')}</CardTitle>
            <CardDescription>{t('user.redemptions.storeDesc')}</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            {cardStoreURL ? <Button render={<a href={cardStoreURL} target="_blank" rel="noreferrer" />}><ExternalLink />{t('user.redemptions.storeOpen')}</Button> : <p className="text-sm text-muted-foreground">{t('user.redemptions.storeNotConfigured')}</p>}
            <div className="rounded-lg border border-border/70 bg-muted/30 p-3">
              <p className="text-sm font-medium">{t('user.redemptions.storeGuideTitle')}</p>
              <ol className="mt-2 list-decimal space-y-1 pl-5 text-sm leading-6 text-muted-foreground">
                {(['step1', 'step2', 'step3', 'step4', 'step5'] as const).map(step => <li key={step}>{t(`user.redemptions.storeGuide.${step}`)}</li>)}
              </ol>
            </div>
          </CardContent>
        </Card>
      </motion.div>

      {/* 兑换记录表 */}
      <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.1 }}>
        <div className="overflow-hidden rounded-lg">
          {isError ? (
            <p className="p-4 text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
          ) : isLoading ? (
            <div className="space-y-2 p-4">
              {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-10" />)}
            </div>
          ) : rows.length === 0 ? (
            <div className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
              <Ticket className="size-10" />
              <p className="font-medium">{t('user.redemptions.emptyTitle')}</p>
              <p className="text-sm">{t('user.redemptions.emptyDesc')}</p>
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t('user.redemptions.table.code')}</TableHead>
                  <TableHead>{t('user.redemptions.table.type')}</TableHead>
                  <TableHead>{t('user.redemptions.table.value')}</TableHead>
                  <TableHead>{t('user.redemptions.table.remark')}</TableHead>
                  <TableHead>{t('user.redemptions.table.expiresAt')}</TableHead>
                  <TableHead>{t('user.redemptions.table.createdAt')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody className="[&_td]:py-3">
                {rows.map(r => (
                  <TableRow key={r.ID}>
                    <TableCell><code className="font-mono text-sm">{r.Code}</code></TableCell>
                    <TableCell>{t(`redemptions.type.${r.CodeType}`)}</TableCell>
                    <TableCell className="tabular-nums">{formatValue(r.CodeType, r.Value)}</TableCell>
                    <TableCell className="max-w-40 truncate" title={r.Remark ?? undefined}>{r.Remark || '—'}</TableCell>
                    <TableCell>{r.ResourceExpiresAt ? formatDateTime(r.ResourceExpiresAt) : '—'}</TableCell>
                    <TableCell>{formatDateTime(r.CreatedAt)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </div>
        {!isLoading && !isError && (
          <PagePagination
            total={data?.total ?? 0}
            pageSize={pageSize}
            page={page}
            onPageChange={setPage}
            onPageSizeChange={s => { setPageSize(s); setPage(1) }}
          />
        )}
      </motion.div>
    </div>
  )
}
