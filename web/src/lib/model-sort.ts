// Catalogue names are provider-controlled identifiers. We only collapse
// unambiguous release snapshots and the well-known Claude families; capability
// suffixes such as mini/pro/vision/thinking remain separate models. The actual
// identifier is always returned unchanged, so requests still use the provider's
// exact model ID.
const claudeFamilies = new Set(['opus', 'sonnet', 'haiku', 'instant'])

function releaseDate(name: string): number {
  const dashed = name.match(/(?:^|[-_.])((?:20|19)\d{2})[-_.](\d{2})[-_.](\d{2})(?:$|[-_.])/)
  if (dashed) return Number(`${dashed[1]}${dashed[2]}${dashed[3]}`)
  const compact = name.match(/(?:^|[-_.])((?:20|19)\d{6})(?:$|[-_.])/)
  if (compact) return Number(compact[1])
  // Gemini-style preview snapshots use MM-DD without repeating the year.
  // Keep them in a comparable bucket so the newest snapshot still wins.
  const preview = name.match(/(?:^|[-_.])(?:preview|exp)[-_.](\d{2})[-_.](\d{2})(?:$|[-_.])/i)
  return preview ? 20_000_000 + Number(preview[1]) * 100 + Number(preview[2]) : 0
}

function withoutReleaseDate(name: string): string {
  return name
    .replace(/[-_.]?(?:20|19)\d{2}[-_.]\d{2}[-_.]\d{2}(?=$|[-_.])/g, '')
    .replace(/[-_.]?(?:20|19)\d{6}(?=$|[-_.])/g, '')
    // Preserve the channel name while removing its dated snapshot suffix.
    .replace(/([-_.])((?:preview|exp))[-_.]\d{2}[-_.]\d{2}(?=$|[-_.])/gi, '$1$2')
    .replace(/[-_.](?:latest|stable)$/i, '')
    .replace(/[-_.]+/g, '-')
    .replace(/^-|-$/g, '')
}

function familyKey(name: string): string {
  const normalized = withoutReleaseDate(name.trim().toLowerCase())
  const parts = normalized.split(/[-_]+/).filter(Boolean)
  if (parts[0] !== 'claude') return normalized

  const familyIndex = parts.findIndex((part, index) => index > 0 && claudeFamilies.has(part))
  if (familyIndex < 0) return normalized
  const variant = parts.slice(familyIndex + 1).filter(part => {
    if (/^\d+(?:\.\d+)?$/.test(part)) return false
    return part !== 'latest' && part !== 'stable'
  })
  return ['claude', parts[familyIndex], ...variant].join('-')
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
    .map((raw, index) => ({ name: raw.trim(), index, family: familyKey(raw) }))
    .filter(item => item.name.length > 0)

  const compareVersions = (left: number[], right: number[]): number => {
    for (let i = 0; i < Math.max(left.length, right.length); i += 1) {
      const delta = (right[i] ?? 0) - (left[i] ?? 0)
      if (delta !== 0) return delta
    }
    return 0
  }

  indexed.sort((left, right) => {
    const dateDelta = releaseDate(right.name) - releaseDate(left.name)
    if (dateDelta !== 0) return dateDelta
    const versionDelta = compareVersions(versionScore(left.name), versionScore(right.name))
    if (versionDelta !== 0) return versionDelta
    const previewDelta = Number(right.name.includes('preview')) - Number(left.name.includes('preview'))
    if (previewDelta !== 0) return previewDelta
    return left.index - right.index
  })
  const latest = new Set<string>()
  return indexed
    .filter(item => {
      if (latest.has(item.family)) return false
      latest.add(item.family)
      return true
    })
    .map(item => item.name)
}
