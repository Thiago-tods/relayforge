import { describe, expect, it } from 'vitest'
import { adviseReason, countCodePoints, REASON_MAX_CODE_POINTS } from './redriveReason'

describe('advisory redrive reason checks', () => {
  it('flags an empty or whitespace-only reason', () => {
    expect(adviseReason('').issue).toBe('empty')
    expect(adviseReason('   ').issue).toBe('empty')
    expect(adviseReason(' \n\t ').issue).toBe('empty')
  })

  it('counts code points, not UTF-16 units', () => {
    expect(countCodePoints('abc')).toBe(3)
    expect(countCodePoints('😀')).toBe(1)
    expect(countCodePoints('a😀b')).toBe(3)
    expect(countCodePoints('\ud800')).toBe(1)
    expect(countCodePoints('')).toBe(0)
  })

  it('accepts exactly 500 code points and flags 501, including astral characters', () => {
    expect(adviseReason('a'.repeat(500)).issue).toBeNull()
    expect(adviseReason('a'.repeat(501)).issue).toBe('too_long')
    expect(adviseReason('😀'.repeat(500)).issue).toBeNull()
    expect('😀'.repeat(500).length).toBe(1000)
    expect(adviseReason('😀'.repeat(501)).issue).toBe('too_long')
    expect(adviseReason('😀'.repeat(500)).count).toBe(REASON_MAX_CODE_POINTS)
  })

  it('counts the trimmed reason', () => {
    expect(adviseReason(`  ${'a'.repeat(500)}  `).issue).toBeNull()
  })

  it('flags C0 controls and DEL inside the reason, including line breaks and tabs', () => {
    for (const bad of ['a\nb', 'a\rb', 'a\tb', 'a\u0000b', 'a\u001fb', 'a\u007fb']) {
      expect(adviseReason(bad).issue, JSON.stringify(bad)).toBe('control')
    }
  })

  it('does not flag C1 controls or Unicode line separators, which the backend accepts', () => {
    expect(adviseReason('a\u0085b').issue).toBeNull()
    expect(adviseReason('a b').issue).toBeNull()
  })

  it('does not flag a trailing line break that trimming removes (the backend trims first)', () => {
    expect(adviseReason('reason\n').issue).toBeNull()
  })

  it('accepts ordinary text', () => {
    expect(adviseReason('Receiver fixed after outage').issue).toBeNull()
  })
})
