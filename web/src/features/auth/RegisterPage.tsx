import { useState } from 'react'
import type { FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { userMessage } from '../../api/errors'
import { useAuth } from '../../auth/authContext'
import { Button } from '../../ui/Button'
import { Field } from '../../ui/Field'
import { useFocusFirstError } from '../../ui/useFocusFirstError'
import { AuthLayout } from './AuthLayout'
import {
  displayNameError,
  emailError,
  newPasswordError,
  organizationNameError,
  slugError,
} from './validation'

type Errors = Partial<
  Record<'displayName' | 'email' | 'password' | 'organizationName' | 'slug', string>
>

export function RegisterPage() {
  const auth = useAuth()
  const [displayName, setDisplayName] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [organizationName, setOrganizationName] = useState('')
  const [slug, setSlug] = useState('')
  const [errors, setErrors] = useState<Errors>({})
  const [failure, setFailure] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const form = useFocusFirstError(errors)

  async function submit(event: FormEvent) {
    event.preventDefault()
    if (busy) return
    const found: Errors = {}
    const checks: [keyof Errors, string | undefined][] = [
      ['displayName', displayNameError(displayName)],
      ['email', emailError(email)],
      ['password', newPasswordError(password)],
      ['organizationName', organizationNameError(organizationName)],
      ['slug', slugError(slug)],
    ]
    for (const [field, message] of checks) if (message) found[field] = message
    setErrors(found)
    if (Object.keys(found).length > 0) return

    setBusy(true)
    setFailure(null)
    try {
      // On success the route guard redirects into the new organization.
      await auth.register({
        email: email.trim(),
        password,
        displayName: displayName.trim(),
        organizationName: organizationName.trim(),
        organizationSlug: slug.trim(),
      })
    } catch (error) {
      setFailure(userMessage(error))
      setBusy(false)
    }
  }

  return (
    <AuthLayout
      title="Create an account"
      footer={<Link to="/login">Already registered? Sign in</Link>}
    >
      <form ref={form} onSubmit={submit} noValidate>
        <Field
          label="Your name"
          name="displayName"
          autoComplete="name"
          value={displayName}
          onChange={(e) => setDisplayName(e.target.value)}
          error={errors.displayName}
        />
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
          autoComplete="new-password"
          hint="12 to 128 characters."
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          error={errors.password}
        />
        <Field
          label="Organization name (optional)"
          name="organizationName"
          value={organizationName}
          onChange={(e) => setOrganizationName(e.target.value)}
          error={errors.organizationName}
        />
        <Field
          label="Organization slug (optional)"
          name="organizationSlug"
          hint="3 to 48 lowercase letters, digits or single hyphens. Derived from the name when empty."
          value={slug}
          onChange={(e) => setSlug(e.target.value)}
          error={errors.slug}
        />
        {failure ? (
          <p className="form-error" role="alert">
            {failure}
          </p>
        ) : null}
        <Button type="submit" variant="primary" busy={busy}>
          Create account
        </Button>
      </form>
    </AuthLayout>
  )
}
