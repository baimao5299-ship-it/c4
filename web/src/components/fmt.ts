// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

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

// datetime-local → RFC3339（本地时区输入 → UTC ISO），非法/空值返回 undefined。
export function toRFC3339(v: string): string | undefined {
  if (!v) return undefined
  const d = new Date(v)
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString()
}

// 逗号列表截断展示，完整内容放 title。
export function commaList(items: string[] | undefined, max = 3): { text: string; full: string } {
  const full = items?.join(', ') ?? ''
  if (!full) return { text: '—', full: '' }
  const head = items!.slice(0, max).join(', ')
  const text = items!.length > max ? `${head} +${items!.length - max}` : head
  return { text, full }
}

// 金额格式化（毫分）：后端计费以毫分为单位，1 USD = 100,000 毫分。
// 空/0 → null，调用方统一显示 —（未计费路径 Cost 为 0/空，与真实 $0.0000 无法区分，直接省略）。
const MILLI_CENTS_PER_USD = 100_000
function usdText(c?: number | null): string | null {
  if (c == null || c <= 0) return null
  return `$${(c / MILLI_CENTS_PER_USD).toFixed(4)}`
}

// 计费成本：毫分 → USD 字符串，如 $3.2500 / $0.0004；空值或 0 显示 —。
export function formatCost(c?: number | null): string {
  return usdText(c) ?? '—'
}

// 每百万 token 价格：USD/1M tokens 正常值直接展示（API 边界已换算，内部存储毫分），
// 如 3.5 → $3.5000/M；空值显示 —（0 = 免费价，照常展示 $0.0000/M）。
export function formatPricePerMillion(c?: number | null): string {
  return c == null ? '—' : `$${c.toFixed(4)}/M`
}
