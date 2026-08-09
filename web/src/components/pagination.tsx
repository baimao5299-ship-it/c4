import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ChevronLeft, ChevronRight } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'

// 每页条数选项（数字字面量，无需翻译键）。
const PAGE_SIZES = [10, 20, 50, 100, 1000]

// 列表页分页条：offset/limit 受控，上一页/下一页 + 页码信息 + 每页条数下拉 + 页码跳转。
// 独立于表格卡片的分页层：左侧页码信息 + 每页条数下拉 + 「跳至第 N 页」（sm 及以上可见），右侧 outline 分页按钮（带方向图标与 disabled 态）。
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
  const [jumpTo, setJumpTo] = useState('')
  const page = Math.floor(offset / limit) + 1
  const totalPages = Math.max(1, Math.ceil(total / limit))

  // 页码跳转：Enter 或按钮确认。非法输入（空/非数字）回退为当前页；越界 clamp 到 [1, totalPages]。
  const jump = () => {
    const n = Math.floor(Number(jumpTo))
    if (jumpTo.trim() === '' || !Number.isFinite(n)) {
      setJumpTo(String(page))
      return
    }
    onOffsetChange((Math.min(Math.max(n, 1), totalPages) - 1) * limit)
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
