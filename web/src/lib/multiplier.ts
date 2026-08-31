// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

/**
 * C4 stores price multipliers as basis points (10,000 = x1) but exposes the
 * normal value as `Multiplier` on management responses. Older deployments may
 * return both fields with the basis-point field still at its default value, so
 * prefer the explicit normal value and use basis points only as a fallback.
 */
const MULTIPLIER_SCALE = 10_000
// Upstream pricing historically permits up to x100; group pricing has a
// narrower x10 validation at its own API boundary.
const MAX_MULTIPLIER = 100

function finiteNumber(value: unknown): number | null {
  const n = typeof value === 'number' ? value : typeof value === 'string' && value.trim() !== '' ? Number(value) : NaN
  return Number.isFinite(n) ? n : null
}

/** Resolve an API multiplier while tolerating old/partially upgraded payloads. */
export function multiplierFromApi(normal: unknown, basisPoints: unknown): number {
  const readable = finiteNumber(normal)
  if (readable != null && readable >= 0 && readable <= MAX_MULTIPLIER) return readable

  const bp = finiteNumber(basisPoints)
  if (bp != null && bp >= 0 && bp <= MAX_MULTIPLIER * MULTIPLIER_SCALE) return bp / MULTIPLIER_SCALE
  return 1
}

/** Format a multiplier without losing small values such as x0.08. */
export function formatMultiplierValue(value: number | null | undefined, maxFractionDigits = 4): string {
  if (value == null || !Number.isFinite(value)) return '—'
  const digits = Math.max(0, Math.min(8, Math.trunc(maxFractionDigits)))
  const normalized = Object.is(value, -0) ? 0 : value
  const text = normalized.toFixed(digits).replace(/\.?0+$/, '')
  return `×${text || '0'}`
}

/** True when a decimal multiplier can be stored without changing its value. */
export function isStorableMultiplier(value: number, max = MAX_MULTIPLIER): boolean {
  if (!Number.isFinite(value) || value < 0 || value > max) return false
  const scaled = value * MULTIPLIER_SCALE
  return Math.abs(scaled - Math.round(scaled)) <= 1e-8
}
