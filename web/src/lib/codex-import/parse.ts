import { unzipSync } from 'fflate'

export const IMPORT_DOCUMENT_SEPARATOR = '\u001e'

type ParseFailure = { __import_error: string }

function failure(message: string): ParseFailure {
  return { __import_error: message }
}

function appendValue(out: unknown[], value: unknown) {
  if (Array.isArray(value)) {
    value.forEach(item => appendValue(out, item))
    return
  }
  out.push(value)
}

function skipWhitespace(text: string, start: number) {
  let index = start
  while (index < text.length && /\s/.test(text[index])) index++
  // A UTF-8 BOM is valid at the beginning of every independently exported
  // file. Multi-file imports concatenate documents, so each document must
  // consume its own BOM before JSON boundary detection.
  if (text[index] === '\uFEFF') index++
  return index
}

function jsonBoundary(text: string, start: number): number | undefined {
  const opening = text[start]
  if (opening === '"') {
    let escaped = false
    for (let index = start + 1; index < text.length; index++) {
      const char = text[index]
      if (escaped) { escaped = false; continue }
      if (char === '\\') { escaped = true; continue }
      if (char === '"') return index + 1
    }
    return undefined
  }
  if (opening !== '{' && opening !== '[') return undefined

  const stack: string[] = [opening === '{' ? '}' : ']']
  let inString = false
  let escaped = false
  for (let index = start + 1; index < text.length; index++) {
    const char = text[index]
    if (inString) {
      if (escaped) escaped = false
      else if (char === '\\') escaped = true
      else if (char === '"') inString = false
      continue
    }
    if (char === '"') { inString = true; continue }
    if (char === '{') stack.push('}')
    else if (char === '[') stack.push(']')
    else if (char === '}' || char === ']') {
      if (stack.pop() !== char) return undefined
      if (stack.length === 0) return index + 1
    }
  }
  return undefined
}

function parseDocument(document: string, out: unknown[]) {
  const fencePattern = /^```(?:jsonl?|ndjson)?\s*$|^```\s*$/i
  const markdownHeading = /^(?:#{1,6}\s+|(?:-\s*){3,}|(?:\*\s*){3,})/
  let cursor = 0
  while (cursor < document.length) {
    cursor = skipWhitespace(document, cursor)
    if (cursor >= document.length) break
    const lineEnd = document.indexOf('\n', cursor)
    const rawLine = document.slice(cursor, lineEnd < 0 ? document.length : lineEnd).trim()
    const lineText = rawLine
    if (fencePattern.test(lineText)) {
      cursor = lineEnd < 0 ? document.length : lineEnd + 1
      continue
    }
    // Markdown exports often include a heading or list around the fenced
    // JSON. These lines are presentation text, not credentials; treating
    // them as rows creates a false invalid item and blocks the whole import.
    if (markdownHeading.test(lineText)) {
      cursor = lineEnd < 0 ? document.length : lineEnd + 1
      continue
    }
    const listValue = lineText.replace(/^(?:[-*+]\s+|>\s+)/, '').trim()
    if (listValue !== lineText) {
      if (listValue.startsWith('{') || listValue.startsWith('[') || listValue.startsWith('"')) {
        try {
          appendValue(out, JSON.parse(listValue) as unknown)
        } catch {
          out.push(failure('JSON 格式无效'))
        }
      } else if (listValue) {
        out.push(listValue)
      }
      cursor = lineEnd < 0 ? document.length : lineEnd + 1
      continue
    }
    const boundary = jsonBoundary(document, cursor)
    if (boundary != null) {
      const source = document.slice(cursor, boundary)
      try {
        appendValue(out, JSON.parse(source) as unknown)
      } catch {
        out.push(failure('JSON 格式无效'))
      }
      cursor = boundary
      continue
    }

    if (document[cursor] === '{' || document[cursor] === '[' || document[cursor] === '"') {
      out.push(failure('JSON 格式无效'))
      const lineEnd = document.indexOf('\n', cursor)
      cursor = lineEnd < 0 ? document.length : lineEnd + 1
      continue
    }

    const source = document.slice(cursor, lineEnd < 0 ? document.length : lineEnd).trim()
    if (source) out.push(source)
    cursor = lineEnd < 0 ? document.length : lineEnd + 1
  }
}

export function parseRawText(rawText: string): { rows: unknown[]; error?: string } {
  const text = rawText.replace(/^\uFEFF/, '').trim()
  if (!text) return { rows: [], error: 'empty' }
  const rows: unknown[] = []
  for (const document of text.split(IMPORT_DOCUMENT_SEPARATOR)) {
    const trimmed = document.replace(/^\uFEFF/, '').trim()
    if (trimmed) parseDocument(trimmed, rows)
  }
  return rows.length ? { rows } : { rows: [], error: 'empty' }
}

const MAX_FILE_BYTES = 5 * 1024 * 1024
const MAX_ARCHIVE_BYTES = 20 * 1024 * 1024
const MAX_ARCHIVE_ENTRIES = 512

function isImportTextName(name: string): boolean {
  return /\.(json|jsonl|ndjson|txt|at_txt)$/i.test(name)
}

/** File extensions accepted by the import dialog. Directory picks often
 * contain editor metadata or unrelated assets; those must not be parsed as
 * credential rows. */
export function isImportFileName(name: string): boolean {
  return isImportTextName(name) || name.toLowerCase().endsWith('.zip')
}

export async function readImportFile(file: File): Promise<string[]> {
  const isZip = file.name.toLowerCase().endsWith('.zip')
  if (!isImportFileName(file.name)) throw new Error('unsupportedFileType')
  if (file.size > (isZip ? MAX_ARCHIVE_BYTES : MAX_FILE_BYTES)) throw new Error('fileTooLarge')
  if (!isZip) return [await file.text()]

  let entries: Record<string, Uint8Array>
  let archiveEntries = 0
  let expandedBytes = 0
  const archiveLimit = 'zipLimit'
  try {
    entries = unzipSync(new Uint8Array(await file.arrayBuffer()), {
      // fflate exposes central-directory sizes to the filter, so reject an
      // oversized archive before inflating its payload. This keeps a highly
      // compressed binary entry from consuming the browser's heap even when
      // that entry would not be imported.
      filter: entry => {
        archiveEntries += 1
        if (archiveEntries > MAX_ARCHIVE_ENTRIES) throw new Error(archiveLimit)
        if (!Number.isSafeInteger(entry.originalSize) || entry.originalSize < 0 || expandedBytes + entry.originalSize > MAX_ARCHIVE_BYTES) {
          throw new Error(archiveLimit)
        }
        expandedBytes += entry.originalSize
        return !entry.name.endsWith('/') && isImportTextName(entry.name)
      },
    })
  } catch (error) {
    if (error instanceof Error && error.message === archiveLimit) throw new Error('fileTooLarge')
    throw new Error('zipInvalid')
  }
  const supported = Object.entries(entries)
    .filter(([name]) => !name.endsWith('/') && isImportTextName(name))
    .sort(([a], [b]) => a.localeCompare(b))
  if (supported.length === 0) throw new Error('zipNoImportFiles')
  const total = supported.reduce((sum, [, data]) => sum + data.length, 0)
  if (total > MAX_ARCHIVE_BYTES) throw new Error('fileTooLarge')
  try {
    const decoder = new TextDecoder('utf-8', { fatal: true })
    return supported.map(([, data]) => decoder.decode(data))
  } catch {
    throw new Error('zipInvalid')
  }
}
