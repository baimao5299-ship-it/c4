import { useTranslation } from 'react-i18next'
import { ChevronLeft, ChevronRight } from 'lucide-react'
import { Button } from '@/components/ui/button'

// 增强分页范式（page/page_size，1-based）分页条：与 Pagination（offset/limit）
// 并存，供以 page/page_size 为查询参数的列表页使用（redemption-codes / pricing）。
// 视觉与 Pagination 一致：左侧页码信息，右侧 outline 分页按钮。
export function PagePagination({
  total,
  pageSize,
  page,
  onPageChange,
}: {
  total: number
  pageSize: number
  page: number // 1-based
  onPageChange: (page: number) => void
}) {
  const { t } = useTranslation()
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  return (
    <div className="flex items-center justify-between px-2 py-2 text-sm text-muted-foreground">
      <span className="tabular-nums">
        {t('list.pageInfo', { page, totalPages, total })}
      </span>
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
