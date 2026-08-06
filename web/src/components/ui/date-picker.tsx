// 日期+时间选择器：Popover 内嵌 Calendar（react-day-picker）+ 原生 time 输入。
// 值格式与 datetime-local 一致：'YYYY-MM-DDTHH:mm'（本地时区），'' = 未设置；
// 页面侧沿用 fmt.toRFC3339 转 RFC3339，不改过滤数据流。
// 交互：点触发器弹出日历 → 选日期（保留原时间，无时间则补 00:00）→ 改时间 →
// 点外部关闭；触发器显示当前值，选中后可点右侧 X 快速清除，Popover 底部也有清除按钮。
import { useState } from "react"
import { useTranslation } from "react-i18next"
import { CalendarIcon, X } from "lucide-react"

import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Calendar } from "@/components/ui/calendar"
import { Input } from "@/components/ui/input"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"

const pad2 = (n: number) => String(n).padStart(2, "0")

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
  const [open, setOpen] = useState(false)

  const clear = () => {
    onChange("")
    setOpen(false)
  }

  return (
    <div className={cn("flex items-center gap-1", className)}>
      <Popover open={open} onOpenChange={setOpen}>
        <PopoverTrigger
          id={id}
          render={
            <Button variant="outline" className="min-w-0 flex-1 justify-start gap-2 font-normal" />
          }
        >
          <CalendarIcon className="size-4 shrink-0 text-muted-foreground" />
          {value ? (
            <span className="truncate tabular-nums">{value.replace("T", " ")}</span>
          ) : (
            <span className="text-muted-foreground">{t("datePicker.placeholder")}</span>
          )}
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
          <Input
            type="time"
            aria-label={t("datePicker.time")}
            value={time}
            disabled={!date}
            onChange={(e) => date && onChange(toValue(date, e.target.value))}
            className="flex-1"
          />
          <Button variant="ghost" size="sm" onClick={clear}>
            {t("datePicker.clear")}
          </Button>
        </div>
      </PopoverContent>
      </Popover>
      {/* 快速清除（值非空时显示）：独立按钮避免与 Trigger 的事件/样式耦合 */}
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
