/// <reference types="node" />
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// jsdom does not lay anything out, so it cannot measure horizontal overflow. This test guards the specific rules that
// keep the Activity page inside a 320 px screen: wide tables scroll inside their own box (which a keyboard can reach),
// and the filter fields shrink. The layout itself is checked in a real browser.

const css = readFileSync(resolve(process.cwd(), 'src/index.css'), 'utf8')

function rule(selector: string): string {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const found = new RegExp(`(?:^|\\n)${escaped}\\s*\\{([^}]*)\\}`).exec(css)
  if (!found) throw new Error(`no rule for ${selector}`)
  return found[1] ?? ''
}

describe('activity page layout rules', () => {
  it('scrolls wide tables inside their own box and shows where keyboard focus is', () => {
    expect(rule('.table-scroll')).toMatch(/overflow-x:\s*auto/)
    expect(rule('.table-scroll')).toMatch(/max-width:\s*100%/)
    expect(rule('.table-scroll:focus-visible')).toMatch(/outline/)
  })

  it('lets filter fields shrink instead of pushing the page wider', () => {
    expect(rule('.filter-fields')).toMatch(/minmax\(min\(100%,\s*14rem\),\s*1fr\)/)
    expect(rule('.filter-fields .field input,\n.filter-fields .field select')).toMatch(
      /min-width:\s*0/,
    )
  })

  it('keeps table cells on one line, so the scroll box (not the page) takes the extra width', () => {
    expect(rule('.data-table th,\n.data-table td')).toMatch(/white-space:\s*nowrap/)
  })
})
