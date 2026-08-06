// 日期+时间选择器：可直接输入（YYYY-MM-DD [HH:mm]），也可点日历按钮弹出
// Calendar（react-day-picker）+ 小时/分钟两个下拉选择。
// 值格式与 datetime-local 一致：'YYYY-MM-DDTHH:mm'（本地时区），'' = 未设置；
// 页面侧沿用 fmt.toRFC3339 转 RFC3339，不改过滤数据流。
// 交互：在输入框手输（Enter/失焦生效）或点日历按钮 → 选日期（保留原时间，无时间则补
// 00:00）→ 改小时/分钟 → 点外部关闭；选中后可点右侧 X 快速清除，Popover 底部也有清除按钮。
import { useEffect, useState } from "react"
import { useTranslation } from "react-i18next"
import { CalendarIcon, X } from "lucide-react"

import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Calendar } from "@/components/ui/calendar"
import { Input } from "@/components/ui/input"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"

const pad2 = (n: number) => String(n).padStart(2, "0")

// 小时 00-23（24 项）；分钟 5 分钟步进 00/05/.../55（12 项）——管理台过滤粒度足够。
const HOURS = Array.from({ length: 24 }, (_, i) => pad2(i))
const MINUTES = Array.from({ length: 12 }, (_, i) => pad2(i * 5))
const HOUR_ITEMS = Object.fromEntries(HOURS.map((h) => [h, h]))
const MINUTE_ITEMS = Object.fromEntries(MINUTES.map((m) => [m, m]))

function parseValue(v: string): { date: Date | undefined; time: string } {
  const m = /^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2})$/.exec(v)
  if (!m) return { date: undefined, time: "" }
  const date = new Date(`${m[1]}T00:00:00`)
  if (Number.isNaN(date.getTime())) return { date: undefined, time: "" }
  return { date, time: m[2] }
}

function toValue(d: Date, time: string): string {
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${time || "00:00"}`
}

// '07:33' → 归一到 5 分钟步进 '07:35'（历史值可能不在步进上，展示与输出统一到步进）
function snapMinute(mm: string): string {
  return pad2((Math.round(Number(mm) / 5) * 5) % 60)
}

/** 手输解析：'YYYY-MM-DD'、'YYYY-MM-DD HH:mm'、'YYYY-MM-DDTHH:mm' → 合法返回，否则 null */
function parseInput(s: string): string | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})(?:[ T](\d{2}):(\d{2}))?$/.exec(s.trim())
  if (!m) return null
  const [, y, mo, d, hh = "00", mm = "00"] = m
  const date = new Date(`${y}-${mo}-${d}T${hh}:${mm}:00`)
  if (Number.isNaN(date.getTime())) return null
  // 防 Date 自动进位（如 2026-02-31 → 03-03）
  if (
    date.getFullYear() !== Number(y) ||
    date.getMonth() + 1 !== Number(mo) ||
    date.getDate() !== Number(d)
  ) {
    return null
  }
  return `${y}-${mo}-${d}T${hh}:${mm}`
}

export interface DateTimePickerProps {
  /** 'YYYY-MM-DDTHH:mm'（本地时区），'' = 未设置 */
  value: string
  onChange: (v: string) => void
  id?: string
  className?: string
}

export function DateTimePicker({ value, onChange, id, className }: DateTimePickerProps) {
  const { t } = useTranslation()
  const { date, time } = parseValue(value)
  const hh = time ? time.slice(0, 2) : "00"
  const mm = time ? snapMinute(time.slice(3, 5)) : "00"
  const [open, setOpen] = useState(false)
  // 输入框本地文本：跟随已提交值，输入过程中不受过滤数据回流影响
  const [text, setText] = useState(value ? value.replace("T", " ") : "")
  useEffect(() => {
    setText(value ? value.replace("T", " ") : "")
  }, [value])

  const commitInput = () => {
    const v = parseInput(text)
    if (v != null) onChange(v)
    else setText(value ? value.replace("T", " ") : "") // 非法输入回退
  }

  const clear = () => {
    onChange("")
    setOpen(false)
  }

  return (
    <div className={cn("flex items-center gap-1", className)}>
      <Input
        id={id}
        value={text}
        placeholder={t("datePicker.placeholder")}
        onChange={(e) => setText(e.target.value)}
        onBlur={commitInput}
        onKeyDown={(e) => {
          if (e.key === "Enter") commitInput()
        }}
        className="min-w-0 flex-1 tabular-nums"
      />
      <Popover open={open} onOpenChange={setOpen}>
        <PopoverTrigger
          render={
            <Button
              variant="outline"
              size="icon-sm"
              className="shrink-0"
              aria-label={t("datePicker.placeholder")}
            />
          }
        >
          <CalendarIcon className="size-4" />
        </PopoverTrigger>
        <PopoverContent align="start" className="p-0">
          <Calendar
            mode="single"
            selected={date}
            defaultMonth={date}
            onSelect={(d) => d && onChange(toValue(d, time))}
            className="p-2"
          />
          <div className="flex items-center gap-2 border-t p-3">
            <Select
              items={HOUR_ITEMS}
              value={hh}
              disabled={!date}
              onValueChange={(v) => {
                if (v != null && date) onChange(toValue(date, `${v}:${mm}`))
              }}
            >
              <SelectTrigger className="w-16" aria-label={t("datePicker.hour")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent className="max-h-60">
                {HOURS.map((h) => (
                  <SelectItem key={h} value={h}>
                    {h}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <span className="select-none text-sm text-muted-foreground">:</span>
            <Select
              items={MINUTE_ITEMS}
              value={mm}
              disabled={!date}
              onValueChange={(v) => {
                if (v != null && date) onChange(toValue(date, `${hh}:${v}`))
              }}
            >
              <SelectTrigger className="w-16" aria-label={t("datePicker.minute")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent className="max-h-60">
                {MINUTES.map((m) => (
                  <SelectItem key={m} value={m}>
                    {m}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <div className="flex-1" />
            <Button variant="ghost" size="sm" onClick={clear}>
              {t("datePicker.clear")}
            </Button>
          </div>
        </PopoverContent>
      </Popover>
      {/* 快速清除（值非空时显示）：独立按钮避免与输入框/日历按钮的事件耦合 */}
      {value && (
        <Button
          variant="ghost"
          size="icon-sm"
          className="shrink-0 text-muted-foreground"
          title={t("datePicker.clear")}
          onClick={() => onChange("")}
        >
          <X />
        </Button>
      )}
    </div>
  )
}
