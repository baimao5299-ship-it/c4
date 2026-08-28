import { parseRawText } from './codex-import/parse'

export type RelayPoolErrorCode =
  | 'invalid_format'
  | 'missing_url'
  | 'invalid_url'
  | 'missing_key'
  | 'invalid_weight'
  | 'invalid_concurrency'
  | 'duplicate'
  | 'too_many'

export interface RelayPoolRow {
  line: number
  name: string
  base_url: string
  upstream_key: string
  weight: number
  max_concurrency: number
  error?: RelayPoolErrorCode
}

export interface RelayPoolParseResult {
  rows: RelayPoolRow[]
}

const MAX_ROWS = 100

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function firstString(record: Record<string, unknown>, keys: string[]): string {
  for (const key of keys) {
    const value = record[key]
    if (typeof value === 'string' && value.trim()) return value.trim()
  }
  return ''
}

function firstValue(record: Record<string, unknown>, keys: string[]): unknown {
  for (const key of keys) {
    if (record[key] !== undefined && record[key] !== null && record[key] !== '') return record[key]
  }
  return undefined
}

function parsePositiveInt(value: unknown, fallback: number): number | undefined {
  if (value === undefined || value === null || value === '') return fallback
  const n = typeof value === 'number' ? value : Number(String(value).trim())
  return Number.isInteger(n) && n > 0 ? n : undefined
}

function splitLine(line: string): string[] {
  if (line.includes('|')) return line.split('|').map(part => part.trim())
  if (line.includes('\t')) return line.split('\t').map(part => part.trim())
  return line.split(',').map(part => part.trim())
}

function unwrapJSON(value: unknown): unknown[] {
  if (Array.isArray(value)) return value
  if (!isRecord(value)) return [value]
  for (const key of ['accounts', 'channels', 'items', 'results', 'records', 'providers', 'list']) {
    if (Array.isArray(value[key])) return value[key] as unknown[]
  }
  // Exporters commonly wrap the actual list in either `data: []` or
  // `data: { accounts: [] }`. Keep unwrapping until the list is reached so
  // those formats do not collapse into one invalid object row.
  if (value.data !== undefined) return unwrapJSON(value.data)
  return [value]
}

function normalizeURL(value: string): { value: string; error?: RelayPoolErrorCode } {
  if (!value) return { value: '', error: 'missing_url' }
  try {
    const url = new URL(value)
    if (url.protocol !== 'http:' && url.protocol !== 'https:') return { value, error: 'invalid_url' }
    if (!url.hostname) return { value, error: 'invalid_url' }
    url.protocol = url.protocol.toLowerCase()
    url.hostname = url.hostname.toLowerCase()
    const path = url.pathname.replace(/\/+$/, '')
    if (path === '/v1') return { value, error: 'invalid_url' }
    url.pathname = path || '/'
    const serialized = url.toString()
    return { value: url.pathname === '/' && !url.search && !url.hash ? serialized.slice(0, -1) : serialized }
  } catch {
    return { value, error: 'invalid_url' }
  }
}

function rowFromValue(value: unknown, line: number, index: number): RelayPoolRow {
  const fallbackName = `relay-${String(index + 1).padStart(2, '0')}`
  let name = fallbackName
  let baseURL = ''
  let upstreamKey = ''
  let weightValue: unknown
  let concurrencyValue: unknown

  if (typeof value === 'string') {
    const cleaned = value.trim().replace(/^[-*+]\s+/, '')
    const parts = splitLine(cleaned)
    if (parts.length < 2 && /\s+/.test(cleaned)) {
      const whitespace = cleaned.split(/\s+/)
      parts.splice(0, parts.length, whitespace[0] ?? '', whitespace.slice(1).join(' '))
    }
    if (parts.length < 2) return { line, name, base_url: '', upstream_key: '', weight: 100, max_concurrency: 8, error: 'invalid_format' }
    baseURL = parts[0] ?? ''
    upstreamKey = parts[1] ?? ''
    name = parts[2] || fallbackName
    weightValue = parts[3]
    concurrencyValue = parts[4]
  } else if (isRecord(value)) {
    name = firstString(value, ['name', 'label', 'title']) || fallbackName
    baseURL = firstString(value, ['base_url', 'baseUrl', 'url', 'endpoint', 'api_base'])
    upstreamKey = firstString(value, ['upstream_key', 'upstreamKey', 'api_key', 'apiKey', 'key', 'token', 'secret'])
    weightValue = firstValue(value, ['weight', 'priority_weight'])
    concurrencyValue = firstValue(value, ['max_concurrency', 'maxConcurrency', 'concurrency'])
  } else {
    return { line, name, base_url: '', upstream_key: '', weight: 100, max_concurrency: 8, error: 'invalid_format' }
  }

  const normalizedURL = normalizeURL(baseURL)
  const weight = parsePositiveInt(weightValue, 100)
  const maxConcurrency = parsePositiveInt(concurrencyValue, 8)
  let error = normalizedURL.error
  if (!error && !upstreamKey) error = 'missing_key'
  if (!error && weight === undefined) error = 'invalid_weight'
  if (!error && maxConcurrency === undefined) error = 'invalid_concurrency'

  return {
    line,
    name: name.trim() || fallbackName,
    base_url: normalizedURL.value,
    upstream_key: upstreamKey.trim(),
    weight: weight ?? 100,
    max_concurrency: maxConcurrency ?? 8,
    ...(error ? { error } : {}),
  }
}

export function parseRelayPoolText(input: string): RelayPoolParseResult {
  let raw = input.replace(/^\uFEFF/, '').trim()
  if (!raw) return { rows: [] }

  // Clipboard exports are often wrapped in a fenced code block. Strip only an
  // outer fence so embedded backticks inside a token remain untouched.
  if (raw.startsWith('```') && raw.endsWith('```')) {
    raw = raw.replace(/^```[^\r\n]*\r?\n?/, '').replace(/\r?\n?```$/, '').trim()
  }

  const entries: Array<{ value: unknown; line: number }> = []
  let parsedWhole = false
  if (raw.startsWith('{') || raw.startsWith('[')) {
    try {
      const parsed = JSON.parse(raw) as unknown
      unwrapJSON(parsed).forEach(value => entries.push({ value, line: 1 }))
      parsedWhole = true
    } catch {
      parsedWhole = false
    }
  }

  if (!parsedWhole) {
    // A common export is one pretty-printed JSON object per record. The
    // line-oriented parser below cannot keep an object spanning several lines
    // together, so reuse the shared boundary-aware parser before falling back
    // to ordinary text lines.
    if (raw.includes('{') || raw.includes('[')) {
      const structured = parseRawText(raw)
      if (!structured.error && structured.rows.some(value => isRecord(value) || Array.isArray(value))) {
        structured.rows.flatMap(unwrapJSON).forEach(value => entries.push({ value, line: 1 }))
        parsedWhole = true
      }
    }
  }

  if (!parsedWhole) {
    raw.split(/\r?\n/).forEach((sourceLine, index) => {
      const line = sourceLine.trim().replace(/^\uFEFF/, '')
      if (!line || line.startsWith('#') || line.startsWith('//')) return
      if (line.startsWith('{') || line.startsWith('[')) {
        try {
          unwrapJSON(JSON.parse(line) as unknown).forEach(value => entries.push({ value, line: index + 1 }))
          return
        } catch {
          // Fall through so the malformed line is shown as an invalid row.
        }
      }
      entries.push({ value: line, line: index + 1 })
    })
  }

  const rows = entries.map(({ value, line }, index) => rowFromValue(value, line, index))
  const seen = new Set<string>()
  rows.forEach(row => {
    if (row.error) return
    const identity = `${row.base_url}\n${row.upstream_key}`
    if (seen.has(identity)) row.error = 'duplicate'
    else seen.add(identity)
  })
  rows.forEach((row, index) => {
    if (index >= MAX_ROWS && !row.error) row.error = 'too_many'
  })
  return { rows }
}
