package api

import (
	"net/http"
	"testing"
)

// graph_series accepts an array as well as a string, and neither form may skip
// the validation the other gets. Nothing exercised the array branch through the
// handler, so a regression that mis-joined it — a wrong separator, a silently
// dropped element — would still have produced a syntactically valid string and
// passed unnoticed.
func TestGraphSeriesPatchForms(t *testing.T) {
	h, _ := newTestServer(t)
	code, leaf := do(t, h, "POST", "/api/v1/targets", map[string]any{
		"parent_id": 1, "name": "irtt", "host": "1.1.1.1", "address_family": "v4",
		"settings": map[string]any{"probe_type": "irtt"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, leaf)
	}
	id := int64(leaf["id"].(float64))
	path := "/api/v1/targets/" + itoa(id)

	// Inherited from the root until set here.
	if got := setting(t, leaf, "graph_series")["effective"]; got != "all" {
		t.Errorf("inherited graph_series = %v, want all", got)
	}

	for _, c := range []struct {
		name string
		send any
		want string
	}{
		{"array", []string{"ipdv_send", "ipdv_receive"}, "ipdv_send ipdv_receive"},
		{"string", "ipdv_receive", "ipdv_receive"},
		// An empty array is not null: it selects none, locally, rather than
		// falling back to what the parent says.
		{"empty array", []string{}, ""},
	} {
		code, body := do(t, h, "PATCH", path, map[string]any{
			"settings": map[string]any{"graph_series": c.send},
		})
		if code != http.StatusOK {
			t.Fatalf("%s: %d %v", c.name, code, body)
		}
		s := setting(t, body, "graph_series")
		if s["effective"] != c.want {
			t.Errorf("%s: effective = %v, want %q", c.name, s["effective"], c.want)
		}
		if s["local"] != c.want {
			t.Errorf("%s: local = %v, want %q (it was set here)", c.name, s["local"], c.want)
		}
	}

	// null clears the local value and inheritance resumes.
	code, body := do(t, h, "PATCH", path, map[string]any{
		"settings": map[string]any{"graph_series": nil},
	})
	if code != http.StatusOK {
		t.Fatalf("clear: %d %v", code, body)
	}
	if got := setting(t, body, "graph_series")["effective"]; got != "all" {
		t.Errorf("after clearing, effective = %v, want the inherited all", got)
	}

	// A typo is refused through the array branch too, not only the string one.
	code, body = do(t, h, "PATCH", path, map[string]any{
		"settings": map[string]any{"graph_series": []string{"ipdv_sideways"}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("an unknown series name was accepted through the array form: %d %v", code, body)
	}
	if msg, _ := body["error"].(string); !contains(msg, "ipdv_sideways") {
		t.Errorf("error does not name the offending value: %v", body["error"])
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
