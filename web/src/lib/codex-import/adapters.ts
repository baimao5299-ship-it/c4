import type { CredentialKind, NormalizedRow } from './normalize'
import { markDuplicateRows, normalizeRow } from './normalize'
import { parseRawText } from './parse'
import { parseSub2API } from './sub2api'
import type { components } from '@/lib/api/schema'

export type SourceId = 'cpa' | 'c3api' | 'sub2api' | 'cockpit' | '9router' | 'codex' | 'axonhub' | 'codex-manager'
export type AdapterResult = { rows: NormalizedRow[]; parseError?: string }
export type ImportAdapter = { id: SourceId; credentialKinds: CredentialKind[]; parse: (text: string, kind: CredentialKind) => AdapterResult }

const supported: CredentialKind[] = ['codex-oauth', 'codex-pat']
const cpa: ImportAdapter = { id: 'cpa', credentialKinds: supported, parse: (text, kind) => {
  const parsed = parseRawText(text)
  if (parsed.error) return { rows: [], parseError: parsed.error }
  return { rows: markDuplicateRows(parsed.rows.map((raw, index) => normalizeRow(raw, kind, index))) }
} }
const stub = (id: SourceId): ImportAdapter => ({ id, credentialKinds: supported, parse: () => ({ rows: [], parseError: 'adapterComingSoon' }) })
const sub2api: ImportAdapter = { id: 'sub2api', credentialKinds: supported, parse: (text, kind) => {
  const parsed = parseSub2API(text, kind)
  return { ...parsed, rows: markDuplicateRows(parsed.rows) }
} }
export const adapters: Record<SourceId, ImportAdapter> = { cpa, c3api: stub('c3api'), sub2api, cockpit: stub('cockpit'), '9router': stub('9router'), codex: stub('codex'), axonhub: stub('axonhub'), 'codex-manager': stub('codex-manager') }
export function getAdapter(id: SourceId) { return adapters[id] }
export type ImportItem = components['schemas']['CodexOAuthImportItem'] | components['schemas']['CodexPATImportItem']
