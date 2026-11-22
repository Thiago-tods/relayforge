import { AppRoutes } from './app/AppRoutes'
import { ThemeProvider } from './app/theme'
import { AuthProvider } from './auth/AuthProvider'
import { ToastProvider } from './ui/toast'

/** Providers and routes. The router itself is supplied by the caller (BrowserRouter, or MemoryRouter in tests). */
export function App() {
  return (
    <ThemeProvider>
      <ToastProvider>
        <AuthProvider>
          <AppRoutes />
        </AuthProvider>
      </ToastProvider>
    </ThemeProvider>
  )
}
