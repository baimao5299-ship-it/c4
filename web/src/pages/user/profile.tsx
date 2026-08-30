// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 用户端个人中心页：账户信息卡（me()——Email/Role/Status/MaxConcurrency/CreatedAt）
// + 临时额度区块（/user/temp-balances——合计 USD + 有效额度列表，FEFO 序即响应序；
// 空结果显示"无临时额度"）+ 修改密码表单（/user/auth/change-password）。
// 单位语义：me().Balance 与 temp-balances 的 amount_usd/total_usd 均为 API 边界
// 已换算的 USD 直显（formatUSD，勿用毫分语义的 formatCost）。
import { useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { KeyRound, Timer, UserRound } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { ApiError, ApiUnauthorized, userApi } from '@/lib/api/client'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { StatusBadge } from '@/components/status-badge'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { toast } from '@/components/ui/toast'
import { formatDateTime, formatUSD } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type TempBalanceRowView = components['schemas']['TempBalanceRow'] & { group_id?: number | null }

const fadeUp = {
  initial: { opacity: 0, y: 12 },
  animate: { opacity: 1, y: 0 },
}

// 账户信息行：label 左 / 值右（对齐 overview 余额卡内容形态）。
function InfoRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3">
      <span className="text-sm text-muted-foreground">{label}</span>
      <span className="text-sm">{children}</span>
    </div>
  )
}

export default function UserProfile() {
  const { t } = useTranslation()

  // 与 AppShell/overview 同键共享缓存（me 一次拉取全局复用）
  const meQ = useQuery({ queryKey: ['user', 'me'], queryFn: () => userApi.me() })
  const tempQ = useQuery({ queryKey: ['user', 'temp-balances'], queryFn: () => userApi.getTempBalances() })
  const tempRows = (tempQ.data?.rows ?? []) as TempBalanceRowView[]

  // —— 修改密码表单（register/login 同款原生 await 提交，非 useMutation：
  // 全局 MutationCache 401 拦截会把"旧密码错误"当会话过期登出，改密码的
  // 401 语义需本地展示）——
  const [oldPwd, setOldPwd] = useState('')
  const [newPwd, setNewPwd] = useState('')
  const [confirm, setConfirm] = useState('')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)

  const submit = async () => {
    if (!oldPwd || !newPwd || !confirm) { setErr(t('user.profile.pwdRequired')); return }
    if (newPwd !== confirm) { setErr(t('user.profile.pwdMismatch')); return }
    setErr('')
    setLoading(true)
    try {
      await userApi.changePassword({ old_password: oldPwd, new_password: newPwd })
      setOldPwd(''); setNewPwd(''); setConfirm('')
      toast.add({ title: t('user.profile.successTitle'), description: t('user.profile.successDesc'), type: 'success' })
    } catch (e) {
      // 401 = 旧密码错误（后端防枚举同登录文案——ApiUnauthorized 非 ApiError 子类）；
      // 400 = 新密码非法（非空且 ≤72 字节）；其余展示服务端 error 字段。
      if (e instanceof ApiUnauthorized) setErr(t('user.profile.oldPasswordWrong'))
      else if (e instanceof ApiError && e.status === 400) setErr(t('user.profile.newPasswordInvalid'))
      else setErr(e instanceof ApiError ? e.message : t('user.common.error'))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('user.profile.title')}</h1>
        <p className="text-sm text-muted-foreground">{t('user.profile.subtitle')}</p>
      </div>

      <div className="grid grid-cols-1 gap-5 xl:grid-cols-2">
        {/* 账户信息卡（me() 现有字段） */}
        <motion.div {...fadeUp} transition={{ duration: 0.25 }}>
          <Card className="h-full">
            <CardHeader>
              <CardDescription className="flex items-center gap-1.5">
                <UserRound className="size-4" /> {t('user.profile.accountTitle')}
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-3">
              {meQ.isError ? (
                <p className="text-sm text-destructive">{t('common.loadFailed', { message: (meQ.error as Error).message })}</p>
              ) : meQ.isLoading ? (
                <div className="space-y-2">
                  {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-5" />)}
                </div>
              ) : (
                <>
                  <InfoRow label={t('user.auth.email')}>{meQ.data?.Email ?? '—'}</InfoRow>
                  <InfoRow label={t('users.roleLabel')}>
                    {meQ.data?.Role ? t(`users.role.${meQ.data.Role}`) : '—'}
                  </InfoRow>
                  <InfoRow label={t('users.statusLabel')}>
                    <StatusBadge status={meQ.data?.Status} />
                  </InfoRow>
                  <InfoRow label={t('user.overview.maxConcurrency')}>
                    {meQ.data?.MaxConcurrency == null ? '—' : meQ.data.MaxConcurrency === 0 ? t('user.overview.unlimited') : meQ.data.MaxConcurrency}
                  </InfoRow>
                  <InfoRow label={t('user.overview.createdAt')}>
                    {meQ.data?.CreatedAt ? formatDateTime(meQ.data.CreatedAt) : '—'}
                  </InfoRow>
                </>
              )}
            </CardContent>
          </Card>
        </motion.div>

        {/* 临时额度卡（/user/temp-balances：合计 USD + FEFO 序有效额度列表） */}
        <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.06 }}>
          <Card className="h-full">
            <CardHeader>
              <CardDescription className="flex items-center gap-1.5">
                <Timer className="size-4" /> {t('user.profile.tempTitle')}
              </CardDescription>
              {tempQ.data && tempRows.length > 0 && (
                <CardTitle className="text-2xl font-semibold tabular-nums">
                  {formatUSD(tempQ.data.total_usd)}
                </CardTitle>
              )}
            </CardHeader>
            <CardContent>
              {tempQ.isError ? (
                <p className="text-sm text-destructive">{t('common.loadFailed', { message: (tempQ.error as Error).message })}</p>
              ) : tempQ.isLoading ? (
                <div className="space-y-2">
                  {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-8" />)}
                </div>
              ) : tempRows.length === 0 ? (
                // 空结果（无有效额度）：total 0 无展示意义，整体空态提示
                <p className="py-6 text-center text-sm text-muted-foreground">{t('user.profile.tempEmpty')}</p>
              ) : (
                <div className="overflow-x-auto rounded-lg border">
                  <div className="min-w-[520px]">
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>{t('user.profile.tempAmount')}</TableHead>
                          <TableHead>{t('user.profile.tempGroup')}</TableHead>
                          <TableHead>{t('user.profile.tempExpiresAt')}</TableHead>
                          <TableHead>{t('user.profile.tempNote')}</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody className="[&_td]:py-3">
                        {tempRows.map(r => (
                          <TableRow key={r.id}>
                            <TableCell className="tabular-nums">{formatUSD(r.amount_usd)}</TableCell>
                            <TableCell className="tabular-nums">{r.group_id == null ? t('user.profile.tempGlobal') : `#${r.group_id}`}</TableCell>
                            <TableCell className="text-xs">{r.expires_at ? formatDateTime(r.expires_at) : t('user.profile.tempPermanent')}</TableCell>
                            <TableCell className="max-w-40 truncate text-xs" title={r.note ?? undefined}>{r.note || '—'}</TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  </div>
                </div>
              )}
            </CardContent>
          </Card>
        </motion.div>
      </div>

      {/* 修改密码表单（提交成功清空 + toast；token_version 递增会立即撤销旧 JWT） */}
      <motion.div {...fadeUp} transition={{ duration: 0.25, delay: 0.12 }}>
        <Card className="max-w-xl">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <KeyRound className="size-4" /> {t('user.profile.pwdTitle')}
            </CardTitle>
            <CardDescription>{t('user.profile.pwdSubtitle')}</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="up-old">{t('user.profile.oldPassword')}</Label>
              <Input id="up-old" type="password" autoComplete="current-password" value={oldPwd} onChange={e => { setOldPwd(e.target.value); setErr('') }} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="up-new">{t('user.profile.newPassword')}</Label>
              <Input id="up-new" type="password" autoComplete="new-password" value={newPwd} onChange={e => { setNewPwd(e.target.value); setErr('') }} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="up-confirm">{t('user.profile.confirmPassword')}</Label>
              <Input id="up-confirm" type="password" autoComplete="new-password" value={confirm} onChange={e => { setConfirm(e.target.value); setErr('') }} onKeyDown={e => { if (e.key === 'Enter') submit() }} />
            </div>
            {err && <p className="text-sm text-destructive">{err}</p>}
            <Button disabled={loading} onClick={submit}>
              {loading ? t('user.profile.submitting') : t('user.profile.submit')}
            </Button>
          </CardContent>
        </Card>
      </motion.div>
    </div>
  )
}
