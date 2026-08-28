// Keep model choices predictable across providers. Explicit release dates and
// numeric version segments take precedence; unknown names retain their input
// order as the provider's catalogue is the only reliable signal for them.
export function sortModelsLatestFirst(models: readonly string[]): string[] {
  const indexed = models
    .map((raw, index) => ({ name: raw.trim(), index }))
    .filter(item => item.name.length > 0)

  const dateScore = (name: string): number => {
    const dashed = name.match(/(?:^|[-_])((?:20|19)\d{2})[-_](\d{2})[-_](\d{2})(?:$|[-_])/)
    if (dashed) return Number(`${dashed[1]}${dashed[2]}${dashed[3]}`)
    const compact = name.match(/(?:^|[-_])((?:20|19)\d{6})(?:$|[-_])/)
    return compact ? Number(compact[1]) : 0
  }

  const versionScore = (name: string): number[] => {
    const values = [...name.matchAll(/(?:^|[-_])([0-9]+(?:\.[0-9]+)*)/g)]
      .flatMap(match => match[1].split('.').map(Number))
    return values.slice(0, 4)
  }

  const compareVersions = (left: number[], right: number[]): number => {
    for (let i = 0; i < Math.max(left.length, right.length); i += 1) {
      const delta = (right[i] ?? 0) - (left[i] ?? 0)
      if (delta !== 0) return delta
    }
    return 0
  }

  return indexed
    .sort((left, right) => {
      const dateDelta = dateScore(right.name) - dateScore(left.name)
      if (dateDelta !== 0) return dateDelta
      const versionDelta = compareVersions(versionScore(left.name), versionScore(right.name))
      if (versionDelta !== 0) return versionDelta
      const previewDelta = Number(right.name.includes('preview')) - Number(left.name.includes('preview'))
      if (previewDelta !== 0) return previewDelta
      return left.index - right.index
    })
    .map(item => item.name)
}
