/// <reference types="node" />
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// jsdom does not lay anything out, so it cannot measure the top bar. This test guards the specific rules that keep
// the organization switcher, theme switch and Sign out inside a 375 px screen (they were pushed off it before).
// The layout itself is checked in a real browser.

const css = readFileSync(resolve(process.cwd(), 'src/index.css'), 'utf8')

/** The text between the braces of the first block that starts with `opener`, with nested braces balanced. */
function block(opener: string): string {
  const start = css.indexOf(opener)
  if (start < 0) throw new Error(`${opener} not found in index.css`)
  let depth = 0
  for (let i = css.indexOf('{', start); i < css.length; i++) {
    if (css[i] === '{') depth += 1
    if (css[i] === '}') depth -= 1
    if (depth === 0) return css.slice(css.indexOf('{', start) + 1, i)
  }
  throw new Error(`${opener} is not closed`)
}

/** The declarations of the rule with exactly this selector inside `scope`. */
function rule(scope: string, selector: string): string {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const found = new RegExp(`(?:^|\\n)\\s*${escaped}\\s*\\{([^}]*)\\}`).exec(scope)
  if (!found) throw new Error(`no rule for ${selector}`)
  return found[1] ?? ''
}

describe('top bar on narrow screens', () => {
  const narrow = block('@media (max-width: 47.99rem)')

  it('lets the organization switcher shrink so the bar stays on one row', () => {
    expect(rule(narrow, '.org-switch')).toMatch(/flex:\s*1 1 0/)
    expect(rule(narrow, '.org-switch')).toMatch(/min-width:\s*0/)
    expect(rule(narrow, '.org-switch select')).toMatch(/width:\s*100%/)
    expect(rule(narrow, '.org-switch select')).toMatch(/min-width:\s*0/)
  })

  it('does not reserve a spacer or wide padding that would push the buttons off screen', () => {
    expect(rule(narrow, '.topbar-spacer')).toMatch(/display:\s*none/)
    expect(rule(narrow, '.topbar')).toMatch(/padding-inline:\s*0\.5rem/)
  })

  it('keeps the brand name for assistive technology only on the narrowest screens', () => {
    const narrowest = block('@media (max-width: 30rem)')
    const brand = rule(narrowest, '.topbar .brand')
    expect(brand).toMatch(/position:\s*absolute/)
    expect(brand).toMatch(/clip-path/)
    expect(brand).not.toMatch(/display:\s*none/)
  })
})
