import { Link } from 'react-router-dom'

export function NotFoundPage({
  message = 'The page you requested does not exist.',
}: {
  message?: string
}) {
  return (
    <div className="page">
      <h1>Page not found</h1>
      <p>{message}</p>
      <p>
        <Link to="/orgs">Go to your organizations</Link>
      </p>
    </div>
  )
}
