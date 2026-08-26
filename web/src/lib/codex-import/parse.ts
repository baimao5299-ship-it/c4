import { strFromU8, unzipSync } from 'fflate'

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
  let cursor = 0
  while (cursor < document.length) {
    cursor = skipWhitespace(document, cursor)
    if (cursor >= document.length) break
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

    const lineEnd = document.indexOf('\n', cursor)
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
    const trimmed = document.trim()
    if (trimmed) parseDocument(trimmed, rows)
  }
  return rows.length ? { rows } : { rows: [], error: 'empty' }
}

const MAX_FILE_BYTES = 5 * 1024 * 1024
const MAX_ARCHIVE_BYTES = 20 * 1024 * 1024

export async function readImportFile(file: File): Promise<string[]> {
  if (file.size > MAX_FILE_BYTES) throw new Error('fileTooLarge')
  if (!file.name.toLowerCase().endsWith('.zip')) return [await file.text()]

  let entries: Record<string, Uint8Array>
  try {
    entries = unzipSync(new Uint8Array(await file.arrayBuffer()))
  } catch {
    throw new Error('zipInvalid')
  }
  const supported = Object.entries(entries).filter(([name]) => !name.endsWith('/') && /\.(json|txt|at_txt)$/i.test(name))
  if (supported.length === 0) throw new Error('zipNoImportFiles')
  const total = supported.reduce((sum, [, data]) => sum + data.length, 0)
  if (total > MAX_ARCHIVE_BYTES) throw new Error('fileTooLarge')
  try {
    return supported.map(([, data]) => strFromU8(data))
  } catch {
    throw new Error('zipInvalid')
  }
}
