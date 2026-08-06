import { useTranslation } from 'react-i18next'
import { Button } from '@/components/ui/button'

// 列表页分页条：offset/limit 受控，上一页/下一页 + 页码信息。
export function Pagination({
  total,
  limit,
  offset,
  onOffsetChange,
}: {
  total: number
  limit: number
  offset: number
  onOffsetChange: (offset: number) => void
}) {
  const { t } = useTranslation()
  const page = Math.floor(offset / limit) + 1
  const totalPages = Math.max(1, Math.ceil(total / limit))
  return (
    <div className="flex items-center justify-between px-2 py-2 text-sm text-muted-foreground">
      <span>
        {t('list.pageInfo', { page, totalPages, total })}
      </span>
      <div className="flex items-center gap-2">
        <Button
          variant="outline"
          size="sm"
          disabled={offset === 0}
          onClick={() => onOffsetChange(Math.max(0, offset - limit))}
        >
          {t('list.prev')}
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={offset + limit >= total}
          onClick={() => onOffsetChange(offset + limit)}
        >
          {t('list.next')}
        </Button>
      </div>
    </div>
  )
}
