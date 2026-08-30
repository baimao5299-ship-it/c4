import { useTranslation } from 'react-i18next'

export function ModelValidationProgress({ checked, total }: { checked?: number; total?: number }) {
  const { t } = useTranslation()
  const hasCounts = Number.isFinite(checked) && Number.isFinite(total) && (total ?? 0) > 0
  const safeTotal = Math.max(1, total ?? 1)
  const safeChecked = Math.min(safeTotal, Math.max(0, checked ?? 0))
  const value = hasCounts ? Math.round((safeChecked / safeTotal) * 100) : undefined
  return (
    <div className="space-y-1.5" role="status" aria-live="polite">
      <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
        <span>{t('upstreams.modelValidationProgress')}</span>
        {hasCounts && <span className="tabular-nums">{safeChecked}/{safeTotal}</span>}
      </div>
      <div
        className="h-1.5 w-full overflow-hidden rounded-full bg-muted"
        role="progressbar"
        aria-label={t('upstreams.modelValidationProgress')}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={value}
      >
        <div
          className={value == null ? 'h-full w-2/5 animate-pulse rounded-full bg-primary' : 'h-full rounded-full bg-primary transition-[width] duration-300'}
          style={value == null ? undefined : { width: `${value}%` }}
        />
      </div>
    </div>
  )
}
