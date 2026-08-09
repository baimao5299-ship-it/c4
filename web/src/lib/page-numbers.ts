// 页码按钮组计算（标准滑动窗口）：totalPages ≤ 7 全显；否则首尾页 + 当前页前后各 2 页，
// 缺口以 'ellipsis' 占位（渲染为「…」）。current 靠近首尾时窗口自然收敛。
// 供 Pagination（offset/limit）与 PagePagination（page/page_size）共用。
export function pageNumbers(current: number, totalPages: number): (number | 'ellipsis')[] {
  if (totalPages <= 7) return Array.from({ length: totalPages }, (_, i) => i + 1)
  const pages = new Set<number>([1, totalPages])
  for (let p = current - 2; p <= current + 2; p++) {
    if (p >= 1 && p <= totalPages) pages.add(p)
  }
  const out: (number | 'ellipsis')[] = []
  let prev = 0
  for (const p of [...pages].sort((a, b) => a - b)) {
    if (p - prev > 1) out.push('ellipsis')
    out.push(p)
    prev = p
  }
  return out
}
