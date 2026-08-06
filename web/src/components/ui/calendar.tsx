// shadcn 风格 Calendar，基于 react-day-picker v10（无 Radix 依赖）。
// 文案（周标题/月份/导航 aria）全部走 i18n t()，zh/en 1:1；周起始日随语言
// （zh-CN 周一 / en 周日），与系统日历习惯一致。
import * as React from "react"
import { useTranslation } from "react-i18next"
import { DayPicker } from "react-day-picker"

import { cn } from "@/lib/utils"
import { buttonVariants } from "@/components/ui/button"

const WEEKDAYS = ["sun", "mon", "tue", "wed", "thu", "fri", "sat"] as const

function Calendar({
  className,
  classNames,
  showOutsideDays = true,
  ...props
}: React.ComponentProps<typeof DayPicker>) {
  const { t, i18n } = useTranslation()
  const zh = i18n.resolvedLanguage?.startsWith("zh") ?? false

  return (
    <DayPicker
      showOutsideDays={showOutsideDays}
      // navLayout="around"：上/下月按钮渲染在 MonthCaption 两侧（经典布局），
      // v10 默认布局中 Nav 是 Months 的首个兄弟节点，绝对定位会错位。
      navLayout="around"
      weekStartsOn={zh ? 1 : 0}
      labels={{
        labelPrevious: () => t("calendar.prev"),
        labelNext: () => t("calendar.next"),
      }}
      formatters={{
        formatWeekdayName: (d) => t(`calendar.day.${WEEKDAYS[d.getDay()]}`),
        formatCaption: (m) => {
          const month = t(`calendar.month.${m.getMonth()}`)
          return zh ? `${m.getFullYear()}年${month}` : `${month} ${m.getFullYear()}`
        },
      }}
      className={cn("p-3", className)}
      classNames={{
        months: "flex flex-col sm:flex-row gap-2",
        month: "relative flex flex-col gap-4",
        month_caption: "flex justify-center pt-1 relative items-center w-full",
        caption_label: "text-sm font-medium",
        nav: "flex items-center gap-1",
        button_previous: cn(
          buttonVariants({ variant: "outline", size: "icon-xs" }),
          // z-10：v10 navLayout="around" 下按钮是 month 的直接绝对定位子节点，
          // 与同排的 month_caption（relative + w-full）重叠时会被其盖住而不可点
          // （同一 z-index 层按 DOM 顺序绘制，caption 在后 → 盖住先渲染的按钮）。
          "absolute top-0 left-0 z-10 h-7 w-7 bg-transparent p-0 opacity-50 hover:opacity-100"
        ),
        button_next: cn(
          buttonVariants({ variant: "outline", size: "icon-xs" }),
          "absolute top-0 right-0 z-10 h-7 w-7 bg-transparent p-0 opacity-50 hover:opacity-100"
        ),
        month_grid: "w-full border-collapse",
        // 周标题行与日期行统一为 7 等分 grid（grid-cols-7）→ 列宽一致，数字与周列对齐
        weekdays: "grid w-full grid-cols-7",
        weekday:
          "text-muted-foreground rounded-md font-normal text-[0.8rem] flex items-center justify-center",
        week: "mt-2 grid w-full grid-cols-7",
        day: "relative flex items-center justify-center p-0 aria-selected:opacity-100",
        day_button: cn(
          buttonVariants({ variant: "ghost", size: "icon-xs" }),
          "h-8 w-8 p-0 font-normal aria-selected:opacity-100"
        ),
        range_start: "day-range-start rounded-l-md",
        range_end: "day-range-end rounded-r-md",
        selected:
          "bg-primary text-primary-foreground hover:bg-primary hover:text-primary-foreground focus:bg-primary focus:text-primary-foreground",
        today: "bg-accent text-accent-foreground",
        outside:
          "day-outside text-muted-foreground aria-selected:bg-accent/50 aria-selected:text-muted-foreground",
        disabled: "text-muted-foreground opacity-50",
        hidden: "invisible",
      }}
      {...props}
    />
  )
}

export { Calendar }
