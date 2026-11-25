import '@testing-library/jest-dom/vitest'
import { afterEach, vi } from 'vitest'
import { resetClient, setIdentityGuard, setUnauthenticatedHandler } from '../api/client'

// jsdom has HTMLDialogElement but not its methods. This stand-in only toggles the `open` attribute: it does not
// trap focus, make the page inert or draw a backdrop, so those behaviors are checked in a real browser, not here.
if (typeof HTMLDialogElement !== 'undefined') {
  const proto = HTMLDialogElement.prototype
  proto.showModal ??= function showModal(this: HTMLDialogElement) {
    if (this.hasAttribute('open'))
      throw new DOMException('The dialog is already open.', 'InvalidStateError')
    this.setAttribute('open', '')
  }
  proto.show ??= function show(this: HTMLDialogElement) {
    this.setAttribute('open', '')
  }
  proto.close ??= function close(this: HTMLDialogElement) {
    if (!this.hasAttribute('open')) return
    this.removeAttribute('open')
    this.dispatchEvent(new Event('close'))
  }
}

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
