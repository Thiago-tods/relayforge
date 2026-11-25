import { useEffect, useRef, useState } from 'react'
import { getDeliveryDetail, redriveDelivery } from '../../api/deliveries'
import type { DeliveryDetail, DeliveryRecord, RedriveResult } from '../../api/deliveryTypes'
import {
  compareAfterAmbiguity,
  reactToError,
  successOutcome,
  type AmbiguityCause,
  type Outcome,
} from './redriveOutcome'

/** The client gives up waiting for the redrive answer after this long. The result is then unknown (ambiguous). */
export const REDRIVE_TIMEOUT_MS = 30_000

export type RedriveFlow =
  | { phase: 'idle' }
  /** The dialog is open. `snapshot` is the delivery as shown when it opened: its attempt count is what we send. */
  | { phase: 'confirming'; snapshot: DeliveryRecord }
  | { phase: 'submitting'; snapshot: DeliveryRecord }
  /** The result of the request is unknown and one GET of the delivery is running. */
  | { phase: 'checking'; snapshot: DeliveryRecord; cause: AmbiguityCause }
  /** The GET failed. Only a manual check (a GET) can resolve the doubt. */
  | { phase: 'unverified'; snapshot: DeliveryRecord; cause: AmbiguityCause }
  | { phase: 'outcome'; outcome: Outcome; refreshFailed: boolean }

export interface DialogError {
  message: string
  /** True when the message belongs to the reason field. */
  field: boolean
}

interface State {
  scope: string
  flow: RedriveFlow
  error: DialogError | null
  permissionLost: boolean
}

interface Run {
  controller: AbortController
  timer: ReturnType<typeof setTimeout> | undefined
  timedOut: boolean
  /** Set when the owner went away (unmount, organization or delivery change): the result is ignored. */
  cancelled: boolean
}

const IDLE: RedriveFlow = { phase: 'idle' }

interface Options {
  orgId: string
  deliveryId: string
  /** Whether the user may redrive (deliveries:redrive). Without it every action does nothing. */
  allowed: boolean
  /** Receives every delivery detail the hook read, so the page can show it. */
  onFresh: (detail: DeliveryDetail) => void
}

/**
 * The redrive state machine. One confirmation sends one POST: the POST is never repeated, not after a timeout, a lost
 * connection, an unreadable 200 or a server error. Such an ambiguous result is resolved by reading the delivery once
 * (and again only when the user asks, which is a GET as well). A fresh redrive needs a fresh dialog.
 */
export function useRedrive({ orgId, deliveryId, allowed, onFresh }: Options) {
  const scope = `${orgId}:${deliveryId}`
  const [state, setState] = useState<State>({
    scope,
    flow: IDLE,
    error: null,
    permissionLost: false,
  })
  const current: State =
    state.scope === scope ? state : { scope, flow: IDLE, error: null, permissionLost: false }
  const flow = current.flow

  const runs = useRef(new Set<Run>())
  // Synchronous guard: two submits in the same tick (double click, Enter plus click) see the same render.
  const inFlight = useRef(false)
  const onFreshRef = useRef(onFresh)
  useEffect(() => {
    onFreshRef.current = onFresh
  })

  // Leaving (unmount, other organization or delivery) cancels everything and ignores its results, and starts no check.
  useEffect(() => {
    const active = runs.current
    return () => {
      for (const run of active) {
        run.cancelled = true
        clearTimeout(run.timer)
        run.controller.abort()
      }
      active.clear()
      inFlight.current = false
    }
  }, [scope])

  const show = (next: RedriveFlow, error: DialogError | null = null, permissionLost?: boolean) =>
    setState((previous) => ({
      scope,
      flow: next,
      error,
      permissionLost:
        permissionLost ?? (previous.scope === scope ? previous.permissionLost : false),
    }))

  const start = (): Run => {
    const run: Run = {
      controller: new AbortController(),
      timer: undefined,
      timedOut: false,
      cancelled: false,
    }
    run.timer = setTimeout(() => {
      run.timedOut = true
      run.controller.abort()
    }, REDRIVE_TIMEOUT_MS)
    runs.current.add(run)
    return run
  }
  const finish = (run: Run) => {
    clearTimeout(run.timer)
    runs.current.delete(run)
  }

  /** One GET of the delivery. Resolves null when the owner went away; throws when the GET failed. */
  const readLatest = async (): Promise<DeliveryDetail | null> => {
    const run = start()
    try {
      const detail = await getDeliveryDetail(orgId, deliveryId, run.controller.signal)
      if (run.cancelled) return null
      onFreshRef.current(detail)
      return detail
    } catch (error) {
      if (run.cancelled) return null
      throw error
    } finally {
      finish(run)
    }
  }

  const refreshAfterOutcome = async () => {
    try {
      await readLatest()
    } catch {
      // The banner stays. The page tells the user the detail may be out of date.
      setState((previous) =>
        previous.scope === scope && previous.flow.phase === 'outcome'
          ? { ...previous, flow: { ...previous.flow, refreshFailed: true } }
          : previous,
      )
    }
  }

  const verify = async (snapshot: DeliveryRecord, cause: AmbiguityCause) => {
    show({ phase: 'checking', snapshot, cause })
    try {
      const detail = await readLatest()
      if (detail === null) return
      show({
        phase: 'outcome',
        outcome: compareAfterAmbiguity(snapshot, detail.delivery),
        refreshFailed: false,
      })
    } catch {
      show({ phase: 'unverified', snapshot, cause })
    }
  }

  const perform = async (snapshot: DeliveryRecord, reason: string) => {
    const run = start()
    let result: RedriveResult
    try {
      result = await redriveDelivery(
        orgId,
        deliveryId,
        { reason, expectedAttemptCount: snapshot.attemptCount },
        snapshot.maxAttempts,
        run.controller.signal,
      )
    } catch (error) {
      finish(run)
      if (run.cancelled) return
      inFlight.current = false
      const reaction = reactToError(error, run.timedOut)
      if (reaction.kind === 'stay') {
        show(
          { phase: 'confirming', snapshot },
          { message: reaction.message, field: reaction.field },
        )
      } else if (reaction.kind === 'close') {
        show(
          { phase: 'outcome', outcome: reaction.outcome, refreshFailed: false },
          null,
          reaction.permissionLost ? true : undefined,
        )
        if (reaction.refresh) await refreshAfterOutcome()
      } else {
        await verify(snapshot, reaction.cause)
      }
      return
    }
    finish(run)
    if (run.cancelled) return
    inFlight.current = false
    show({ phase: 'outcome', outcome: successOutcome(result), refreshFailed: false })
    await refreshAfterOutcome()
  }

  return {
    flow,
    dialogError: current.error,
    /** True after the server answered 403 forbidden: the action stays hidden for this page. */
    permissionLost: current.permissionLost,
    /** Opens the dialog for the delivery as it is shown now. */
    open: (delivery: DeliveryRecord) => {
      if (!allowed || current.permissionLost || delivery.status !== 'FAILED') return
      if (flow.phase !== 'idle' && flow.phase !== 'outcome') return
      show({ phase: 'confirming', snapshot: delivery })
    },
    cancel: () => {
      if (flow.phase === 'confirming') show(IDLE)
    },
    submit: (reason: string) => {
      if (!allowed || inFlight.current || flow.phase !== 'confirming') return
      inFlight.current = true
      show({ phase: 'submitting', snapshot: flow.snapshot })
      void perform(flow.snapshot, reason)
    },
    /** A manual state check after a failed one. It is a GET and never a POST. */
    checkAgain: () => {
      if (flow.phase !== 'unverified') return
      void verify(flow.snapshot, flow.cause)
    },
  }
}
