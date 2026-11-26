package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/auth"
	"github.com/timdebruijn/smokeng/internal/probe"
)

func irttCall(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

func TestIRTTKeysAPI(t *testing.T) {
	st := seededStore(t)
	t.Cleanup(func() {
		probe.SetIRTTHMACKeys(nil)
		st.DeleteIRTTKey(context.Background(), "[::1]:2112")
		probe.RefreshIRTTKeys(context.Background(), st)
	})
	probe.SetIRTTHMACKeys(map[string][]byte{"file.example:2112": []byte("from-file")})

	as := func(s *auth.Session) http.Handler {
		return New(st, Options{Alerts: alert.NewManager(st, nil), Auth: &fakeAuth{session: s}}, fstest.MapFS{})
	}
	admin := as(&auth.Session{Subject: "a", Role: auth.RoleAdmin, Expires: 1 << 40})
	viewer := as(&auth.Session{Subject: "v", Role: auth.RoleViewer, Expires: 1 << 40})
	esc := func(ep string) string { return "/api/v1/irtt-keys/" + url.PathEscape(ep) }

	for _, m := range []string{"GET", "PUT", "DELETE"} {
		if code, _, _ := irttCall(t, viewer, m, esc("h.example:2112"), `{"generate":true}`); code != http.StatusForbidden {
			t.Errorf("viewer %s = %d, want 403", m, code)
		}
	}

	// Generate, then read back.
	code, body, _ := irttCall(t, admin, "PUT", esc("h.example:2112"), `{"generate":true}`)
	key, _ := body["key"].(string)
	if code != 200 || body["source"] != "db" || len(key) != 43 || strings.HasPrefix(key, "0x") {
		t.Fatalf("generate: %d %v", code, body)
	}
	code, body, _ = irttCall(t, admin, "GET", esc("h.example:2112"), "")
	if code != 200 || body["key"] != key || body["source"] != "db" || body["endpoint"] != "h.example:2112" {
		t.Fatalf("get: %d %v", code, body)
	}

	// A hand-set key replaces it; an IPv6 endpoint routes through the escape.
	if code, body, _ = irttCall(t, admin, "PUT", esc("[::1]:2112"), `{"key":"0xdeadbeef"}`); code != 200 || body["endpoint"] != "[::1]:2112" {
		t.Fatalf("put ipv6: %d %v", code, body)
	}

	// The keyfile wins: PUT is refused, GET names the source and no value.
	if code, _, _ = irttCall(t, admin, "PUT", esc("file.example:2112"), `{"key":"x"}`); code != http.StatusConflict {
		t.Errorf("put on keyfile endpoint = %d, want 409", code)
	}
	code, body, raw := irttCall(t, admin, "GET", esc("file.example:2112"), "")
	if code != 200 || body["source"] != "keyfile" || strings.Contains(raw, "from-file") || body["key"] != nil {
		t.Errorf("get keyfile endpoint: %d %s", code, raw)
	}

	// Bad input.
	for _, c := range []struct{ ep, body string }{
		{"nohost", `{"key":"x"}`},
		{"h:0", `{"key":"x"}`},
		{"h:70000", `{"key":"x"}`},
		{":2112", `{"key":"x"}`},
		{"h:2112", `{"key":"0xzz"}`},
		{"h:2112", `{}`},
		{"h:2112", `{"key":"x","generate":true}`},
		{"h:2112", `{"key":"` + strings.Repeat("a", 1025) + `"}`},
	} {
		if code, _, _ := irttCall(t, admin, "PUT", esc(c.ep), c.body); code != http.StatusBadRequest {
			t.Errorf("PUT %s %.30s = %d, want 400", c.ep, c.body, code)
		}
	}

	// Delete.
	if code, _, _ = irttCall(t, admin, "DELETE", esc("h.example:2112"), ""); code != http.StatusNoContent {
		t.Errorf("delete = %d", code)
	}
	if _, body, _ = irttCall(t, admin, "GET", esc("h.example:2112"), ""); body["source"] != "none" {
		t.Errorf("after delete: %v", body)
	}
}
