import { useEffect, useRef, useState } from 'react'
import { isAbortError, userMessage } from '../../api/errors'
import { Button } from '../../ui/Button'
import { Select } from '../../ui/Select'

export interface PagedOption {
  id: string
  label: string
}

export interface PagedPage {
  options: PagedOption[]
  /** How many rows the server returned (including rows left out of `options`): the next offset counts rows. */
  fetched: number
  /** The server's exact total. */
  total: number
}

interface Loaded {
  options: PagedOption[]
  nextOffset: number
  total: number
}

interface Props {
  label: string
  /** Plural noun for messages, for example "endpoints". */
  noun: string
  value: string
  onChange: (id: string) => void
  load: (offset: number, signal: AbortSignal) => Promise<PagedPage>
  /** A filter: adds an empty option with this label. Without it the select is a required choice. */
  allLabel?: string
  /** The label of a selected value that has not been loaded yet (for example a deep-linked filter). */
  unloadedLabel?: (id: string) => string
  /** Load the first page when the field appears, instead of waiting for the user. */
  autoLoad?: boolean
  error?: string | undefined
  hint?: string | undefined
  disabled?: boolean
}

/**
 * A select over a paginated server list, loaded one page at a time with "Load more" and the server's exact total.
 * The pages are not a snapshot: rows created or changed while paging can be missed. A request still running when the
 * field goes away is aborted and its answer is ignored. Filtering by the selected value is the server's job; this
 * only names the options.
 */
export function PagedSelect({
  label,
  noun,
  value,
  onChange,
  load,
  allLabel,
  unloadedLabel,
  autoLoad = false,
  error,
  hint,
  disabled = false,
}: Props) {
  const [loaded, setLoaded] = useState<Loaded | null>(null)
  const [busy, setBusy] = useState(autoLoad)
  const [failure, setFailure] = useState<unknown>(null)
  const pending = useRef<AbortController | null>(null)
  const loadRef = useRef(load)
  useEffect(() => {
    loadRef.current = load
  })

  async function fetchPage(offset: number) {
    pending.current?.abort()
    const controller = new AbortController()
    pending.current = controller
    try {
      const page = await loadRef.current(offset, controller.signal)
      if (controller.signal.aborted) return
      setLoaded((previous) => {
        const known = new Map((previous?.options ?? []).map((option) => [option.id, option]))
        for (const option of page.options) known.set(option.id, option)
        return {
          options: [...known.values()],
          // An empty page ends the enumeration even if the total says otherwise (rows removed meanwhile).
          nextOffset: offset + page.fetched,
          total: page.fetched === 0 ? offset : page.total,
        }
      })
    } catch (caught) {
      if (controller.signal.aborted || isAbortError(caught)) return
      setFailure(caught)
    } finally {
      if (!controller.signal.aborted) setBusy(false)
    }
  }

  function request(offset: number) {
    setBusy(true)
    setFailure(null)
    void fetchPage(offset)
  }

  useEffect(() => {
    if (autoLoad) void fetchPage(0)
    return () => pending.current?.abort()
    // The first page is requested once when the field appears.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const options = loaded?.options ?? []
  const known = options.some((option) => option.id === value)
  const more = loaded !== null && loaded.nextOffset < loaded.total

  return (
    <div className="paged-select">
      <Select
        label={label}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        hint={hint}
        disabled={disabled}
        aria-invalid={error ? true : undefined}
      >
        {allLabel !== undefined ? (
          <option value="">{allLabel}</option>
        ) : (
          <option value="">{busy && loaded === null ? `Loading ${noun}…` : `Choose…`}</option>
        )}
        {value !== '' && !known ? (
          <option value={value}>{unloadedLabel ? unloadedLabel(value) : value}</option>
        ) : null}
        {options.map((option) => (
          <option key={option.id} value={option.id}>
            {option.label}
          </option>
        ))}
      </Select>
      <div className="paged-select-actions">
        {loaded === null ? (
          busy ? (
            <span className="muted" role="status">
              Loading {noun}…
            </span>
          ) : (
            <Button busy={busy} disabled={disabled} onClick={() => request(0)}>
              Load {noun}
            </Button>
          )
        ) : more ? (
          <Button busy={busy} disabled={disabled} onClick={() => request(loaded.nextOffset)}>
            {busy
              ? `Loading ${noun}…`
              : `Load more ${noun} (${loaded.nextOffset} of ${loaded.total})`}
          </Button>
        ) : (
          <span className="muted">
            {loaded.options.length === 0 ? `No ${noun} are available.` : `All ${noun} are loaded.`}
          </span>
        )}
      </div>
      {error ? (
        <p className="field-error" role="alert">
          {error}
        </p>
      ) : null}
      {failure !== null ? (
        <p className="field-error" role="alert">
          Could not load {noun}. {userMessage(failure)}
        </p>
      ) : null}
    </div>
  )
}
