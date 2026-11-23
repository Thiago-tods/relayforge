import { describe, expect, it } from 'vitest'
import {
  checkRange,
  parseInputValue,
  parseUrlValue,
  readUrlRange,
  toInputValue,
  attemptWindowFor,
} from './dateRange'

describe('checkRange', () => {
  it('treats two empty values as the default window', () => {
    expect(checkRange('', '')).toEqual({ ok: true, range: null })
  })

  it('reads input values as UTC and writes whole minutes', () => {
    expect(checkRange('2026-11-01T00:00', '2026-11-02T06:30')).toEqual({
      ok: true,
      range: { from: '2026-11-01T00:00:00Z', to: '2026-11-02T06:30:00Z' },
    })
  })

  it('does not depend on the time zone of the browser', () => {
    // The same input gives the same instant whatever the local zone is: the value is built with UTC functions.
    expect(parseInputValue('2026-06-15T12:00')?.toISOString()).toBe('2026-06-15T12:00:00.000Z')
  })

  it('asks for both dates when only one is given', () => {
    expect(checkRange('2026-11-01T00:00', '')).toMatchObject({
      ok: false,
      errors: { to: expect.any(String) },
    })
    expect(checkRange('', '2026-11-01T00:00')).toMatchObject({
      ok: false,
      errors: { from: expect.any(String) },
    })
  })

  it('refuses a value that is not a date', () => {
    expect(checkRange('2026-02-30T00:00', '2026-03-02T00:00')).toMatchObject({
      ok: false,
      errors: { from: 'Enter a valid UTC date and time.' },
    })
    expect(checkRange('nonsense', '2026-03-02T00:00')).toMatchObject({ ok: false })
  })

  it('requires From to be before To', () => {
    expect(checkRange('2026-11-02T00:00', '2026-11-02T00:00')).toMatchObject({
      ok: false,
      errors: { to: 'To must be after From.' },
    })
    expect(checkRange('2026-11-03T00:00', '2026-11-02T00:00')).toMatchObject({ ok: false })
  })

  it('allows exactly 31 days and refuses one minute more', () => {
    expect(checkRange('2026-10-01T00:00', '2026-11-01T00:00')).toMatchObject({ ok: true })
    expect(checkRange('2026-10-01T00:00', '2026-11-01T00:01')).toMatchObject({
      ok: false,
      errors: { to: 'The window must not exceed 31 days.' },
    })
  })

  it('refuses the year 0000', () => {
    expect(checkRange('0000-01-01T00:00', '0000-01-02T00:00')).toMatchObject({ ok: false })
  })
})

describe('URL values', () => {
  it('accepts whole minutes with or without zero seconds, and normalizes them', () => {
    expect(readUrlRange('2026-11-01T00:00Z', '2026-11-02T00:00:00Z')).toEqual({
      from: '2026-11-01T00:00:00Z',
      to: '2026-11-02T00:00:00Z',
    })
  })

  it.each([
    ['2026-11-01T00:00:30Z', 'seconds that are not zero'],
    ['2026-11-01T00:00:00+02:00', 'an offset'],
    ['2026-11-01', 'a date only'],
    ['2026-13-01T00:00:00Z', 'month 13'],
    ['', 'nothing'],
  ])('rejects %s (%s)', (value) => {
    expect(parseUrlValue(value)).toBeNull()
  })

  it('rejects a reversed or too long range', () => {
    expect(readUrlRange('2026-11-02T00:00:00Z', '2026-11-01T00:00:00Z')).toBeNull()
    expect(readUrlRange('2026-09-01T00:00:00Z', '2026-11-01T00:00:00Z')).toBeNull()
  })

  it('shows a stored value in the input format', () => {
    expect(toInputValue('2026-11-01T08:15:00Z')).toBe('2026-11-01T08:15')
    expect(toInputValue('junk')).toBe('')
  })
})

describe('attemptWindowFor', () => {
  const days = (n: number) => n * 24 * 60 * 60 * 1000

  it('runs from the minute of creation to the next whole minute after the latest completion', () => {
    const window = attemptWindowFor('2026-11-20T10:00:30Z', '2026-11-22T11:00:00.5Z')!
    expect(window).toEqual({
      range: { from: '2026-11-20T10:00:00Z', to: '2026-11-22T11:01:00Z' },
      truncated: false,
    })
    // Every attempt starts after creation and before completion, so every one is inside [from, to).
    expect(Date.parse(window.range.from)).toBeLessThanOrEqual(Date.parse('2026-11-20T10:00:30Z'))
    expect(Date.parse(window.range.to)).toBeGreaterThan(Date.parse('2026-11-22T11:00:00.5Z'))
    expect(readUrlRange(window.range.from, window.range.to)).toEqual(window.range)
  })

  it('keeps a completion that is already a whole minute as the exclusive end', () => {
    expect(attemptWindowFor('2026-11-22T10:00:00Z', '2026-11-22T11:00:00Z')?.range.to).toBe(
      '2026-11-22T11:00:00Z',
    )
  })

  it('allows exactly 31 days without calling it truncated', () => {
    const window = attemptWindowFor('2026-10-22T11:00:00Z', '2026-11-22T11:00:00Z')!
    expect(window.truncated).toBe(false)
    expect(Date.parse(window.range.to) - Date.parse(window.range.from)).toBe(days(31))
  })

  it('cuts a longer lifetime to the last 31 days and says it was cut', () => {
    // Created 82 days before it last failed, for example a delivery redriven weeks after its first failure.
    const window = attemptWindowFor('2026-09-01T08:20:10Z', '2026-11-22T11:00:00Z')!
    expect(window.truncated).toBe(true)
    expect(window.range.to).toBe('2026-11-22T11:00:00Z')
    expect(window.range.from).toBe('2026-10-22T11:00:00Z')
    expect(Date.parse(window.range.to) - Date.parse(window.range.from)).toBe(days(31))
    expect(readUrlRange(window.range.from, window.range.to)).toEqual(window.range)
  })

  it('treats one minute more than 31 days as truncated', () => {
    expect(attemptWindowFor('2026-10-22T10:59:00Z', '2026-11-22T11:00:00Z')?.truncated).toBe(true)
  })

  it('still gives a one-minute window when creation and completion share a minute', () => {
    const window = attemptWindowFor('2026-11-22T11:00:10Z', '2026-11-22T11:00:20Z')!
    expect(window.range).toEqual({ from: '2026-11-22T11:00:00Z', to: '2026-11-22T11:01:00Z' })
    expect(window.truncated).toBe(false)
  })

  it('does not depend on any range of a feed, nor on the clock of the browser', () => {
    const window = attemptWindowFor('2026-11-05T00:00:00Z', '2026-11-21T09:00:00Z')!
    expect(window.range).toEqual({ from: '2026-11-05T00:00:00Z', to: '2026-11-21T09:00:00Z' })
  })

  it('returns null when a value is not a time', () => {
    expect(attemptWindowFor('soon', '2026-11-22T11:00:00Z')).toBeNull()
    expect(attemptWindowFor('2026-11-22T11:00:00Z', 'later')).toBeNull()
  })
})
