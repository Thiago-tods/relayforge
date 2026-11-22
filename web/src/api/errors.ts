/** An error answered by the API, or a transport failure (status 0). Messages are safe to show to users. */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  /** Seconds from the Retry-After header, when the server sent one. */
  readonly retryAfter: number | undefined

  constructor(status: number, code: string, message: string, retryAfter?: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.retryAfter = retryAfter
  }
}

export function isApiError(value: unknown): value is ApiError {
  return value instanceof ApiError
}

export function isAbortError(value: unknown): boolean {
  return value instanceof DOMException && value.name === 'AbortError'
}

/** Returns the message of an unknown thrown value, never exposing anything but an ApiError message. */
export function userMessage(value: unknown): string {
  if (isApiError(value)) {
    if (value.status === 429 && value.retryAfter !== undefined) {
      return `${value.message} Try again in ${value.retryAfter} seconds.`
    }
    return value.message
  }
  return 'Something went wrong. Try again.'
}
