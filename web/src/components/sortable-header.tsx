// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import type { ReactNode } from 'react'
import { ArrowDown, ArrowUp, ArrowUpDown } from 'lucide-react'
import { TableHead } from '@/components/ui/table'
import { cn } from '@/lib/utils'

export type SortOrder = 'asc' | 'desc'

// 可排序列头：active 显示方向箭头，inactive hover 显示双向图标（三态由父级驱动：
// 无排序 → 该列降序 → 升序 → 取消排序）。整列 th 可点击（按钮语义）。
export function SortableHeader({
  field,
  label,
  active,
  order,
  onToggle,
  className,
}: {
  field: string
  label: ReactNode
  active: boolean
  order: SortOrder
  onToggle: (field: string) => void
  className?: string
}) {
  return (
    <TableHead className={className}>
      <button
        type="button"
        onClick={() => onToggle(field)}
        className={cn(
          'group relative inline-flex w-full cursor-pointer items-center gap-1 font-medium',
          active ? 'text-foreground' : 'text-muted-foreground hover:text-foreground'
        )}
      >
        {label}
        {active ? (
          order === 'desc' ? <ArrowDown className="size-3.5" /> : <ArrowUp className="size-3.5" />
        ) : (
          // 非 active 箭头不占位（absolute，th px-2 padding 区内浮显）：
          // 占位会让右对齐列的表头文字比数据偏左一个箭头宽（字段与内容不对齐）。
          <ArrowUpDown className="pointer-events-none absolute -right-2 top-1/2 size-3.5 -translate-y-1/2 opacity-0 transition-opacity group-hover:opacity-60" />
        )}
      </button>
    </TableHead>
  )
}
