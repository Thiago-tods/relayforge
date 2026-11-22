import { COUNT_CAP, formatCount, isCapped } from '../lib/format'

/**
 * A delivery count. A saturated count (100,000 or more) is shown as a lower bound, "≥ 100,000", and is announced as
 * "at least 100,000": it is never presented as an exact value.
 */
export function Count({ value }: { value: number }) {
  if (!isCapped(value)) return <>{formatCount(value)}</>
  return (
    <>
      <span className="lower-bound" aria-hidden="true">
        {formatCount(value)}
      </span>
      <span className="visually-hidden">at least {COUNT_CAP.toLocaleString('en-US')}</span>
    </>
  )
}
