import { formatUtc } from '../lib/format'

/** A timestamp from the API, shown in UTC and labeled as such. The exact value stays in the datetime attribute. */
export function DateTime({ value }: { value: string }) {
  return <time dateTime={value}>{formatUtc(value)}</time>
}
