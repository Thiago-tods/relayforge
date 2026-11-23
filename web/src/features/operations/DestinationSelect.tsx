import { useEffect, useRef, useState } from 'react'
import { listDestinations } from '../../api/destinations'
import { isAbortError, userMessage } from '../../api/errors'
import type { DestinationOption } from '../../api/operationsTypes'
import { Button } from '../../ui/Button'
import { Select } from '../../ui/Select'

const PAGE_SIZE = 100

interface Loaded {
  options: DestinationOption[]
  /** The offset of the next page. It counts rows fetched, not rows kept after merging. */
  nextOffset: number
  total: number
}

function label(option: DestinationOption): string {
  return `${option.name} — ${option.status} (${option.id.slice(0, 8)})`
}

/**
 * A destination filter. Destinations are fetched only when the user asks, one page of 100 at a time, with the
 * server's exact total shown. Filtering is done by the server through `destinationId`: this list only names the
 * destinations, it never filters anything. Merging by id avoids duplicates but does not make the enumeration a
 * snapshot: destinations created or changed while paging can still be missed or shown out of order.
 */
export function DestinationSelect({
  orgId,
  value,
  onChange,
}: {
  orgId: string
  value: string
  onChange: (id: string) => void
}) {
  const [loaded, setLoaded] = useState<Loaded | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const pending = useRef<AbortController | null>(null)

  // A request still running when the field goes away (or the organization changes) is cancelled.
  useEffect(() => () => pending.current?.abort(), [])

  async function load(offset: number) {
    pending.current?.abort()
    const controller = new AbortController()
    pending.current = controller
    setBusy(true)
    setError(null)
    try {
      const page = await listDestinations(orgId, { limit: PAGE_SIZE, offset }, controller.signal)
      if (controller.signal.aborted) return
      setLoaded((previous) => {
        const known = new Map((previous?.options ?? []).map((option) => [option.id, option]))
        for (const option of page.items) known.set(option.id, option)
        return {
          options: [...known.values()],
          // An empty page ends the enumeration even if the total says otherwise (rows removed meanwhile).
          nextOffset: offset + page.items.length,
          total: page.items.length === 0 ? offset : page.total,
        }
      })
    } catch (caught) {
      if (controller.signal.aborted || isAbortError(caught)) return
      setError(caught)
    } finally {
      if (!controller.signal.aborted) setBusy(false)
    }
  }

  const options = loaded?.options ?? []
  const known = options.some((option) => option.id === value)
  const more = loaded !== null && loaded.nextOffset < loaded.total

  return (
    <div className="destination-select">
      <Select
        label="Destination"
        value={value}
        onChange={(event) => onChange(event.target.value)}
        hint="Destinations load on request, 100 at a time. The list can change while you load it."
      >
        <option value="">All destinations</option>
        {value !== '' && !known ? (
          <option value={value}>{`Destination ${value} (not loaded)`}</option>
        ) : null}
        {options.map((option) => (
          <option key={option.id} value={option.id}>
            {label(option)}
          </option>
        ))}
      </Select>
      <div className="destination-actions">
        {loaded === null ? (
          <Button busy={busy} onClick={() => void load(0)}>
            {busy ? 'Loading destinations…' : 'Load destinations'}
          </Button>
        ) : more ? (
          <Button busy={busy} onClick={() => void load(loaded.nextOffset)}>
            {busy ? 'Loading destinations…' : `Load more (${loaded.nextOffset} of ${loaded.total})`}
          </Button>
        ) : (
          <span className="muted">
            {loaded.total === 0
              ? 'This organization has no destinations.'
              : `All ${loaded.total} destinations are loaded.`}
          </span>
        )}
      </div>
      {error !== null ? (
        <p className="field-error" role="alert">
          {userMessage(error)}
        </p>
      ) : null}
    </div>
  )
}
