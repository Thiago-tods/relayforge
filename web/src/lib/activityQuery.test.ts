import { describe, expect, it } from 'vitest'
import { DEST_A, DELIVERY_A } from '../test/fetchStub'
import { normalizedParams, readActivityQuery, sectionEntries, withSection } from './activityQuery'

const RANGE = { from: '2026-11-01T00:00:00Z', to: '2026-11-02T00:00:00Z' }

describe('reading the URL', () => {
  it('has no filters and nothing ignored for an empty query', () => {
    expect(readActivityQuery(new URLSearchParams())).toEqual({
      summary: { range: null },
      failures: { range: null, reason: '', destinationId: '' },
      attempts: { range: null, outcome: '', deliveryId: '' },
      ignored: false,
    })
  })

  it('reads every section from its own prefix', () => {
    const query = readActivityQuery(
      new URLSearchParams({
        sFrom: RANGE.from,
        sTo: RANGE.to,
        fFrom: RANGE.from,
        fTo: RANGE.to,
        fReason: 'UNRECORDED',
        fDest: DEST_A.toUpperCase(),
        aOutcome: 'ABANDONED',
        aDelivery: DELIVERY_A,
      }),
    )
    expect(query.summary.range).toEqual(RANGE)
    expect(query.failures).toEqual({
      range: RANGE,
      reason: 'UNRECORDED',
      destinationId: DEST_A,
    })
    expect(query.attempts).toEqual({ range: null, outcome: 'ABANDONED', deliveryId: DELIVERY_A })
    expect(query.ignored).toBe(false)
  })

  it.each([
    ['one end of a range', { fFrom: RANGE.from }],
    ['a reversed range', { fFrom: RANGE.to, fTo: RANGE.from }],
    ['an unknown reason', { fReason: 'MADE_UP' }],
    ['a lower case reason', { fReason: 'unrecorded' }],
    ['an unknown outcome', { aOutcome: 'MADE_UP' }],
    ['a destination that is not a UUID', { fDest: 'abc' }],
    ['a delivery that is not a UUID', { aDelivery: 'abc' }],
  ])('ignores %s and says so', (_name, params) => {
    const query = readActivityQuery(new URLSearchParams(params))
    expect(query.ignored).toBe(true)
    expect(query.failures.reason).toBe('')
    expect(query.failures.range).toBeNull()
    expect(query.attempts.outcome).toBe('')
  })

  it('keeps a valid part when another part of the same link is not valid', () => {
    const query = readActivityQuery(
      new URLSearchParams({ fReason: 'EGRESS_BLOCKED', aOutcome: 'nope' }),
    )
    expect(query.failures.reason).toBe('EGRESS_BLOCKED')
    expect(query.ignored).toBe(true)
  })

  it('does not read a cursor or a default window from the URL', () => {
    const query = readActivityQuery(new URLSearchParams({ cursor: 'abc', fCursor: 'abc' }))
    expect(query.ignored).toBe(false)
    expect(query.failures).toEqual({ range: null, reason: '', destinationId: '' })
  })
})

describe('writing the URL', () => {
  it('writes only the values that are set', () => {
    expect(sectionEntries('failures', { range: null, reason: '', destinationId: '' })).toEqual([])
    expect(
      sectionEntries('failures', { range: RANGE, reason: 'EGRESS_BLOCKED', destinationId: DEST_A }),
    ).toEqual([
      ['fFrom', RANGE.from],
      ['fTo', RANGE.to],
      ['fReason', 'EGRESS_BLOCKED'],
      ['fDest', DEST_A],
    ])
    expect(sectionEntries('attempts', { range: null, outcome: 'TIMEOUT', deliveryId: '' })).toEqual(
      [['aOutcome', 'TIMEOUT']],
    )
  })

  it('replaces one section and leaves the others and unrelated parameters alone', () => {
    const params = new URLSearchParams({
      sFrom: RANGE.from,
      sTo: RANGE.to,
      fReason: 'UNRECORDED',
      aOutcome: 'TIMEOUT',
      other: 'keep',
    })
    const next = withSection(params, 'failures', {
      range: null,
      reason: 'EGRESS_BLOCKED',
      destinationId: '',
    })
    expect(next.get('fReason')).toBe('EGRESS_BLOCKED')
    expect(next.get('sFrom')).toBe(RANGE.from)
    expect(next.get('aOutcome')).toBe('TIMEOUT')
    expect(next.get('other')).toBe('keep')
    expect(params.get('fReason')).toBe('UNRECORDED')
  })

  it('removes a section completely on reset', () => {
    const params = new URLSearchParams({ aOutcome: 'TIMEOUT', aDelivery: DELIVERY_A })
    const next = withSection(params, 'attempts', { range: null, outcome: '', deliveryId: '' })
    expect(next.toString()).toBe('')
  })

  it('never writes a cursor', () => {
    const next = withSection(new URLSearchParams(), 'failures', {
      range: RANGE,
      reason: 'UNRECORDED',
      destinationId: DEST_A,
    })
    expect([...next.keys()].some((key) => /cursor/i.test(key))).toBe(false)
  })
})

describe('normalizing the URL', () => {
  it('drops every ignored value and keeps the valid ones, in normalized form', () => {
    const params = new URLSearchParams({
      fReason: 'nope',
      fFrom: RANGE.from,
      aOutcome: 'TIMEOUT',
      aDelivery: DELIVERY_A.toUpperCase(),
      sFrom: RANGE.from,
      sTo: RANGE.to,
      other: 'keep',
    })
    const query = readActivityQuery(params)
    expect(query.ignored).toBe(true)
    const next = normalizedParams(params, query)
    expect(next.get('fReason')).toBeNull()
    expect(next.get('fFrom')).toBeNull()
    expect(next.get('aOutcome')).toBe('TIMEOUT')
    expect(next.get('aDelivery')).toBe(DELIVERY_A)
    expect(next.get('sFrom')).toBe(RANGE.from)
    expect(next.get('other')).toBe('keep')
    // What remains reads back with nothing ignored.
    expect(readActivityQuery(next).ignored).toBe(false)
  })

  it('removes a duplicated parameter instead of leaving the extra copies', () => {
    const params = new URLSearchParams('fReason=EGRESS_BLOCKED&fReason=INTERNAL_ERROR')
    const next = normalizedParams(params, readActivityQuery(params))
    expect(next.getAll('fReason')).toEqual(['EGRESS_BLOCKED'])
  })
})
