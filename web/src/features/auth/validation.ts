// Client-side checks only save a round trip. The backend repeats every rule and its answer is the one that counts.

const SLUG_PATTERN = /^[a-z0-9]+(-[a-z0-9]+)*$/

/** Counts Unicode code points, like the backend counts runes. */
function length(value: string): number {
  return Array.from(value).length
}

export function emailError(email: string): string | undefined {
  const value = email.trim()
  if (value === '') return 'Enter your email address.'
  if (!value.includes('@')) return 'Enter a valid email address.'
  return undefined
}

export function passwordRequired(password: string): string | undefined {
  return password === '' ? 'Enter your password.' : undefined
}

export function newPasswordError(password: string): string | undefined {
  const n = length(password)
  if (n < 12) return 'Use at least 12 characters.'
  if (n > 128) return 'Use at most 128 characters.'
  return undefined
}

export function displayNameError(name: string): string | undefined {
  const n = length(name.trim())
  if (n === 0) return 'Enter your name.'
  if (n > 100) return 'Use at most 100 characters.'
  return undefined
}

/** Blank is allowed: the backend derives the value. */
export function organizationNameError(name: string): string | undefined {
  return length(name.trim()) > 100 ? 'Use at most 100 characters.' : undefined
}

/** Blank is allowed: the backend derives a slug from the name. */
export function slugError(slug: string): string | undefined {
  const value = slug.trim()
  if (value === '') return undefined
  const n = length(value)
  if (n < 3 || n > 48 || !SLUG_PATTERN.test(value)) {
    return 'Use 3 to 48 lowercase letters, digits or single hyphens.'
  }
  return undefined
}
