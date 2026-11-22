import { Button } from './Button'

/**
 * Previous and next for a cursor feed. There is no total and no jump to a page: the server only says whether
 * another page follows.
 */
export function Pager({
  label,
  pageNumber,
  hasPrevious,
  hasNext,
  busy,
  onPrevious,
  onNext,
}: {
  label: string
  pageNumber: number
  hasPrevious: boolean
  hasNext: boolean
  busy: boolean
  onPrevious: () => void
  onNext: () => void
}) {
  return (
    <nav className="pager" aria-label={`${label} pages`}>
      <Button onClick={onPrevious} disabled={!hasPrevious || busy}>
        Previous
      </Button>
      <span className="pager-page">
        Page {pageNumber}
        {hasNext ? '' : ' (last page)'}
      </span>
      <Button onClick={onNext} disabled={!hasNext || busy}>
        Next
      </Button>
    </nav>
  )
}
