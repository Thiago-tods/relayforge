import type { ReactNode } from 'react'
import { Button } from './Button'

export function LoadingState({ label = 'Loading' }: { label?: string }) {
  return (
    <div className="state" role="status">
      <span className="spinner" aria-hidden="true" />
      <span>{label}…</span>
    </div>
  )
}

export function ErrorState({
  title = 'Something went wrong',
  message,
  onRetry,
  children,
}: {
  title?: string
  message: string
  onRetry?: () => void
  children?: ReactNode
}) {
  return (
    <div className="state state-error" role="alert">
      <h2>{title}</h2>
      <p>{message}</p>
      {onRetry ? <Button onClick={onRetry}>Try again</Button> : null}
      {children}
    </div>
  )
}
