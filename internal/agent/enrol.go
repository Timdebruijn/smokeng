package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// IDPath is where an agent remembers the id enrolment gave it, so a token is
// needed once rather than on every start.
func IDPath(keyPath string) string { return keyPath + ".id" }

// LoadAgentID reads a previously enrolled id. The bool reports whether there
// was one.
func LoadAgentID(keyPath string) (int64, bool, error) {
	data, err := os.ReadFile(IDPath(keyPath))
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("agent: %s does not contain an agent id", IDPath(keyPath))
	}
	return id, true, nil
}

func saveAgentID(keyPath string, id int64) error {
	return os.WriteFile(IDPath(keyPath), []byte(strconv.FormatInt(id, 10)+"\n"), 0o600)
}

// Enrol exchanges a one-time token for an agent id (DESIGN.md §9b). The token
// is the only credential on this request, which is why a plain-HTTP master is
// refused here as firmly as it is for the signed endpoints: this is the
// request that would leak something usable.
func Enrol(ctx context.Context, master, token string, key ed25519.PrivateKey, insecure bool) (int64, string, error) {
	// A loopback master is exempt for the same reason agent.New is: the token
	// is a credential, but over a literal loopback address it never reaches a
	// network interface, so there is nothing to intercept it. Without this the
	// token path was stricter than the pull/push path and than this flag's own
	// help text, which already says loopback needs no flag — so `agent run
	// --master http://127.0.0.1:8080 --token …`, the ordinary same-host
	// enrolment, failed where the --agent-id form on the same URL succeeded.
	if !strings.HasPrefix(master, "https://") && !insecure && !masterIsLoopback(master) {
		return 0, "", fmt.Errorf("agent: refusing to send an enrolment token to %q over plain HTTP "+
			"that is not on loopback; pass --insecure-allow-http only for local development", master)
	}
	body, err := json.Marshal(map[string]string{
		"token":  token,
		"pubkey": PublicKey(key),
	})
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(master, "/")+"/api/v1/agent/enrol", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode != http.StatusCreated {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return 0, "", fmt.Errorf("agent: enrolment refused: %s", e.Error)
	}
	var out struct {
		AgentID int64  `json:"agent_id"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, "", fmt.Errorf("agent: master returned an unreadable enrolment response: %w", err)
	}
	if out.AgentID == 0 {
		return 0, "", fmt.Errorf("agent: master returned no agent id")
	}
	return out.AgentID, out.Name, nil
}

// EnrolOnce enrols only if this key has not been enrolled before, so a service
// that restarts with --token in its unit file does not try to spend a token it
// already spent.
func EnrolOnce(ctx context.Context, master, token, keyPath string, key ed25519.PrivateKey, insecure bool) (int64, error) {
	if id, ok, err := LoadAgentID(keyPath); err != nil {
		return 0, err
	} else if ok {
		return id, nil
	}
	id, name, err := Enrol(ctx, master, token, key, insecure)
	if err != nil {
		return 0, err
	}
	if err := saveAgentID(keyPath, id); err != nil {
		// The agent exists on the master now; losing the id locally would mean
		// enrolling a second one under a name that is already taken.
		return 0, fmt.Errorf("agent: enrolled as %q with id %d, but could not record it: %w",
			name, id, err)
	}
	return id, nil
}
