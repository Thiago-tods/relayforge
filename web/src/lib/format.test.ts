import { describe, expect, it } from 'vitest'
import { COUNT_CAP, formatAge, formatCount, formatPercent, formatUtc, isCapped } from './format'

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

describe('formatPercent', () => {
  it('shows an ordinary rate with one decimal', () => {
    expect(formatPercent(1 / 6)).toBe('16.7%')
    expect(formatPercent(0.5)).toBe('50.0%')
    expect(formatPercent(0.001)).toBe('0.1%')
  })

  it('shows zero and a total rate exactly', () => {
    expect(formatPercent(0)).toBe('0.0%')
    expect(formatPercent(1)).toBe('100.0%')
  })

  it('never rounds a rate that is not zero down to 0.0%', () => {
    // 1 failed in 99,999 succeeded is about 0.001%.
    expect(formatPercent(1 / 100_000)).toBe('< 0.1%')
    expect(formatPercent(0.00049)).toBe('< 0.1%')
  })

  it('never rounds a rate that is not total up to 100.0%', () => {
    expect(formatPercent(0.99999)).toBe('> 99.9%')
    expect(formatPercent(0.9996)).toBe('> 99.9%')
    expect(formatPercent(0.9994)).toBe('99.9%')
  })

  it('shows a dash for a value that is not a rate', () => {
    expect(formatPercent(Number.NaN)).toBe('—')
    expect(formatPercent(-0.1)).toBe('—')
    expect(formatPercent(1.5)).toBe('—')
  })
})
