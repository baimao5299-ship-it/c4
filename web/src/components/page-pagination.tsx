import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ChevronLeft, ChevronRight } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'

// 每页条数选项（数字字面量，无需翻译键）。
const PAGE_SIZES = [10, 20, 50, 100, 1000]

// 增强分页范式（page/page_size，1-based）分页条：与 Pagination（offset/limit）
// 并存，供以 page/page_size 为查询参数的列表页使用（redemption-codes / pricing）。
// 视觉与 Pagination 一致：左侧页码信息 + 每页条数下拉 + 「跳至第 N 页」（sm 及以上可见），右侧 outline 分页按钮。
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
  const [jumpTo, setJumpTo] = useState('')
  const totalPages = Math.max(1, Math.ceil(total / pageSize))

  // 页码跳转：Enter 或按钮确认。非法输入（空/非数字）回退为当前页；越界 clamp 到 [1, totalPages]。
  const jump = () => {
    const n = Math.floor(Number(jumpTo))
    if (jumpTo.trim() === '' || !Number.isFinite(n)) {
      setJumpTo(String(page))
      return
    }
    onPageChange(Math.min(Math.max(n, 1), totalPages))
    setJumpTo('')
  }

  return (
    <div className="flex flex-wrap items-center justify-between gap-3 px-2 py-2 text-sm text-muted-foreground">
      <div className="flex flex-wrap items-center gap-2">
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
        <div className="hidden items-center gap-1.5 sm:flex">
          <span className="text-xs">{t('list.jumpTo')}</span>
          <Input
            type="number"
            min={1}
            max={totalPages}
            value={jumpTo}
            onChange={e => setJumpTo(e.target.value)}
            onKeyDown={e => {
              if (e.key === 'Enter') {
                e.preventDefault()
                jump()
              }
            }}
            aria-label={t('list.jumpToLabel')}
            className="h-7 w-14 px-1.5 text-xs tabular-nums"
          />
          <span className="text-xs">{t('list.jumpPage')}</span>
          <Button variant="outline" size="sm" className="h-7 px-2 text-xs" onClick={jump}>
            {t('list.jump')}
          </Button>
        </div>
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
