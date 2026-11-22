import '@testing-library/jest-dom/vitest'
import { afterEach, vi } from 'vitest'
import { resetClient, setIdentityGuard, setUnauthenticatedHandler } from '../api/client'

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  resetClient()
  setUnauthenticatedHandler(null)
  setIdentityGuard(null)
  window.localStorage.clear()
  delete document.documentElement.dataset.theme
})
