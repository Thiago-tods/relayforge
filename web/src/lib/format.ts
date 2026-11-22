/**
 * The largest value any operational counter reports. A counter that reaches it is saturated: the real number is AT
 * LEAST this, not exactly this. Each counter saturates on its own, so this is decided per count, never from the
 * response-level `capped` flag.
 */
export const COUNT_CAP = 100_000

export function isCapped(count: number): boolean {
  return count >= COUNT_CAP
}

/** A count for display. A saturated count is shown as a lower bound and never as an exact number. */
export function formatCount(count: number): string {
  return isCapped(count) ? `≥ ${COUNT_CAP.toLocaleString('en-US')}` : count.toLocaleString('en-US')
}

/** A UTC instant as `2026-11-22 17:27:03 UTC`. Anything that is not a valid timestamp shows as a dash. */
export function formatUtc(iso: string): string {
  const time = new Date(iso)
  if (Number.isNaN(time.getTime())) return '—'
  return `${time.toISOString().slice(0, 19).replace('T', ' ')} UTC`
}

/** A span in whole seconds, such as `1d 2h 3m 4s`. Zero units are left out, except for a span of zero. */
export function formatAge(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '—'
  const total = Math.floor(seconds)
  const parts: [number, string][] = [
    [Math.floor(total / 86_400), 'd'],
    [Math.floor((total % 86_400) / 3_600), 'h'],
    [Math.floor((total % 3_600) / 60), 'm'],
    [total % 60, 's'],
  ]
  const shown = parts.filter(([value]) => value > 0).map(([value, unit]) => `${value}${unit}`)
  return shown.length > 0 ? shown.join(' ') : '0s'
}

/**
 * A rate between 0 and 1 as a percentage with one decimal. A rate that is not zero is never rounded to "0.0%", and
 * one that is not total is never rounded to "100.0%": the rounding would hide that some deliveries did (or did not)
 * fail.
 */
export function formatPercent(rate: number): string {
  if (!Number.isFinite(rate) || rate < 0 || rate > 1) return '—'
  const percent = rate * 100
  if (percent > 0 && percent < 0.1) return '< 0.1%'
  if (percent < 100 && percent >= 99.95) return '> 99.9%'
  return `${percent.toFixed(1)}%`
}
