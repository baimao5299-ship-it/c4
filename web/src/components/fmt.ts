// 展示格式化工具（页面共享，放 components/ 以便与提交范围一致）。
// 后端 err_rate 为比率（0~1），按 brief 以百分比展示。
export function formatPercent(v?: number): string {
  return v == null ? '—' : `${(v * 100).toFixed(1)}%`
}

export function formatDateTime(iso?: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString('zh-CN', { hour12: false })
}

// 超长文本截断（title/末尾省略），适合表格单元格。
export function truncate(s: string | undefined | null, n = 16): string {
  if (!s) return '—'
  return s.length > n ? `${s.slice(0, n)}…` : s
}

// 逗号列表截断展示，完整内容放 title。
export function commaList(items: string[] | undefined, max = 3): { text: string; full: string } {
  const full = items?.join(', ') ?? ''
  if (!full) return { text: '—', full: '' }
  const head = items!.slice(0, max).join(', ')
  const text = items!.length > max ? `${head} +${items!.length - max}` : head
  return { text, full }
}
