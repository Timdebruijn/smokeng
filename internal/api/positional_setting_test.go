package api

import (
	"net/http"
	"testing"
)

// The targets API takes and returns positional_loss with the usual provenance:
// inherited from the root until set here, local once set, inheritance again
// after null, and a bad value is a 400 rather than a stored typo.
func TestPositionalLossThroughTheAPI(t *testing.T) {
	h, _ := newTestServer(t)
	code, grp := do(t, h, "POST", "/api/v1/targets", map[string]any{"parent_id": 1, "name": "lab"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, grp)
	}
	s := setting(t, grp, "positional_loss")
	if s["effective"] != "exclude" || s["local"] != nil {
		t.Errorf("inherited = %v, want exclude with no local value", s)
	}
	src, _ := s["source"].(map[string]any)
	if src == nil || src["path"] != "/" {
		t.Errorf("source = %v, want the root", s["source"])
	}
	path := "/api/v1/targets/" + itoa(int64(grp["id"].(float64)))

	code, body := do(t, h, "PATCH", path, map[string]any{"settings": map[string]any{"positional_loss": "flag_only"}})
	if code != http.StatusOK {
		t.Fatalf("patch: %d %v", code, body)
	}
	s = setting(t, body, "positional_loss")
	if s["effective"] != "flag_only" || s["local"] != "flag_only" || s["source"] != "local" {
		t.Errorf("after set = %v, want flag_only, local", s)
	}

	code, body = do(t, h, "PATCH", path, map[string]any{"settings": map[string]any{"positional_loss": nil}})
	if code != http.StatusOK {
		t.Fatalf("clear: %d %v", code, body)
	}
	if got := setting(t, body, "positional_loss")["effective"]; got != "exclude" {
		t.Errorf("after clearing = %v, want the inherited exclude", got)
	}

	code, body = do(t, h, "PATCH", path, map[string]any{"settings": map[string]any{"positional_loss": "never"}})
	if code != http.StatusBadRequest {
		t.Errorf("a bad value was accepted: %d %v", code, body)
	}
}
