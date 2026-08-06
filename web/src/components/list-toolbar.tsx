import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { ArrowDownAZ, ArrowUpAZ, Search, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

export interface SortOption {
  value: string
  label: string
}

export type SortOrder = 'asc' | 'desc'

// 列表页通用工具栏：名称搜索 + 排序字段/顺序切换 + 资源专属筛选。
// 全部受控（props 驱动），children 由页面注入资源专属筛选（status 多选、template 下拉等）。
export function ListToolbar({
  name,
  onNameChange,
  sort,
  onSortChange,
  order,
  onOrderChange,
  sortOptions,
  children,
}: {
  name: string
  onNameChange: (name: string) => void
  sort: string
  onSortChange: (sort: string) => void
  order: SortOrder
  onOrderChange: (order: SortOrder) => void
  sortOptions: SortOption[]
  children?: ReactNode
}) {
  const { t } = useTranslation()
  return (
    <div className="flex flex-wrap items-center gap-2">
      <div className="relative w-48">
        <Search className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          className="pl-8"
          placeholder={t('list.search')}
          value={name}
          onChange={e => onNameChange(e.target.value)}
        />
      </div>
      <Select value={sort} onValueChange={onSortChange}>
        <SelectTrigger size="sm" aria-label={t('list.sort')}>
          <SelectValue placeholder={t('list.sort')} />
        </SelectTrigger>
        <SelectContent>
          {sortOptions.map(opt => (
            <SelectItem key={opt.value} value={opt.value}>
              {opt.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <Button
        variant="outline"
        size="sm"
        aria-pressed={order === 'asc'}
        aria-label={t('list.order')}
        onClick={() => onOrderChange(order === 'asc' ? 'desc' : 'asc')}
      >
        {order === 'asc' ? <ArrowUpAZ /> : <ArrowDownAZ />}
        {order === 'asc' ? t('list.asc') : t('list.desc')}
      </Button>
      {name !== '' && (
        <Button variant="ghost" size="sm" onClick={() => onNameChange('')}>
          <X />
          {t('list.reset')}
        </Button>
      )}
      {children}
    </div>
  )
}
