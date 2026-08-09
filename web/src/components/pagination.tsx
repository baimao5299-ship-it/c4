import { useTranslation } from 'react-i18next'
import { ChevronLeft, ChevronRight } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'

// 每页条数选项（数字字面量，无需翻译键）。
const PAGE_SIZES = [10, 20, 50, 100, 1000]

// 列表页分页条：offset/limit 受控，上一页/下一页 + 页码信息。
// 独立于表格卡片的分页层：左侧页码信息 + 每页条数下拉，右侧 outline 分页按钮（带方向图标与 disabled 态）。
export function Pagination({
  total,
  limit,
  offset,
  onOffsetChange,
  onLimitChange,
}: {
  total: number
  limit: number
  offset: number
  onOffsetChange: (offset: number) => void
  onLimitChange: (limit: number) => void
}) {
  const { t } = useTranslation()
  const page = Math.floor(offset / limit) + 1
  const totalPages = Math.max(1, Math.ceil(total / limit))
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 px-2 py-2 text-sm text-muted-foreground">
      <div className="flex items-center gap-2">
        <span className="tabular-nums">
          {t('list.pageInfo', { page, totalPages, total })}
        </span>
        <Select
          items={Object.fromEntries(PAGE_SIZES.map(n => [String(n), String(n)]))}
          value={String(limit)}
          onValueChange={v => onLimitChange(Number(v))}
        >
          <SelectTrigger size="sm" aria-label={`${t('list.pageSize')}: ${limit}`}>
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
          disabled={offset === 0}
          onClick={() => onOffsetChange(Math.max(0, offset - limit))}
        >
          <ChevronLeft />
          {t('list.prev')}
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={offset + limit >= total}
          onClick={() => onOffsetChange(offset + limit)}
        >
          {t('list.next')}
          <ChevronRight />
        </Button>
      </div>
    </div>
  )
}
