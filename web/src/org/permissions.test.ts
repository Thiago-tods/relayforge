import { describe, expect, it } from 'vitest'
import { can } from './permissions'

describe('can', () => {
  it('lets only OWNER, ADMIN and DEVELOPER redrive a delivery', () => {
    expect(can('OWNER', 'deliveries:redrive')).toBe(true)
    expect(can('ADMIN', 'deliveries:redrive')).toBe(true)
    expect(can('DEVELOPER', 'deliveries:redrive')).toBe(true)
    expect(can('VIEWER', 'deliveries:redrive')).toBe(false)
  })

  it('keeps ownership transfer to the OWNER', () => {
    expect(can('OWNER', 'ownership:transfer')).toBe(true)
    expect(can('ADMIN', 'ownership:transfer')).toBe(false)
  })

  it('keeps secret rotation and revocation away from DEVELOPER', () => {
    expect(can('DEVELOPER', 'destinations:rotate_secret')).toBe(false)
    expect(can('DEVELOPER', 'destinations:revoke')).toBe(false)
    expect(can('ADMIN', 'destinations:rotate_secret')).toBe(true)
  })

  it('hides raw event payloads from VIEWER', () => {
    expect(can('VIEWER', 'events:list')).toBe(true)
    expect(can('VIEWER', 'events:read_payload')).toBe(false)
  })

  it('grants nothing when the role is unknown', () => {
    expect(can(undefined, 'organization:view')).toBe(false)
  })
})
