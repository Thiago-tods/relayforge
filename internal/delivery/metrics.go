package delivery

import (
	"strings"

	"github.com/Thiago-tods/relayforge/internal/observability"
)

// Metrics are the delivery engine's counters, histograms and gauges. Names are
// fixed and carry no labels, so cardinality is constant: organizations,
// deliveries, events and destinations never appear in a metric. Every field may
// be nil (no registry), and a nil metric ignores updates.
type Metrics struct {
	Claimed            *observability.Counter
	Succeeded          *observability.Counter
	RetriesScheduled   *observability.Counter
	Failed             *observability.Counter
	AbandonedAttempts  *observability.Counter
	StaleFinalizations *observability.Counter
	FinalizeErrors     *observability.Counter
	Interrupted        *observability.Counter
	ExpiredRecovered   *observability.Counter
	RecoveryErrors     *observability.Counter
	CancelledRevoked   *observability.Counter
	ClaimErrors        *observability.Counter
	// FailedByReason counts deliveries that ended as failed, one counter per FailureReason.
	// The registry has no labels, so each reason has its own fixed metric name: the set is
	// closed (FailureReason), so cardinality is constant and no value from a request,
	// destination or organization can ever become a metric name.
	FailedByReason map[FailureReason]*observability.Counter

	AttemptSeconds  *observability.Histogram
	EndToEndSeconds *observability.Histogram

	Pending           *observability.Gauge
	DuePending        *observability.Gauge
	InFlight          *observability.Gauge
	ExpiredLeases     *observability.Gauge
	OutboxUnpublished *observability.Gauge
	// Ages, in whole seconds, are the stuck-work signals: counts alone cannot tell a
	// healthy backlog from a stalled one.
	OldestDueAgeSeconds    *observability.Gauge
	OldestOutboxAgeSeconds *observability.Gauge
}

// NewMetrics registers the delivery metrics. reg may be nil.
func NewMetrics(reg *observability.Registry) *Metrics {
	byReason := make(map[FailureReason]*observability.Counter, len(AllFailureReasons()))
	for _, r := range AllFailureReasons() {
		byReason[r] = reg.Counter(FailedReasonMetricName(r), "Deliveries that ended as failed with reason "+string(r)+".")
	}
	return &Metrics{
		FailedByReason:     byReason,
		Claimed:            reg.Counter("relayforge_delivery_claimed_total", "Deliveries claimed by a worker."),
		Succeeded:          reg.Counter("relayforge_delivery_succeeded_total", "Deliveries completed with a 2xx response."),
		RetriesScheduled:   reg.Counter("relayforge_delivery_retries_scheduled_total", "Failed attempts that scheduled another attempt."),
		Failed:             reg.Counter("relayforge_delivery_failed_total", "Deliveries that ended as failed (permanent error or retries exhausted)."),
		AbandonedAttempts:  reg.Counter("relayforge_delivery_abandoned_attempts_total", "Attempts recorded as abandoned (lease expired, destination or secret unavailable)."),
		StaleFinalizations: reg.Counter("relayforge_delivery_stale_finalizations_total", "Results discarded because the claim token was no longer current."),
		FinalizeErrors:     reg.Counter("relayforge_delivery_finalize_errors_total", "Results that could not be stored; the lease will expire and recovery will record the attempt."),
		Interrupted:        reg.Counter("relayforge_delivery_interrupted_total", "Requests interrupted by shutdown and left for lease recovery."),
		ExpiredRecovered:   reg.Counter("relayforge_delivery_expired_leases_recovered_total", "Expired leases recovered by the reaper."),
		RecoveryErrors:     reg.Counter("relayforge_delivery_recovery_errors_total", "Expired leases the reaper could not recover."),
		CancelledRevoked:   reg.Counter("relayforge_delivery_cancelled_revoked_total", "Pending deliveries cancelled because their destination was revoked."),
		ClaimErrors:        reg.Counter("relayforge_delivery_claim_errors_total", "Failed attempts to claim deliveries."),

		AttemptSeconds:  reg.Histogram("relayforge_delivery_attempt_seconds", "Duration of one delivery attempt."),
		EndToEndSeconds: reg.Histogram("relayforge_delivery_end_to_end_seconds", "Time from delivery creation to success."),

		Pending:           reg.Gauge("relayforge_delivery_pending", "Deliveries waiting for an attempt."),
		DuePending:        reg.Gauge("relayforge_delivery_due_pending", "Pending deliveries whose next attempt is due."),
		InFlight:          reg.Gauge("relayforge_delivery_in_flight", "Deliveries currently claimed by a worker."),
		ExpiredLeases:     reg.Gauge("relayforge_delivery_expired_leases", "In-flight deliveries whose lease has expired and awaits recovery."),
		OutboxUnpublished: reg.Gauge("relayforge_outbox_unpublished", "Outbox records not yet published."),

		OldestDueAgeSeconds:    reg.Gauge("relayforge_delivery_oldest_due_age_seconds", "Seconds the longest-waiting due delivery has been due (0 when none)."),
		OldestOutboxAgeSeconds: reg.Gauge("relayforge_outbox_oldest_unpublished_age_seconds", "Age in seconds of the oldest unpublished outbox record (0 when none)."),
	}
}

// FailedReasonMetricName is the counter name for one failure reason, for example
// relayforge_delivery_failed_destination_rejected_total.
func FailedReasonMetricName(r FailureReason) string {
	return "relayforge_delivery_failed_" + strings.ToLower(string(r)) + "_total"
}

// countFailed counts one failed delivery in the total and in its reason's counter. An
// unknown reason cannot reach here (Finish rejects it), but is still counted in the total only.
func (m *Metrics) countFailed(r FailureReason) {
	m.Failed.Inc()
	if c := m.FailedByReason[r]; c != nil {
		c.Inc()
	}
}
