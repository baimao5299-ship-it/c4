import { useTranslation } from 'react-i18next'
import { ChevronLeft, ChevronRight } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'

// 每页条数选项（数字字面量，无需翻译键）。
const PAGE_SIZES = [10, 20, 50, 100, 1000]

// 增强分页范式（page/page_size，1-based）分页条：与 Pagination（offset/limit）
// 并存，供以 page/page_size 为查询参数的列表页使用（redemption-codes / pricing）。
// 视觉与 Pagination 一致：左侧页码信息 + 每页条数下拉，右侧 outline 分页按钮。
export function PagePagination({
  total,
  pageSize,
  page,
  onPageChange,
  onPageSizeChange,
}: {
  total: number
  pageSize: number
  page: number // 1-based
  onPageChange: (page: number) => void
  onPageSizeChange: (size: number) => void
}) {
  const { t } = useTranslation()
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 px-2 py-2 text-sm text-muted-foreground">
      <div className="flex items-center gap-2">
        <span className="tabular-nums">
          {t('list.pageInfo', { page, totalPages, total })}
        </span>
        <Select
          items={Object.fromEntries(PAGE_SIZES.map(n => [String(n), String(n)]))}
          value={String(pageSize)}
          onValueChange={v => onPageSizeChange(Number(v))}
        >
          <SelectTrigger size="sm" aria-label={`${t('list.pageSize')}: ${pageSize}`}>
            <span className="text-xs">{t('list.pageSize')}</span>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {PAGE_SIZES.map(n => (
              <SelectItem key={n} value={String(n)} label={String(n)}>{n}</SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <div className="flex items-center gap-2">
        <Button
          variant="outline"
          size="sm"
          disabled={page <= 1}
          onClick={() => onPageChange(page - 1)}
        >
          <ChevronLeft />
          {t('list.prev')}
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={page >= totalPages}
          onClick={() => onPageChange(page + 1)}
        >
          {t('list.next')}
          <ChevronRight />
        </Button>
      </div>
    </div>
  )
}
