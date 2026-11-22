import { useState } from 'react'
import type { FormEvent } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { createOrganization } from '../../api/organizations'
import { userMessage } from '../../api/errors'
import { useAuth } from '../../auth/authContext'
import { organizationNameError, slugError } from '../auth/validation'
import { Button } from '../../ui/Button'
import { Field } from '../../ui/Field'
import { useFocusFirstError } from '../../ui/useFocusFirstError'
import { useToast } from '../../ui/toastContext'

export function OrgPickerPage() {
  const { organizations, addOrganization } = useAuth()
  const toast = useToast()
  const navigate = useNavigate()
  const [name, setName] = useState('')
  const [slug, setSlug] = useState('')
  const [errors, setErrors] = useState<{ name?: string; slug?: string }>({})
  const [failure, setFailure] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const form = useFocusFirstError(errors)

  async function submit(event: FormEvent) {
    event.preventDefault()
    if (busy) return
    const nameProblem = name.trim() === '' ? 'Enter a name.' : organizationNameError(name)
    const slugProblem = slugError(slug)
    setErrors({
      ...(nameProblem ? { name: nameProblem } : {}),
      ...(slugProblem ? { slug: slugProblem } : {}),
    })
    if (nameProblem || slugProblem) return
    setBusy(true)
    setFailure(null)
    try {
      const organization = await createOrganization(name.trim(), slug.trim())
      addOrganization(organization)
      toast.push('success', `Created ${organization.name}.`)
      await navigate(`/o/${organization.id}`)
    } catch (error) {
      setFailure(userMessage(error))
      setBusy(false)
    }
  }

  return (
    <div className="page">
      <header className="page-header">
        <h1>Organizations</h1>
        <p>Choose the organization you want to work in.</p>
      </header>

      {organizations.length === 0 ? (
        <p className="empty">You are not a member of any organization yet. Create one below.</p>
      ) : (
        <ul className="card-list" aria-label="Your organizations">
          {organizations.map((organization) => (
            <li key={organization.id}>
              <Link to={`/o/${organization.id}`} className="card-link">
                <span className="card-title">{organization.name}</span>
                <span className="muted">{organization.slug}</span>
                <span className="badge">{organization.role}</span>
              </Link>
            </li>
          ))}
        </ul>
      )}

      <section className="panel" aria-labelledby="create-org">
        <h2 id="create-org">Create an organization</h2>
        <form ref={form} onSubmit={submit} noValidate>
          <Field
            label="Name"
            name="name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            error={errors.name}
          />
          <Field
            label="Slug (optional)"
            name="slug"
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
            Create organization
          </Button>
        </form>
      </section>
    </div>
  )
}
