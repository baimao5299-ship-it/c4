import { useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Button } from '@/components/ui/button'

// 复制到剪贴板：优先 Clipboard API（localhost 属安全上下文），失败回退 execCommand。
export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    try {
      const ta = document.createElement('textarea')
      ta.value = text
      ta.style.position = 'fixed'
      ta.style.opacity = '0'
      document.body.appendChild(ta)
      ta.select()
      const ok = document.execCommand('copy')
      document.body.removeChild(ta)
      return ok
    } catch {
      return false
    }
  }
}

// 明文 key 展示框（分组创建 / key 轮换后，仅此一次明文）：高亮 + 复制按钮。
export function KeyBox({ title, value, hint }: { title?: string; value: string; hint?: string }) {
  const { t } = useTranslation()
  const [copied, setCopied] = useState(false)
  return (
    <div className="rounded-lg border border-emerald-300 bg-emerald-50 p-3 dark:border-emerald-800 dark:bg-emerald-950/30">
      {title && <p className="mb-1.5 text-xs font-medium text-emerald-700 dark:text-emerald-300">{title}</p>}
      <div className="flex items-center gap-2">
        <code className="flex-1 break-all font-mono text-sm font-semibold text-emerald-900 select-all dark:text-emerald-100">
          {value}
        </code>
        <Button
          size="sm"
          variant="outline"
          className="shrink-0 border-emerald-300 text-emerald-700 hover:bg-emerald-100 dark:border-emerald-800 dark:text-emerald-300 dark:hover:bg-emerald-900/40"
          onClick={async () => {
            if (await copyText(value)) {
              setCopied(true)
              setTimeout(() => setCopied(false), 2000)
            }
          }}
        >
          {copied ? <Check /> : <Copy />}
          {copied ? t('keybox.copied') : t('keybox.copy')}
        </Button>
      </div>
      {hint && <p className="mt-1.5 text-xs text-emerald-700/70 dark:text-emerald-300/60">{hint}</p>}
    </div>
  )
}
