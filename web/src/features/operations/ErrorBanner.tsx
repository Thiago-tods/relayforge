import { isApiError, userMessage } from '../../api/errors'
import { Button } from '../../ui/Button'

/** The error of one read, with a retry. A read that timed out gets the hint that fits that read. */
export function ErrorBanner({
  error,
  onRetry,
  timeoutHint,
}: {
  error: unknown
  onRetry: () => void
  timeoutHint: string
}) {
  return (
    <div className="form-error" role="alert">
      <p>{userMessage(error)}</p>
      {isApiError(error) && error.code === 'read_model_timeout' ? <p>{timeoutHint}</p> : null}
      <Button onClick={onRetry}>Try again</Button>
    </div>
  )
}
