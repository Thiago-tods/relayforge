/** The path the user was trying to open before being sent to sign-in, if it is a safe same-origin app path. */
export function returnPath(state: unknown): string | null {
  if (typeof state === 'object' && state !== null && 'from' in state) {
    const from = (state as { from: unknown }).from
    // Only paths inside this app are honored, so crafted navigation state cannot send the user elsewhere.
    if (typeof from === 'string' && from.startsWith('/') && !from.startsWith('//')) return from
  }
  return null
}

/** Where a user goes right after signing in: back to where they were, else straight into their only organization. */
export function landingPath(state: unknown, organizations: readonly { id: string }[]): string {
  const back = returnPath(state)
  if (back !== null) return back
  const only = organizations.length === 1 ? organizations[0] : undefined
  return only ? `/o/${only.id}` : '/orgs'
}
