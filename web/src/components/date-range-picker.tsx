// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 时间范围选择器：单入口 Popover——react-day-picker range 日历选起止日期 +
// from/to 各一个原生 time input（官方 calendar-time 同款，指示器隐藏）。
// 值格式与 DateTimePicker 一致：'YYYY-MM-DDTHH:mm'（本地时区），'' = 未设置。
// 交互：日历选范围保留原时间（无则 00:00）；改时间只动对应端；点外部关闭。
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { CalendarIcon } from 'lucide-react'
import type { DateRange } from 'react-day-picker'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { Calendar } from '@/components/ui/calendar'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'

const pad2 = (n: number) => String(n).padStart(2, '0')

function localDateStr(d: Date): string {
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`
}
function timeOf(v: string): string {
  return /T(\d{2}:\d{2})/.exec(v)?.[1] ?? '00:00'
}
function withTime(v: string, time: string): string {
  return `${v.slice(0, 10)}T${time}`
}
// 'YYYY-MM-DDTHH:mm' → 'YYYY-MM-DD HH:mm'（trigger 展示，数字格式双语通用）
function fmt(v: string): string {
  return v.replace('T', ' ')
}

export interface DateRangeValue {
  from: string
  to: string
}

export function DateRangePicker({
  value,
  onChange,
  className,
}: {
  value: DateRangeValue
  onChange: (v: DateRangeValue) => void
  className?: string
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)

  const selected: DateRange | undefined =
    value.from && value.to
      ? { from: new Date(`${value.from.slice(0, 10)}T00:00:00`), to: new Date(`${value.to.slice(0, 10)}T00:00:00`) }
      : undefined

  // 日历选范围：日期替换 + 保留两端原时间（无则 00:00）；选单天 → from=to。
  const onSelect = (r: DateRange | undefined) => {
    if (!r?.from) return
    const toDate = r.to ?? r.from
    onChange({ from: withTime(localDateStr(r.from), timeOf(value.from)), to: withTime(localDateStr(toDate), timeOf(value.to)) })
  }

  // 时间变更：只动对应端，日期沿用当前值（无日期则用 from 的日期或今天）。
  const setTime = (which: 'from' | 'to', time: string) => {
    const base = value[which] || value.from
    const date = base ? base.slice(0, 10) : localDateStr(new Date())
    onChange({ ...value, [which]: `${date}T${time || '00:00'}` })
  }

  const timeInputCls =
    'h-8 rounded-md border border-input bg-transparent px-2 text-sm text-foreground outline-none focus-visible:border-ring [&::-webkit-calendar-picker-indicator]:hidden [&::-webkit-calendar-picker-indicator]:appearance-none'

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        render={
          <Button variant="outline" size="lg" className={cn('w-full justify-start gap-2 font-normal', className)} />
        }
      >
        <CalendarIcon className="size-4 text-muted-foreground" />
        {value.from || value.to ? (
          <span className="truncate">
            {value.from ? fmt(value.from) : '…'} — {value.to ? fmt(value.to) : '…'}
          </span>
        ) : (
          <span className="text-muted-foreground">{t('dateRange.placeholder')}</span>
        )}
      </PopoverTrigger>
      <PopoverContent align="start" className="w-auto p-0">
        <Calendar mode="range" numberOfMonths={2} selected={selected} onSelect={onSelect} defaultMonth={selected?.from} />
        <div className="flex items-center gap-3 border-t p-3">
          <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
            {t('dateRange.from')}
            <input type="time" step={300} value={timeOf(value.from)} onChange={e => setTime('from', e.target.value)} className={timeInputCls} />
          </label>
          <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
            {t('dateRange.to')}
            <input type="time" step={300} value={timeOf(value.to)} onChange={e => setTime('to', e.target.value)} className={timeInputCls} />
          </label>
        </div>
      </PopoverContent>
    </Popover>
  )
}
