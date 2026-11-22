import type { ReactNode } from 'react'
import { ThemeToggle } from '../../app/ThemeToggle'

export function AuthLayout({
  title,
  footer,
  children,
}: {
  title: string
  footer: ReactNode
  children: ReactNode
}) {
  return (
    <main className="auth-page">
      <div className="auth-card">
        <div className="auth-head">
          <span className="brand">RelayForge</span>
          <ThemeToggle />
        </div>
        <h1>{title}</h1>
        {children}
        <p className="auth-footer">{footer}</p>
      </div>
    </main>
  )
}
