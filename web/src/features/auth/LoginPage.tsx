import { useState } from 'react'
import type { FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { userMessage } from '../../api/errors'
import { useAuth } from '../../auth/authContext'
import { Button } from '../../ui/Button'
import { Field } from '../../ui/Field'
import { useFocusFirstError } from '../../ui/useFocusFirstError'
import { AuthLayout } from './AuthLayout'
import { emailError, passwordRequired } from './validation'

export function LoginPage() {
  const auth = useAuth()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [errors, setErrors] = useState<{ email?: string; password?: string }>({})
  const [failure, setFailure] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const form = useFocusFirstError(errors)

  async function submit(event: FormEvent) {
    event.preventDefault()
    if (busy) return
    const next = { email: emailError(email), password: passwordRequired(password) }
    setErrors({
      ...(next.email ? { email: next.email } : {}),
      ...(next.password ? { password: next.password } : {}),
    })
    if (next.email || next.password) return
    setBusy(true)
    setFailure(null)
    try {
      // On success the route guard redirects, so there is nothing to do here.
      await auth.login(email.trim(), password)
    } catch (error) {
      setFailure(userMessage(error))
      setBusy(false)
    }
  }

  return (
    <AuthLayout title="Sign in" footer={<Link to="/register">Create an account</Link>}>
      {auth.notice ? (
        <p className="notice" role="status">
          {auth.notice}
        </p>
      ) : null}
      <form ref={form} onSubmit={submit} noValidate>
        <Field
          label="Email"
          type="email"
          name="email"
          autoComplete="username"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          error={errors.email}
        />
        <Field
          label="Password"
          type="password"
          name="password"
          autoComplete="current-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          error={errors.password}
        />
        {failure ? (
          <p className="form-error" role="alert">
            {failure}
          </p>
        ) : null}
        <Button type="submit" variant="primary" busy={busy}>
          Sign in
        </Button>
      </form>
    </AuthLayout>
  )
}
