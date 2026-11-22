// Tabs of one browser share one session cookie and therefore one CSRF token (ADR 10). When one tab signs in or
// out, the others are told so they can re-check who the session belongs to. Only these two words travel: never a
// token, an identity or any data. Where BroadcastChannel is missing the tabs still recover lazily, because a
// rejected CSRF token triggers the same identity check.

export type AuthSignal = 'signed-in' | 'signed-out'

export interface AuthChannel {
  post: (signal: AuthSignal) => void
  close: () => void
}

const CHANNEL_NAME = 'relayforge-auth'

export function openAuthChannel(onSignal: (signal: AuthSignal) => void): AuthChannel {
  if (typeof BroadcastChannel === 'undefined') {
    return { post: () => undefined, close: () => undefined }
  }
  const channel = new BroadcastChannel(CHANNEL_NAME)
  channel.onmessage = (event: MessageEvent<unknown>) => {
    if (event.data === 'signed-in' || event.data === 'signed-out') onSignal(event.data)
  }
  return {
    post: (signal) => channel.postMessage(signal),
    close: () => channel.close(),
  }
}
