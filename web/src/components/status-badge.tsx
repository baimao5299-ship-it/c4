import type { components } from '@/lib/api/schema'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'

export type AccountStatus = components['schemas']['AccountStatus']

const STATUS_META: Record<AccountStatus, { label: string; className: string }> = {
  active: { label: '可用', className: 'bg-emerald-500/10 text-emerald-600 dark:bg-emerald-400/10 dark:text-emerald-400' },
  unhealthy: { label: '不健康', className: 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400' },
  '429': { label: '限流中', className: 'bg-amber-500/10 text-amber-600 dark:bg-amber-400/10 dark:text-amber-400' },
  disabled: { label: '已禁用', className: 'bg-muted text-muted-foreground' },
}

export function statusLabel(status?: AccountStatus): string {
  return (status && STATUS_META[status].label) ?? STATUS_META.disabled.label
}

export function StatusBadge({ status, className }: { status?: AccountStatus; className?: string }) {
  const meta = STATUS_META[status ?? 'disabled']
  return <Badge className={cn(meta.className, className)}>{meta.label}</Badge>
}
