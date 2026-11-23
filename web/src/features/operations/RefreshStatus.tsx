import { userMessage } from '../../api/errors'

/**
 * Says that the latest refresh of a section failed while the data of the last successful read is still shown. It is
 * a notice, not an alert: nothing is blocked and the next scheduled refresh tries again. The wrapper is always
 * mounted so that a screen reader is already watching it when the text first appears.
 */
export function RefreshStatus({ error, hasData }: { error: unknown; hasData: boolean }) {
  return (
    <div role="status">
      {error !== null && hasData ? (
        <p className="notice">
          Could not refresh this section. The data below is from the last successful read.{' '}
          {userMessage(error)}
        </p>
      ) : null}
    </div>
  )
}
