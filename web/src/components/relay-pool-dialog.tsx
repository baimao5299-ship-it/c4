import { useEffect, useMemo, useState } from 'react'
import { AlertCircle, CheckCircle2, Layers3, LoaderCircle } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { toast } from '@/components/ui/toast'
import type { components } from '@/lib/api/schema'
import { ApiError } from '@/lib/api/client'
import { parseRelayPoolText, type RelayPoolRow } from '@/lib/relay-pool'

type Template = components['schemas']['Template']
type Group = components['schemas']['Group']

interface RelayPoolDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  templates: Template[]
  groups: Group[]
  onDone: () => void
}

type ImportState = 'pending' | 'success' | 'failed'
type ImportResultRow = RelayPoolRow & { state?: ImportState; message?: string }

const errorKey = (code: RelayPoolRow['error']) => (code ? `accounts.relayPool.errors.${code}` : '')

export function RelayPoolDialog({ open, onOpenChange, templates, groups, onDone }: RelayPoolDialogProps) {
  const { t } = useTranslation()
  const relayTemplates = useMemo(
    () => templates.filter(template => template.CredentialType === 'api_key' || template.CredentialType === 'responses-special'),
    [templates],
  )
  const [templateId, setTemplateId] = useState('')
  const [groupId, setGroupId] = useState('')
  const [rawText, setRawText] = useState('')
  const [result, setResult] = useState<ImportResultRow[] | null>(null)
  const [pending, setPending] = useState(false)
  const parsed = useMemo(() => parseRelayPoolText(rawText).rows, [rawText])
  const validRows = parsed.filter(row => !row.error)
  const invalidRows = parsed.filter(row => row.error)
  const retryRows = parsed.filter((row, index) => !row.error && result?.[index]?.state !== 'success')

  useEffect(() => {
    if (!open) {
      setTemplateId('')
      setGroupId('')
      setRawText('')
      setResult(null)
      setPending(false)
    }
  }, [open])

  const submit = async () => {
    if (!templateId || !groupId || retryRows.length === 0 || invalidRows.length > 0 || pending) return
    setPending(true)
    const rows: ImportResultRow[] = parsed.map((row, index) => {
      const previous = result?.[index]
      return { ...row, state: previous?.state, message: previous?.message }
    })
    const template = Number(templateId)
    const group = Number(groupId)
    const pendingRows = rows.filter(row => !row.error && row.state !== 'success')
    let succeeded = 0
    let failed = 0
    for (const row of pendingRows) {
      if (row.error || row.state === 'success') continue
      row.state = 'pending'
      try {
        await api.createAccount({
          name: row.name,
          template_id: template,
          base_url: row.base_url,
          upstream_key: row.upstream_key,
          status: 'active',
          weight: row.weight,
          max_concurrency: row.max_concurrency,
          group_ids: [group],
        })
        row.state = 'success'
        succeeded += 1
      } catch (error) {
        row.state = 'failed'
        row.message = error instanceof ApiError ? `HTTP ${error.status}` : t('accounts.relayPool.failed')
        failed += 1
      }
      setResult([...rows])
    }
    if (succeeded > 0) onDone()
    toast.add({
      title: t('accounts.relayPool.summary', { success: succeeded, failed }),
      type: failed > 0 ? 'error' : 'success',
    })
    setPending(false)
  }

  const close = (next: boolean) => {
    if (!next && pending) return
    onOpenChange(next)
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="flex max-h-[88vh] w-full flex-col gap-0 overflow-hidden p-0 sm:max-w-4xl">
        <DialogHeader className="shrink-0 border-b px-6 pt-6 pb-4">
          <DialogTitle className="flex items-center gap-2 text-xl"><Layers3 className="size-5 text-primary" />{t('accounts.relayPool.title')}</DialogTitle>
          <DialogDescription>{t('accounts.relayPool.desc')}</DialogDescription>
        </DialogHeader>

        <ScrollArea className="min-h-0 flex-1">
          <div className="space-y-5 px-6 py-6">
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-2">
                <Label>{t('accounts.relayPool.templateLabel')}</Label>
                <Select value={templateId || null} onValueChange={value => setTemplateId(String(value ?? ''))}>
                  <SelectTrigger className="h-10 w-full"><SelectValue placeholder={t('accounts.relayPool.templatePlaceholder')} /></SelectTrigger>
                  <SelectContent>
                    {relayTemplates.map(template => <SelectItem key={template.ID} value={String(template.ID)} label={template.Name ?? `#${template.ID}`}>{template.Name ?? `#${template.ID}`}</SelectItem>)}
                  </SelectContent>
                </Select>
                {relayTemplates.length === 0 && <p className="text-xs text-destructive">{t('accounts.relayPool.noTemplates')}</p>}
              </div>
              <div className="space-y-2">
                <Label>{t('accounts.relayPool.groupLabel')}</Label>
                <Select value={groupId || null} onValueChange={value => setGroupId(String(value ?? ''))}>
                  <SelectTrigger className="h-10 w-full"><SelectValue placeholder={t('accounts.relayPool.groupPlaceholder')} /></SelectTrigger>
                  <SelectContent>
                    {groups.map(group => <SelectItem key={group.ID} value={String(group.ID)} label={group.Name ?? `#${group.ID}`}>{group.Name ?? `#${group.ID}`}</SelectItem>)}
                  </SelectContent>
                </Select>
                {groups.length === 0 && <p className="text-xs text-destructive">{t('accounts.relayPool.noGroups')}</p>}
              </div>
            </div>

            <div className="space-y-2">
              <Label>{t('accounts.relayPool.inputLabel')}</Label>
              <textarea
                value={rawText}
                onChange={event => { setRawText(event.target.value); setResult(null) }}
                placeholder={t('accounts.relayPool.inputPlaceholder')}
                rows={9}
                className="min-h-[210px] w-full resize-y rounded-lg border border-input bg-background px-4 py-3 font-mono text-sm leading-relaxed outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/20"
              />
              <p className="text-xs text-muted-foreground">{t('accounts.relayPool.inputHint')}</p>
            </div>

            <div className="flex flex-wrap items-center gap-2 rounded-lg border bg-card px-4 py-3 text-sm">
              <Badge variant="secondary">{t('accounts.relayPool.stats.valid', { count: validRows.length })}</Badge>
              <Badge variant={invalidRows.length > 0 ? 'destructive' : 'secondary'}>{t('accounts.relayPool.stats.invalid', { count: invalidRows.length })}</Badge>
              <span className="text-muted-foreground">{t('accounts.relayPool.stats.limit')}</span>
            </div>

            {parsed.length > 0 && (
              <div className="overflow-hidden rounded-lg border">
                <table className="w-full text-sm">
                  <thead className="bg-muted/40 text-left text-xs text-muted-foreground">
                    <tr>
                      <th className="px-3 py-2">{t('accounts.relayPool.table.line')}</th>
                      <th className="px-3 py-2">{t('accounts.relayPool.table.name')}</th>
                      <th className="px-3 py-2">{t('accounts.relayPool.table.url')}</th>
                      <th className="px-3 py-2">{t('accounts.relayPool.table.weight')}</th>
                      <th className="px-3 py-2">{t('accounts.relayPool.table.status')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {parsed.map((row, index) => {
                      const current: ImportResultRow = result?.[index] ?? row
                      const isSuccess = current.state === 'success'
                      const isFailed = current.state === 'failed' || !!current.error
                      return (
                        <tr key={`${row.line}-${index}`} className="border-t align-top">
                          <td className="px-3 py-2 tabular-nums text-muted-foreground">{row.line}</td>
                          <td className="max-w-36 truncate px-3 py-2" title={row.name}>{row.name}</td>
                          <td className="max-w-64 truncate px-3 py-2 font-mono text-xs" title={row.base_url}>{row.base_url || '—'}</td>
                          <td className="px-3 py-2 tabular-nums">{row.weight}</td>
                          <td className="px-3 py-2 text-xs">
                            {current.state === 'pending' && <span className="inline-flex items-center gap-1 text-muted-foreground"><LoaderCircle className="size-3.5 animate-spin" />{t('accounts.relayPool.pending')}</span>}
                            {isSuccess && <span className="inline-flex items-center gap-1 text-emerald-600"><CheckCircle2 className="size-3.5" />{t('accounts.relayPool.success')}</span>}
                            {isFailed && !isSuccess && <span className="inline-flex items-start gap-1 text-destructive"><AlertCircle className="mt-0.5 size-3.5 shrink-0" /><span>{current.message ?? (current.error ? t(errorKey(current.error)) : t('accounts.relayPool.failed'))}</span></span>}
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
            )}

            {!rawText.trim() && <Alert><AlertDescription>{t('accounts.relayPool.empty')}</AlertDescription></Alert>}
            {invalidRows.length > 0 && <Alert variant="destructive"><AlertDescription>{t('accounts.relayPool.invalidHint')}</AlertDescription></Alert>}
            {result && result.some(row => row.state === 'failed') && <Alert variant="destructive"><AlertDescription>{t('accounts.relayPool.retryHint')}</AlertDescription></Alert>}
          </div>
        </ScrollArea>

        <DialogFooter className="shrink-0 rounded-b-[14px] border-t bg-muted/10 px-6 py-5">
          <Button variant="outline" onClick={() => close(false)} disabled={pending}>{t('common.cancel')}</Button>
          <Button onClick={submit} disabled={pending || retryRows.length === 0 || invalidRows.length > 0 || !templateId || !groupId || relayTemplates.length === 0 || groups.length === 0}>
            {pending ? <><LoaderCircle className="animate-spin" />{t('accounts.relayPool.importing')}</> : t('accounts.relayPool.submit', { count: retryRows.length })}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
