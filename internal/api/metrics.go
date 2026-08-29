package api

import (
	"net/http"
	"strconv"

	"github.com/timdebruijn/smokeng/internal/metrics"
	"github.com/timdebruijn/smokeng/internal/probe"
	"github.com/timdebruijn/smokeng/internal/store"
)

// ProbeStats exposes the prober's own health.
type ProbeStats interface {
	Stats() probe.Stats
}

// IngestStats exposes how agent submissions are faring.
type IngestStats interface {
	Stats() (accepted, rejected int64)
}

// handleMetrics writes Prometheus text exposition about smokeng itself.
//
// Measurement data is deliberately absent. Latency and loss live in the store
// at full resolution and are read as Arrow; sending them through Prometheus
// would flatten every interval to one number, which is the exact loss this
// project exists to avoid. These metrics answer "is smokeng healthy", never
// "what is the network doing".
func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var out []metrics.Metric

	out = append(out, metrics.Metric{
		Name: "smokeng_build_info", Help: "Build information; the value is always 1.",
		Kind: metrics.Gauge,
		Samples: []metrics.Sample{
			{Labels: map[string]string{"version": s.version}, Value: 1},
		},
	})

	if s.probe != nil {
		st := s.probe.Stats()
		out = append(out,
			metrics.Simple("smokeng_targets_active",
				"Targets currently being probed by this instance.", metrics.Gauge,
				float64(st.ActiveTargets)),
			metrics.Simple("smokeng_measurements_written_total",
				"Measurements written to the store.", metrics.Counter,
				float64(st.MeasurementsWritten)),
			metrics.Simple("smokeng_measurement_write_errors_total",
				"Failed attempts to write a batch of measurements.", metrics.Counter,
				float64(st.WriteErrors)),
			metrics.Simple("smokeng_measurements_dropped_total",
				"Finalized measurements discarded because the writer fell behind. "+
					"Any value above zero means data was lost.", metrics.Counter,
				float64(st.Dropped)),
			metrics.Simple("smokeng_late_replies_total",
				"Replies that arrived after their interval had been finalized.", metrics.Counter,
				float64(st.LateReplies)),
			metrics.Simple("smokeng_socket_overflow_measurements_total",
				"Measurements taken while the kernel was dropping replies for want of "+
					"receive-queue space, so their loss is ours rather than the network's.",
				metrics.Counter, float64(st.SocketOverflows)),
			metrics.Simple("smokeng_dns_failures_total",
				"Intervals skipped because a target's hostname could not be resolved.",
				metrics.Counter, float64(st.DNSFailures)),
		)
	}

	if s.ingest != nil {
		accepted, rejected := s.ingest.Stats()
		out = append(out,
			metrics.Simple("smokeng_ingest_accepted_total",
				"Agent submissions that passed verification.", metrics.Counter, float64(accepted)),
			metrics.Simple("smokeng_ingest_rejected_total",
				"Agent submissions refused. The reason is in the log, deliberately not "+
					"in a label: a breakdown here would tell a caller which check they failed.",
				metrics.Counter, float64(rejected)),
		)
	}

	if agents, err := s.agents.ListAgents(r.Context()); err == nil {
		var enabled, lastSeen []metrics.Sample
		for _, a := range agents {
			if a.ID == store.LocalAgentID {
				continue // the built-in prober never reports in
			}
			labels := map[string]string{"agent": a.Name, "id": strconv.FormatInt(a.ID, 10)}
			enabled = append(enabled, metrics.Sample{Labels: labels, Value: boolValue(a.Enabled)})
			// Absent rather than zero when an agent has never reported: zero
			// would read as "last seen in 1970" and alert on it.
			if a.LastSeen != 0 {
				lastSeen = append(lastSeen, metrics.Sample{Labels: labels, Value: float64(a.LastSeen)})
			}
		}
		out = append(out,
			metrics.Metric{Name: "smokeng_agent_enabled",
				Help: "Whether a remote agent is enabled.", Kind: metrics.Gauge, Samples: enabled},
			metrics.Metric{Name: "smokeng_agent_last_seen_seconds",
				Help: "Unix time an agent last submitted successfully; absent if it never has.",
				Kind: metrics.Gauge, Samples: lastSeen})
	}

	if s.alerts != nil {
		out = append(out, metrics.Simple("smokeng_alerts_firing",
			"Alert rules currently firing.", metrics.Gauge, float64(len(s.alerts.Firing()))))
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := metrics.Write(w, out); err != nil {
		internalError(w, err)
	}
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
