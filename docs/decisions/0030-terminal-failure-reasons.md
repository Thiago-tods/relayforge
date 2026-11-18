# 30. A delivery that fails records why, in a closed taxonomy

Date: 2026-11-18

## Status

Accepted. Builds on ADR 24 (lifecycle and retries). The state machine, retry timing, attempt budgets, claim
behavior and lease recovery are unchanged.

## Context

A delivery became `FAILED` in exactly one place, `delivery.Finish`, but nothing recorded why. The retry policy
computed a reason (`non_retryable` or `exhausted`), the worker logged it, and it was then lost. What remained
(`last_http_status`, `last_error_class`) could not tell a final 404 from a 503 on the last attempt, and did not
say whether a delivery that ran out of attempts had been failing because of the destination or because of the
platform (an undecryptable secret, a lost lease). Operators could not tell what to fix.

Seven situations end in `FAILED`, all through `Decide`:

1. a 3xx response (redirects are never followed);
2. a 4xx other than 408, 425 and 429;
3. the egress guard refused the address or URL;
4. the delivery's event or destination could not be loaded (`data_missing`; defensive, foreign keys prevent it);
5. the last attempt ended in a retryable network, timeout or HTTP result;
6. the last attempt was abandoned by the worker (`destination_inactive`, `secret_unavailable`,
   `shutdown_interrupted`);
7. the last attempt's lease expired and the reaper recorded `lease_expired`.

## Decision

### Taxonomy

Five values, one closed set (`delivery.FailureReason`), chosen by code from the attempt's classification and never
derived from a response body, header or error text:

| Reason | Situations | Meaning |
|---|---|---|
| `DESTINATION_REJECTED` | 2 | The destination answered with a final 4xx. Fix the destination or the event contract. |
| `REDIRECT_REFUSED` | 1 | The destination answered with a 3xx. RelayForge never follows redirects; update the destination URL. |
| `EGRESS_BLOCKED` | 3 | The egress guard refused the destination address or URL (private, reserved or malformed target). |
| `RETRIES_EXHAUSTED` | 5, 6, 7 | The attempt budget ran out while results were still retryable. The last attempt's `outcome`, `httpStatus` and `errorClass` say what the last failure looked like, including platform causes such as `secret_unavailable` or `lease_expired`. |
| `INTERNAL_ERROR` | 4, and any outcome nothing else describes | A platform fault. Not caused by the destination. |

Names that were considered and not adopted: `DESTINATION_DISABLED` and `DESTINATION_NOT_FOUND` never lead to
`FAILED` (a paused destination keeps its deliveries retryable, a revoked one cancels them: `CANCELLED`);
`INVALID_CONFIGURATION` overlaps `EGRESS_BLOCKED`; `OPERATOR_ACTION_REQUIRED` is a remedy, not a cause, and can be
derived from the reason; `UNKNOWN_TERMINAL_FAILURE` is replaced by `INTERNAL_ERROR`, which is also the fallback, so
a new outcome can never produce a `FAILED` row without a reason.

`RETRIES_EXHAUSTED` is deliberately not split into destination and platform causes: the attempt history already
carries the cause, and a smaller taxonomy is easier to keep stable.

### Where it is decided

`RetryPolicy.Decide` returns `Decision.FailureReason`, set exactly when the status is `FAILED`. Both callers of
`Finish`, the worker (`Process`) and lease recovery (`recoverBatch`), already go through `Decide`, so every path
into `FAILED` receives a reason and none can forget it. `Finish` refuses (before writing anything) a decision whose
status and reason disagree: `FAILED` without a valid reason, or any other status with one.

### Where it is stored (migration 00019)

- `deliveries.failure_reason`: non-NULL only while the delivery is `FAILED`.
- `delivery_attempts.terminal_reason`: the same reason on the one attempt that ended the delivery. Earlier and
  successful attempts keep NULL.

Both are nullable text with the same closed `CHECK` list (the same style as `delivery_attempts_outcome_valid`),
so a new reason needs a migration on purpose.

#### What the database enforces

These are constraints PostgreSQL checks for every write, whatever code issues it:

- `failure_reason` is one of the five values;
- `failure_reason IS NULL OR status = 'FAILED'`, so a retry (`PENDING`), a claim or a success can never carry one;
- `status <> 'FAILED' OR failure_reason IS NOT NULL`, so a delivery that fails from now on always has one. This one
  is added `NOT VALID` and never validated, because of the legacy rows below;
- `terminal_reason` is one of the five values and is NULL on a successful attempt;
- terminal deliveries stay immutable (`relayforge_enforce_delivery_transition`), the reason included, and
  `delivery_attempts` stays append-only (`relayforge_reject_attempt_change`). No trigger changed.

#### What the application guarantees (`delivery.Finish`)

PostgreSQL does **not** independently enforce that a delivery's `failure_reason` equals the `terminal_reason` of an
attempt, that such an attempt is the delivery's last one, or that no earlier attempt carries a `terminal_reason`:
those are properties of how the rows are written. `Finish` is the only production code that writes either column,
and it provides them:

- it refuses, before writing anything, a decision whose status and reason disagree (`FAILED` without a valid reason,
  or any other status with one);
- in one transaction it inserts the terminal attempt with `terminal_reason` and then updates the delivery with the
  same `failure_reason`, using the existing claim-token fence (`status = 'IN_FLIGHT' AND claim_token = $token`);
- if the fenced update affects no row, the claim is stale: `Finish` returns `ErrStaleClaim` and the caller rolls
  back, which removes the attempt and its reason together. A worker racing the reaper on the last attempt therefore
  leaves exactly one attempt row, and one reason on both rows, from whichever of them won.

Direct SQL that bypasses `Finish` could still write a `terminal_reason` on a non-final `HTTP_ERROR` attempt, or two
different reasons. The tests check the application guarantees (including the race and rollback cases); nothing
should write these columns outside `Finish`.

### Legacy rows

Deliveries that were already `FAILED` keep a NULL `failure_reason`; nothing is backfilled. The old reason is only
partly recoverable from the history, and terminal rows are immutable, so a backfill would have to bypass the
guard. The API omits the field for them and the documentation says what its absence means: "failed before reasons
were recorded". The `NOT VALID` constraint is enforced for every row written from now on.

### Redrive readiness

Nothing here implements redrive. The design leaves it room, and a future redrive (Phase 4.4.6) must respect these
points:

- It must clear `deliveries.failure_reason` (and `completed_at`) in the same `UPDATE` that leaves `FAILED`: the
  constraints reject a `PENDING` delivery that keeps a reason.
- Existing attempt history must remain unchanged. `delivery_attempts` is append-only, so the attempt that failed
  keeps its `terminal_reason` and that reason stays available after the redrive.
- Moving `FAILED` to `PENDING` is not enough. The transition trigger forbids it today and needs a deliberate change.
- A delivery that exhausted its attempt budget has `attempt_count = max_attempts`. A `PENDING` delivery must have
  `attempt_count < max_attempts` (`deliveries_attempts_match_status`), and `max_attempts` is immutable in the same
  trigger, so redriving a `RETRIES_EXHAUSTED` delivery also needs an explicit retry-budget decision (how many more
  attempts, and how that is recorded) and the matching trigger change. Attempt numbers must keep growing without
  gaps.
- After a redrive a delivery can fail again, so a delivery may have **several** attempts with a `terminal_reason`,
  one per failure episode. Today's wording, "the attempt that ended the delivery", then means the attempt that
  ended the latest failure episode, and the API documentation will need to say so. The delivery's `failure_reason`
  always describes the latest episode (and is absent while the delivery is not `FAILED`).

A test (`TestTheReasonConstraintsLeaveRoomForARedrive`) disables the transition trigger inside a transaction that
is always rolled back, and shows that clearing the reason is accepted while the attempt history, its terminal reason
included, is unchanged, and that an exhausted delivery still cannot return to `PENDING` without a budget change.
Production triggers were not relaxed.

### Deployment order

Migration 00019 and the application binary depend on each other:

- The **new** binary cannot finalize deliveries against the **old** schema: `Finish` writes `terminal_reason` and
  `failure_reason`, which do not exist yet, so every finalization fails, successes included.
- The **old** binary cannot safely finalize deliveries against the **new** schema: it does not write a reason, so
  the `deliveries_failed_has_reason` constraint rejects its transitions into `FAILED` (a final 4xx, an exhausted
  last attempt, and recovery of an expired lease on the last attempt). Its other transitions still work.

So deploy in this order:

1. Apply migration 00019.
2. Drain or promptly replace the old application workers.
3. Deploy the new application binary.

This is a rolling-deployment compatibility limitation, not a data-loss issue. A delivery whose finalization is
rejected stays `IN_FLIGHT`; its lease expires and, once the new binary runs, lease recovery records the attempt as
abandoned (the attempt still counts) and the delivery continues normally. Nothing is lost or corrupted, but deliveries that were due to fail wait until the old workers are
gone, so keep the window short.

### API

The two existing read routes, with no new route, permission or role change (`deliveries:read`: OWNER, ADMIN,
DEVELOPER and VIEWER):

- `GET .../deliveries` and `GET .../deliveries/{id}`: `failureReason` on a delivery;
- `GET .../deliveries/{id}`: `terminalReason` on the attempt that ended it.

Both are omitted, not `null`, when absent (not failed, not terminal, or legacy). Only the enum value is returned.
The value is a stable code for clients to map to their own text; details stay in the already-exposed
`outcome`, `httpStatus` and `errorClass`. No raw error message, response body, payload, URL or secret is stored or
returned, because the reason is chosen by code from a closed set.

### Metrics and logs

- `relayforge_delivery_failed_total` is unchanged.
- One counter per reason, with a fixed name: `relayforge_delivery_failed_destination_rejected_total`,
  `..._redirect_refused_total`, `..._egress_blocked_total`, `..._retries_exhausted_total` and
  `..._internal_error_total`. The registry has no labels; because the set is closed, the metric set is constant and
  nothing from a request, destination or organization can become a metric name. The per-reason counters sum to the
  total.
- The existing "delivery attempt failed" and "expired lease recovered" log events carry `failure_reason` when (and
  only when) the delivery failed.
- Audit is not used. Terminal failures are system events with no actor, one per delivery, and would flood the audit
  table. Audit belongs to the operator action that redrives a delivery.

## Consequences

- Every new `FAILED` delivery says why, in the delivery, in its last attempt, in a metric and in a log line.
- `Decision` gains a field; code that builds a `FAILED` `Decision` by hand (tests) must set a valid reason.
- Existing `FAILED` rows read back without a reason.
- Adding a reason later needs a migration (an `IN` list in two `CHECK` constraints) and a Go constant.

## Limitations and risks

- A shutdown interruption or a lost lease on the **last** attempt ends the delivery as `RETRIES_EXHAUSTED`. That is
  existing behavior (the attempt counts), now visible: the last attempt's `errorClass` shows it. It is not changed here.
- `data_missing` cannot occur while the foreign keys hold, so `INTERNAL_ERROR` is exercised through the same
  `Decide` and `Finish` path rather than through the worker.
- The migration adds the constraints `NOT VALID` and validates the ones every row satisfies, outside a transaction,
  so it takes only brief locks and no long scan under an exclusive lock. If it is interrupted, running it again
  repairs it (every statement is idempotent).
