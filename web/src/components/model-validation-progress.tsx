import { useTranslation } from 'react-i18next'
import { LoaderCircle } from 'lucide-react'

export interface ModelValidationProgressProps {
  /** Number of work items the server has actually finished. */
  checked?: number
  /** Number of work items in the server-side snapshot. */
  total?: number
  /** Optional localized label for a different work-unit counter. */
  label?: string
}

/**
 * Render a progress bar only when the API supplied a coherent count pair.
 *
 * The old component rendered an animated partial bar whenever a caller did
 * not have counts. That looked like progress even though the synchronous
 * validation endpoint had not reported any work completed yet. Keeping the
 * unknown state indeterminate makes it impossible to mistake a heartbeat for
 * a percentage; an async endpoint can opt in simply by passing both counts.
 */
export function ModelValidationProgress({ checked, total, label }: ModelValidationProgressProps) {
  const { t } = useTranslation()
  // A queued batch starts with a server snapshot of 0/0. Treat that as
  // "work has not been discovered yet", rather than as an empty completed
  // job: rendering 100% at that point made a live validation look finished.
  // A genuinely empty workload has no progress bar to report, so the same
  // pending state is the least misleading representation there as well.
  const hasCounts = Number.isFinite(checked) && Number.isFinite(total) && (total ?? 0) > 0
  const safeTotal = hasCounts ? Math.max(0, Math.floor(total ?? 0)) : 0
  const safeChecked = hasCounts ? Math.min(safeTotal, Math.max(0, Math.floor(checked ?? 0))) : 0
  const value = hasCounts ? (safeTotal === 0 ? 100 : Math.round((safeChecked / safeTotal) * 100)) : undefined
  const countLabel = hasCounts ? `${safeChecked}/${safeTotal}` : t('upstreams.modelValidationProgressPending')

  return (
    <div className="space-y-1.5" role="status" aria-live="polite" aria-busy={!hasCounts}>
      <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
        <span>{label || t('upstreams.modelValidationProgress')}</span>
        {hasCounts ? <span className="tabular-nums">{countLabel}</span> : (
          <span className="inline-flex items-center gap-1.5">
            <LoaderCircle className="size-3.5 animate-spin" aria-hidden="true" />
            <span>{countLabel}</span>
          </span>
        )}
      </div>
      {value != null && (
        <div
          className="h-1.5 w-full overflow-hidden rounded-full bg-muted"
          role="progressbar"
          aria-label={label || t('upstreams.modelValidationProgress')}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={value}
        >
          <div className="h-full rounded-full bg-primary transition-[width] duration-300" style={{ width: `${value}%` }} />
        </div>
      )}
    </div>
  )
}
