import { Link } from 'react-router-dom'

export interface PageNotice {
  tone: 'info' | 'error'
  title: string
  message: string
  /** An optional next step, for example the subscription that was found. */
  link?: { to: string; label: string }
}

/** A notice under the page heading. Errors are announced at once (alert), information politely (status). */
export function NoticeBanner({ notice }: { notice: PageNotice }) {
  return (
    <section
      className={
        notice.tone === 'error' ? 'form-error destination-notice' : 'notice destination-notice'
      }
      role={notice.tone === 'error' ? 'alert' : 'status'}
      aria-label={notice.title}
    >
      <strong>{notice.title}.</strong> {notice.message}
      {notice.link !== undefined ? (
        <>
          {' '}
          <Link to={notice.link.to}>{notice.link.label}</Link>
        </>
      ) : null}
    </section>
  )
}
