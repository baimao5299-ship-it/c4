import type { ReactNode } from 'react'
import { ArrowDown, ArrowUp, ArrowUpDown } from 'lucide-react'
import { TableHead } from '@/components/ui/table'
import { cn } from '@/lib/utils'
import type { SortOrder } from '@/components/list-toolbar'

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
          'group inline-flex w-full cursor-pointer items-center gap-1 font-medium',
          active ? 'text-foreground' : 'text-muted-foreground hover:text-foreground'
        )}
      >
        {label}
        {active ? (
          order === 'desc' ? <ArrowDown className="size-3.5" /> : <ArrowUp className="size-3.5" />
        ) : (
          <ArrowUpDown className="size-3.5 opacity-0 transition-opacity group-hover:opacity-60" />
        )}
      </button>
    </TableHead>
  )
}
