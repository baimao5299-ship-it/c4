// Catalogue names are provider-controlled identifiers. Keep distinct model
// versions and capabilities visible; only an explicit release/snapshot alias
// is eligible for latest-only collapsing. The provider's original spelling is
// always returned unchanged so a displayed model can be sent back verbatim.

type SnapshotInfo = {
  family: string
  date: number
  snapshot: boolean
}

// Date forms used by common OpenAI-compatible catalogues. Matching is limited
// to a trailing token: a numeric version in the middle of an identifier must
// never become a deduplication key by accident.
const fullDateAtEnd = /^(.*?)([-_.])((?:19|20)\d{2})[-_.](\d{2})[-_.](\d{2})$/
const compactDateAtEnd = /^(.*?)([-_.])((?:19|20)\d{6})$/
// Some providers use a contemporary YYMMDD suffix (for example
// `doubao-seed-2-0-pro-260215`). Keep the same conservative 20xx-39xx
// window as the backend alias resolver so ordinary numeric model IDs are not
// accidentally collapsed.
const shortCompactDateAtEnd = /^(.*?)([-_.])((?:2[0-9]|3[0-9])\d{4})$/
const previewDateAtEnd = /^(.*?)([-_.](?:preview|exp))[-_.](\d{2})[-_.](\d{2})$/i
const monthDayAtEnd = /^(.*?)([-_.])((?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01]))$/
const latestAliasAtEnd = /^(.*?)[-_.](latest|stable)$/i

function calendarDate(year: number, month: number, day: number): number | null {
  const value = new Date(Date.UTC(year, month - 1, day))
  if (value.getUTCFullYear() !== year || value.getUTCMonth() !== month - 1 || value.getUTCDate() !== day) return null
  return year * 10_000 + month * 100 + day
}

function snapshotInfo(name: string): SnapshotInfo {
  const trimmed = name.trim()
  let match = trimmed.match(fullDateAtEnd)
  if (match) {
    const date = calendarDate(Number(match[3]), Number(match[4]), Number(match[5]))
    if (date != null && match[1].length > 0) {
      return { family: match[1].toLowerCase(), date, snapshot: true }
    }
  }
  match = trimmed.match(compactDateAtEnd)
  if (match) {
    const raw = match[3]
    const date = calendarDate(Number(raw.slice(0, 4)), Number(raw.slice(4, 6)), Number(raw.slice(6, 8)))
    if (date != null && match[1].length > 0) {
      return { family: match[1].toLowerCase(), date, snapshot: true }
    }
  }
  match = trimmed.match(shortCompactDateAtEnd)
  if (match) {
    const raw = match[3]
    const date = calendarDate(Number(`20${raw.slice(0, 2)}`), Number(raw.slice(2, 4)), Number(raw.slice(4, 6)))
    if (date != null && match[1].length > 0) {
      return { family: match[1].toLowerCase(), date, snapshot: true }
    }
  }
  match = trimmed.match(previewDateAtEnd)
  if (match) {
    const month = Number(match[3])
    const day = Number(match[4])
    if (calendarDate(2024, month, day) != null && match[1].length > 0) {
      return {
        family: `${match[1]}${match[2]}`.toLowerCase(),
        date: 20_000_000 + month * 100 + day,
        snapshot: true,
      }
    }
  }
  match = trimmed.match(monthDayAtEnd)
  if (match) {
    const raw = match[3]
    const month = Number(raw.slice(0, 2))
    const day = Number(raw.slice(2, 4))
    if (calendarDate(2024, month, day) != null && match[1].length > 0) {
      // A year is not present in this provider convention. Keep it in a
      // separate comparison range so an explicit YYYYMMDD always wins.
      return { family: match[1].toLowerCase(), date: 20_000_000 + Number(raw), snapshot: true }
    }
  }
  match = trimmed.match(latestAliasAtEnd)
  if (match && match[1].length > 0) {
    return {
      family: match[1].toLowerCase(),
      date: 1,
      snapshot: true,
    }
  }
  return { family: trimmed, date: 0, snapshot: false }
}

function withoutReleaseDate(name: string): string {
  return snapshotInfo(name).family
}

function versionScore(name: string): number[] {
  const values = [...withoutReleaseDate(name).matchAll(/(?:^|[-_])([0-9]+(?:\.[0-9]+)*)/g)]
    .flatMap(match => match[1].split('.').map(Number))
  return values.slice(0, 4)
}

// Keep model choices predictable across providers. Explicit release dates and
// numeric version segments take precedence; unknown names retain their input
// order as the provider's catalogue is the only reliable signal for them.
export function sortModelsLatestFirst(models: readonly string[]): string[] {
  const indexed = models
    .map((raw, index) => {
      const name = raw.trim()
      const info = snapshotInfo(name)
      return { name, index, ...info }
    })
    .filter(item => item.name.length > 0)

  const compareVersions = (left: number[], right: number[]): number => {
    for (let i = 0; i < Math.max(left.length, right.length); i += 1) {
      const delta = (right[i] ?? 0) - (left[i] ?? 0)
      if (delta !== 0) return delta
    }
    return 0
  }

  indexed.sort((left, right) => {
    const dateDelta = right.date - left.date
    if (dateDelta !== 0) return dateDelta
    const versionDelta = compareVersions(versionScore(left.name), versionScore(right.name))
    if (versionDelta !== 0) return versionDelta
    const previewDelta = Number(right.name.toLowerCase().includes('preview')) - Number(left.name.toLowerCase().includes('preview'))
    if (previewDelta !== 0) return previewDelta
    return left.index - right.index
  })
  const seenExact = new Set<string>()
  const latest = new Set<string>()
  return indexed
    .filter(item => {
      // Whitespace-only differences are not meaningful model IDs. For all
      // other non-snapshot identifiers, do not collapse case or punctuation:
      // those can be provider-specific routes rather than aliases.
      if (seenExact.has(item.name)) return false
      seenExact.add(item.name)
      if (!item.snapshot) return true
      if (latest.has(item.family)) return false
      latest.add(item.family)
      return true
    })
    .map(item => item.name)
}
