import type { TFunction } from 'i18next'
import { useTranslation } from 'react-i18next'
import type { components } from '@/lib/api/schema'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'

export type AccountStatus = components['schemas']['AccountStatus']

// 状态色只做小圆点 + 文字着色（玻璃底由 Badge 提供），不做整块状态色填充。
const STATUS_META: Record<AccountStatus, { dot: string; text: string }> = {
  active: { dot: 'bg-teal-500', text: 'text-teal-700' },
  unhealthy: { dot: 'bg-amber-500', text: 'text-amber-700' },
  '429': { dot: 'bg-orange-500', text: 'text-orange-700' },
  disabled: { dot: 'bg-rose-300', text: 'text-rose-400' },
}

export function statusLabel(status: AccountStatus | undefined, t: TFunction): string {
  return t(`status.${status ?? 'disabled'}`)
}

export function StatusBadge({ status, className }: { status?: AccountStatus; className?: string }) {
  const { t } = useTranslation()
  const meta = STATUS_META[status ?? 'disabled']
  return (
    <Badge className={cn('border-white/50 bg-white/45 shadow-[inset_0_1px_0_rgba(255,255,255,0.65)] backdrop-blur-md', meta.text, className)}>
      <span className={cn('size-1.5 shrink-0 rounded-full', meta.dot)} />
      {statusLabel(status, t)}
    </Badge>
  )
}
