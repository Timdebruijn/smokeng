package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Alert is one outgoing notification.
type Alert struct {
	Rule       *Rule
	TargetPath string
	TargetHost string
	AgentName  string
	Firing     bool
	Since      time.Time
	Value      float64
}

// Notifier delivers alerts. smokeng ships exactly one implementation, a
// webhook: notification channels are somebody else's job, and Alertmanager
// already does grouping, silencing and routing better than a monitoring tool
// would by reinventing them.
type Notifier interface {
	Notify(ctx context.Context, alerts []Alert) error
}

// Webhook posts alerts in Alertmanager's v2 format, so it can be pointed
// straight at an Alertmanager or at anything that speaks the same shape.
type Webhook struct {
	URL    string
	Client *http.Client
}

// amAlert mirrors Alertmanager's POST /api/v2/alerts payload.
type amAlert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt,omitempty"`
	EndsAt       string            `json:"endsAt,omitempty"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
}

func (w *Webhook) Notify(ctx context.Context, alerts []Alert) error {
	if len(alerts) == 0 {
		return nil
	}
	payload := make([]amAlert, 0, len(alerts))
	for _, a := range alerts {
		unit := "ms"
		if a.Rule.Metric == MetricLoss {
			unit = "%"
		}
		am := amAlert{
			Labels: map[string]string{
				"alertname": a.Rule.Name,
				"target":    a.TargetPath,
				"host":      a.TargetHost,
				"agent":     a.AgentName,
				"metric":    string(a.Rule.Metric),
				"severity":  "warning",
			},
			Annotations: map[string]string{
				"summary": fmt.Sprintf("%s on %s: %s is %.3g%s",
					a.Rule.Name, a.TargetPath, a.Rule.Metric, a.Value, unit),
				"description": fmt.Sprintf("Rule %q (%s) has been satisfied for %s.",
					a.Rule.Name, a.Rule.Describe(), a.TargetPath),
			},
		}
		if !a.Since.IsZero() {
			am.StartsAt = a.Since.UTC().Format(time.RFC3339)
		}
		// A firing alert carries no end: Alertmanager expires it on its own
		// resolve timeout if we stop repeating it, which is why firing alerts
		// are re-sent periodically rather than announced once.
		if !a.Firing {
			am.EndsAt = time.Now().UTC().Format(time.RFC3339)
		}
		payload = append(payload, am)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("alert: webhook %s returned %s", w.URL, resp.Status)
	}
	return nil
}
