import { Button } from './Button'
import { useToast } from './toastContext'

/** An identifier shown as selectable text with a button that copies it. */
export function CopyText({ value, what }: { value: string; what: string }) {
  const toast = useToast()
  async function copy() {
    try {
      await navigator.clipboard.writeText(value)
      toast.push('success', `${what} copied.`)
    } catch {
      toast.push('error', `Could not copy the ${what.toLowerCase()}. Select the text and copy it.`)
    }
  }
  return (
    <span className="copy-text">
      <code>{value}</code>
      <Button
        variant="ghost"
        onClick={() => void copy()}
        aria-label={`Copy ${what.toLowerCase()} ${value}`}
      >
        Copy
      </Button>
    </span>
  )
}
