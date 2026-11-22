import { describe, expect, it } from 'vitest'
import { COUNT_CAP, formatAge, formatCount, formatUtc, isCapped } from './format'

describe('formatCount', () => {
  it('shows counts below the cap exactly', () => {
    expect(formatCount(0)).toBe('0')
    expect(formatCount(1234)).toBe('1,234')
    expect(formatCount(99_999)).toBe('99,999')
  })

  it('never shows a saturated count as an exact value', () => {
    expect(COUNT_CAP).toBe(100_000)
    expect(formatCount(100_000)).toBe('≥ 100,000')
    expect(formatCount(100_001)).toBe('≥ 100,000')
    expect(isCapped(99_999)).toBe(false)
    expect(isCapped(100_000)).toBe(true)
  })
})

describe('formatAge', () => {
  it.each([
    [0, '0s'],
    [59, '59s'],
    [60, '1m'],
    [3661, '1h 1m 1s'],
    [7503, '2h 5m 3s'],
    [90_061, '1d 1h 1m 1s'],
    [86_400, '1d'],
  ])('formats %d seconds as %s', (seconds, text) => {
    expect(formatAge(seconds)).toBe(text)
  })

  it('shows a dash for a value that is not a span', () => {
    expect(formatAge(-1)).toBe('—')
    expect(formatAge(Number.NaN)).toBe('—')
  })
})

describe('formatUtc', () => {
  it('shows an instant in UTC, whatever the offset of the input', () => {
    expect(formatUtc('2026-11-22T17:27:03Z')).toBe('2026-11-22 17:27:03 UTC')
    expect(formatUtc('2026-11-22T19:27:03.123456+02:00')).toBe('2026-11-22 17:27:03 UTC')
  })

  it('shows a dash for text that is not a timestamp', () => {
    expect(formatUtc('not a time')).toBe('—')
  })
})
