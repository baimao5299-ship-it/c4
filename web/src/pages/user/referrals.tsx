// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Check, Clock3, Copy, Gift, UsersRound, WalletCards } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { formatDateTime, formatUSD } from '@/components/fmt'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { toast } from '@/components/ui/toast'
import { userApi, type ReferralReward, type ReferralSummary } from '@/lib/api/client'

function statusTone(status: string): string {
  if (status === 'claimed' || status === 'credited') return 'text-emerald-700 dark:text-emerald-400'
  if (status === 'reversed') return 'text-muted-foreground'
  if (status === 'claimable') return 'text-primary'
  return 'text-amber-700 dark:text-amber-400'
}

function displayStatus(reward: ReferralReward): string {
  if (reward.status === 'credited') return 'claimed'
  if (reward.status === 'reversed') return 'reversed'
  if (reward.status === 'pending') {
    const availableAt = new Date(reward.available_at).getTime()
    return Number.isFinite(availableAt) && availableAt <= Date.now() ? 'claimable' : 'frozen'
  }
  return reward.status
}

export default function UserReferrals() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const [copied, setCopied] = useState<'code' | 'link' | null>(null)
  const query = useQuery({ queryKey: ['user', 'referrals'], queryFn: () => userApi.getReferrals() })
  const claim = useMutation({
    mutationFn: () => userApi.claimReferralRewards(),
    onSuccess: summary => {
      qc.setQueryData<ReferralSummary>(['user', 'referrals'], summary)
      qc.invalidateQueries({ queryKey: ['user', 'me'] })
      toast.add({ title: t('user.referrals.claimed'), type: 'success' })
    },
  })

  const copy = async (kind: 'code' | 'link', value: string) => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(kind)
      setTimeout(() => setCopied(current => current === kind ? null : current), 1600)
      toast.add({ title: t('common.copied'), type: 'success' })
    } catch {
      toast.add({ title: t('user.referrals.copyFailed'), type: 'error' })
    }
  }

  const data = query.data
  if (query.isLoading) return <div className="space-y-4"><Skeleton className="h-24" /><Skeleton className="h-36" /><Skeleton className="h-64" /></div>
  if (query.isError || !data) return <p role="alert" className="text-sm text-destructive">{t('common.loadFailed', { message: (query.error as Error)?.message ?? t('user.common.error') })}</p>

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('user.referrals.title')}</h1>
        <p className="mt-1 text-sm text-muted-foreground">{t('user.referrals.subtitle')}</p>
      </div>

      <section aria-labelledby="invite-heading" className="space-y-3 rounded-lg border bg-background/70 p-4 sm:p-5">
        <div>
          <h2 id="invite-heading" className="font-semibold">{t('user.referrals.inviteTitle')}</h2>
          <p className="mt-1 text-sm text-muted-foreground">{t('user.referrals.inviteHint')}</p>
        </div>
        <div className="grid gap-3 md:grid-cols-[minmax(0,0.7fr)_minmax(0,1.3fr)]">
          <div className="space-y-1.5">
            <span className="text-xs font-medium text-muted-foreground">{t('user.referrals.code')}</span>
            <div className="flex min-h-12 items-center gap-2 rounded-md border bg-muted/30 px-3">
              <code className="min-w-0 flex-1 truncate text-base font-semibold tracking-normal">{data.invite_code}</code>
              <Button size="icon" variant="ghost" className="size-11 shrink-0" title={t('user.referrals.copyCode')} onClick={() => { void copy('code', data.invite_code) }}>
                {copied === 'code' ? <Check /> : <Copy />}
              </Button>
            </div>
          </div>
          <div className="space-y-1.5">
            <span className="text-xs font-medium text-muted-foreground">{t('user.referrals.link')}</span>
            <div className="flex min-h-12 items-center gap-2 rounded-md border bg-muted/30 px-3">
              <span className="min-w-0 flex-1 truncate text-sm" title={data.invite_link}>{data.invite_link}</span>
              <Button size="icon" variant="ghost" className="size-11 shrink-0" title={t('user.referrals.copyLink')} onClick={() => { void copy('link', data.invite_link) }}>
                {copied === 'link' ? <Check /> : <Copy />}
              </Button>
            </div>
          </div>
        </div>
      </section>

      <section aria-label={t('user.referrals.summary')} className="grid grid-cols-2 gap-px overflow-hidden rounded-lg border bg-border lg:grid-cols-4">
        <SummaryItem icon={UsersRound} label={t('user.referrals.invited')} value={String(data.invited_count)} />
        <SummaryItem icon={Clock3} label={t('user.referrals.frozen')} value={formatUSD(data.frozen_amount)} />
        <SummaryItem icon={Gift} label={t('user.referrals.claimable')} value={formatUSD(data.claimable_amount)} />
        <SummaryItem icon={WalletCards} label={t('user.referrals.claimedTotal')} value={formatUSD(data.claimed_amount)} />
      </section>

      <div className="flex flex-col gap-3 rounded-lg border border-primary/20 bg-primary/5 p-4 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex min-w-0 gap-3">
          <Clock3 className="mt-0.5 size-5 shrink-0 text-primary" />
          <div><p className="text-sm font-medium">{t('user.referrals.freezeTitle')}</p><p className="mt-0.5 text-xs text-muted-foreground">{t('user.referrals.freezeHint')}</p></div>
        </div>
        <Button className="min-h-11 w-full sm:w-auto" disabled={claim.isPending || data.claimable_amount <= 0} onClick={() => claim.mutate()}>
          <Gift />{claim.isPending ? t('user.referrals.claiming') : t('user.referrals.claim')}
        </Button>
      </div>
      {claim.isError && <p role="alert" className="text-sm text-destructive">{(claim.error as Error).message}</p>}

      <section aria-labelledby="reward-heading" className="space-y-3">
        <div><h2 id="reward-heading" className="text-lg font-semibold">{t('user.referrals.rewards')}</h2><p className="text-sm text-muted-foreground">{t('user.referrals.rewardsHint')}</p></div>
        {data.rewards.length === 0 ? (
          <div className="rounded-lg border border-dashed px-4 py-10 text-center text-sm text-muted-foreground">{t('user.referrals.empty')}</div>
        ) : (
          <div className="divide-y overflow-hidden rounded-lg border bg-background/70">
            {data.rewards.map(reward => <RewardRow key={reward.id} reward={reward} />)}
          </div>
        )}
      </section>
    </div>
  )
}

function SummaryItem({ icon: Icon, label, value }: { icon: typeof UsersRound; label: string; value: string }) {
  return <div className="min-w-0 bg-background p-4"><div className="flex items-center gap-2 text-xs text-muted-foreground"><Icon className="size-4 shrink-0" /><span className="truncate">{label}</span></div><p className="mt-2 truncate text-xl font-semibold tabular-nums" title={value}>{value}</p></div>
}

function RewardRow({ reward }: { reward: ReferralReward }) {
  const { t } = useTranslation()
  const status = displayStatus(reward)
  const invitee = reward.invitee_email ?? (reward.invitee_id == null ? t('user.referrals.invitee') : t('user.referrals.inviteeId', { id: reward.invitee_id }))
  return (
    <div className="grid gap-3 p-4 sm:grid-cols-[minmax(0,1fr)_auto] sm:items-center">
      <div className="min-w-0"><p className="truncate text-sm font-medium" title={invitee}>{invitee}</p><p className="mt-1 text-xs text-muted-foreground">{t(`user.referrals.source.${reward.source_type}`, { defaultValue: reward.source_type })} · {formatDateTime(reward.created_at)}</p></div>
      <div className="flex items-center justify-between gap-3 sm:justify-end"><div className="text-right"><p className="font-semibold tabular-nums">+{formatUSD(reward.reward_amount)}</p><p className="text-xs text-muted-foreground">{t('user.referrals.baseAmount', { amount: formatUSD(reward.base_amount) })}</p></div><Badge variant="secondary" className={statusTone(status)}>{t(`user.referrals.status.${status}`, { defaultValue: status })}</Badge></div>
    </div>
  )
}
