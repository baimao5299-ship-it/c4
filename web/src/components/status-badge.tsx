import type { TFunction } from 'i18next'
import { useTranslation } from 'react-i18next'
import type { components } from '@/lib/api/schema'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'

export type AccountStatus = components['schemas']['AccountStatus']

const STATUS_META: Record<AccountStatus, { className: string }> = {
  active: { className: 'bg-emerald-500/10 text-emerald-600 dark:bg-emerald-400/10 dark:text-emerald-400' },
  unhealthy: { className: 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400' },
  '429': { className: 'bg-amber-500/10 text-amber-600 dark:bg-amber-400/10 dark:text-amber-400' },
  disabled: { className: 'bg-muted text-muted-foreground' },
}

export function statusLabel(status: AccountStatus | undefined, t: TFunction): string {
  return t(`status.${status ?? 'disabled'}`)
}

export function StatusBadge({ status, className }: { status?: AccountStatus; className?: string }) {
  const { t } = useTranslation()
  const meta = STATUS_META[status ?? 'disabled']
  return <Badge className={cn(meta.className, className)}>{statusLabel(status, t)}</Badge>
}
