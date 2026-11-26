package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/timdebruijn/smokeng/internal/probe"
)

// maxIRTTKeyLen bounds the key text a caller may store.
const maxIRTTKeyLen = 1024

// irttEndpoint validates the {endpoint} path value ("host:port", IPv6 as
// "[::1]:2112") and returns it in the form probes look it up by. PathValue has
// already URL-unescaped it.
func irttEndpoint(r *http.Request) (string, error) {
	host, port, err := net.SplitHostPort(r.PathValue("endpoint"))
	if err != nil || host == "" {
		return "", fmt.Errorf("endpoint must be host:port, with IPv6 written as [::1]:2112")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("endpoint port must be 1-65535")
	}
	return net.JoinHostPort(host, strconv.Itoa(p)), nil
}

// irttKeyBody is the one shape every irtt-keys response has. Key is set only
// when the source is the database: a keyfile value never leaves the host.
type irttKeyBody struct {
	Endpoint string `json:"endpoint"`
	Source   string `json:"source"` // "db", "keyfile" or "none"
	Key      string `json:"key,omitempty"`
}

func (s *server) irttKeyState(r *http.Request, endpoint string) (irttKeyBody, error) {
	if probe.IRTTKeyfileHas(endpoint) {
		return irttKeyBody{Endpoint: endpoint, Source: "keyfile"}, nil
	}
	k, ok, err := s.st.GetIRTTKey(r.Context(), endpoint)
	if err != nil {
		return irttKeyBody{}, err
	}
	if !ok {
		return irttKeyBody{Endpoint: endpoint, Source: "none"}, nil
	}
	return irttKeyBody{Endpoint: endpoint, Source: "db", Key: k.Key}, nil
}

func (s *server) handleGetIRTTKey(w http.ResponseWriter, r *http.Request) {
	endpoint, err := irttEndpoint(r)
	if err != nil {
		badRequest(w, err)
		return
	}
	body, err := s.irttKeyState(r, endpoint)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *server) handlePutIRTTKey(w http.ResponseWriter, r *http.Request) {
	endpoint, err := irttEndpoint(r)
	if err != nil {
		badRequest(w, err)
		return
	}
	var req struct {
		Key      string `json:"key"`
		Generate bool   `json:"generate"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		badRequestMsg(w, "malformed request body")
		return
	}
	if req.Generate == (req.Key != "") {
		badRequestMsg(w, `send either {"key":"…"} or {"generate":true}`)
		return
	}
	if req.Generate {
		// 32 random bytes, URL-safe base64: 43 characters, never starting 0x,
		// so irtt reads it as raw text and the same string works on the server.
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			internalError(w, err)
			return
		}
		req.Key = base64.RawURLEncoding.EncodeToString(raw)
	}
	if len(req.Key) > maxIRTTKeyLen {
		badRequestMsg(w, fmt.Sprintf("key is longer than %d bytes", maxIRTTKeyLen))
		return
	}
	if _, err := probe.ParseIRTTKey(req.Key); err != nil {
		badRequest(w, err)
		return
	}
	if probe.IRTTKeyfileHas(endpoint) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this endpoint has a key in the keyfile on the host, which wins over one set here; change it there",
		})
		return
	}
	by := s.callerName(r)
	if by == "" {
		by = "unknown"
	}
	if err := s.st.PutIRTTKey(r.Context(), endpoint, req.Key, by, time.Now()); err != nil {
		internalError(w, err)
		return
	}
	s.refreshIRTTKeys(r)
	writeJSON(w, http.StatusOK, irttKeyBody{Endpoint: endpoint, Source: "db", Key: req.Key})
}

func (s *server) handleDeleteIRTTKey(w http.ResponseWriter, r *http.Request) {
	endpoint, err := irttEndpoint(r)
	if err != nil {
		badRequest(w, err)
		return
	}
	if err := s.st.DeleteIRTTKey(r.Context(), endpoint); err != nil {
		internalError(w, err)
		return
	}
	s.refreshIRTTKeys(r)
	w.WriteHeader(http.StatusNoContent)
}

// refreshIRTTKeys makes the change take effect now rather than at the engine's
// next reload. The write has already succeeded, so a failure here is logged and
// the periodic reload picks it up.
func (s *server) refreshIRTTKeys(r *http.Request) {
	if err := probe.RefreshIRTTKeys(r.Context(), s.st); err != nil {
		log.Printf("api: refresh irtt keys: %v", err)
	}
}
