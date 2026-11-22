/// <reference types="node" />
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { PERMISSIONS, ROLE_PERMISSIONS } from './permissions'
import type { Role } from '../api/types'

// Reads the backend source of truth (internal/tenancy/role.go) and fails when permissions.ts differs from it.
// It understands exactly the shapes role.go uses today: the Perm constants, the `permissions` map literal and the
// loops in init(). If role.go is restructured, the sanity checks below fail loudly so this parser gets updated
// instead of silently passing.

const source = readFileSync(resolve(process.cwd(), '..', 'internal', 'tenancy', 'role.go'), 'utf8')

const ROLES: Record<string, Role> = {
  RoleOwner: 'OWNER',
  RoleAdmin: 'ADMIN',
  RoleDeveloper: 'DEVELOPER',
  RoleViewer: 'VIEWER',
}

function constants(): Map<string, string> {
  const found = new Map<string, string>()
  for (const m of source.matchAll(/(Perm\w+)\s+Permission\s*=\s*"([^"]+)"/g)) {
    found.set(m[1] ?? '', m[2] ?? '')
  }
  return found
}

function names(text: string): string[] {
  return Array.from(text.matchAll(/\b(?:Perm|Role)\w+/g), (m) => m[0])
}

function backendMatrix(consts: Map<string, string>): Map<Role, Set<string>> {
  const matrix = new Map<Role, Set<string>>(Object.values(ROLES).map((r) => [r, new Set<string>()]))
  const grant = (roleName: string, permName: string) => {
    const role = ROLES[roleName]
    const value = consts.get(permName)
    if (!role || value === undefined)
      throw new Error(`role.go grants an unknown pair: ${roleName} ${permName}`)
    matrix.get(role)?.add(value)
  }

  // 1. The `permissions` map literal.
  const literal =
    /var permissions = map\[Role\]map\[Permission\]bool\{([\s\S]*?)\n\}/.exec(source)?.[1] ?? ''
  for (const entry of literal.matchAll(/(Role\w+):\s*\{([\s\S]*?)\}/g)) {
    for (const perm of (entry[2] ?? '').matchAll(/(Perm\w+):\s*true/g))
      grant(entry[1] ?? '', perm[1] ?? '')
  }

  // 2. The lists and loops of init().
  const lists = new Map<string, string[]>()
  const destination =
    /var destinationPermissions = \[\]Permission\{([\s\S]*?)\}/.exec(source)?.[1] ?? ''
  lists.set('destinationPermissions', names(destination))

  const init = /func init\(\) \{([\s\S]*?)\n\}/.exec(source)?.[1] ?? ''
  const loop =
    /for _, (\w+) := range (destinationPermissions|\[\](?:Permission|Role)\{([^}]*)\}) \{([\s\S]*?)\n\t\}/g
  for (const m of init.matchAll(loop)) {
    const variable = m[1] ?? ''
    const items = m[3] !== undefined ? names(m[3]) : (lists.get(m[2] ?? '') ?? [])
    for (const assignment of (m[4] ?? '').matchAll(/permissions\[(\w+)\]\[(\w+)\] = true/g)) {
      const [, first = '', second = ''] = assignment
      for (const item of items) {
        grant(first === variable ? item : first, second === variable ? item : second)
      }
    }
  }
  return matrix
}

describe('permissions.ts against internal/tenancy/role.go', () => {
  const consts = constants()
  const backend = backendMatrix(consts)

  it('parses role.go well enough to be a meaningful comparison', () => {
    expect(consts.size).toBeGreaterThanOrEqual(25)
    expect(backend.get('OWNER')?.size).toBeGreaterThanOrEqual(25)
    expect(backend.get('VIEWER')?.size).toBeGreaterThanOrEqual(5)
  })

  it('lists exactly the permissions the backend defines', () => {
    expect([...PERMISSIONS].sort()).toEqual([...consts.values()].sort())
  })

  for (const role of Object.values(ROLES)) {
    it(`grants ${role} exactly what the backend grants`, () => {
      expect([...ROLE_PERMISSIONS[role]].sort()).toEqual([...(backend.get(role) ?? [])].sort())
    })
  }
})
