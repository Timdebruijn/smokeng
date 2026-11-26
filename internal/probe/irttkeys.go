package probe

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/timdebruijn/smokeng/internal/store"
)

// The HMAC key an irtt server requires belongs to the server, not to the
// smokeng target, so it is keyed on the endpoint the target is configured with
// (host:port as written, not the resolved address, so it survives DNS
// changes). It authenticates this prober to a server run with `--hmac`, closing
// the reflection/amplification and off-path spoofing an open UDP server
// invites. An endpoint with no key is probed without HMAC; if its server
// requires one, the session is refused and recorded as a flagged send failure.
//
// Keys never sit in the target tree, so they are not in the target list or the
// TOML export. They come from two places, merged into one map the probes read:
//
//   - the keyfile (--irtt-hmac-keys), loaded at start on the master or an
//     agent; and
//   - the database, set by a global admin through /api/v1/irtt-keys, master
//     only. Agents are not handed these.
//
// On the same endpoint the keyfile wins: it is the operator's word on the host.
var (
	irttKeyMu    sync.Mutex // serialises writers; readers use irttHMACKeys
	irttFileKeys map[string][]byte
	irttDBKeys   map[string][]byte
	irttHMACKeys atomic.Pointer[map[string][]byte] // the merge of the two
)

// ParseIRTTKey turns key text into the bytes irtt authenticates with, the way
// irtt parses `--hmac` itself (decodeHexOrNot): text starting 0x is hex, any
// other text is its own bytes. An empty key, or bad hex after 0x, is an error.
func ParseIRTTKey(s string) ([]byte, error) {
	if strings.HasPrefix(s, "0x") {
		b, err := hex.DecodeString(s[2:])
		if err != nil {
			return nil, fmt.Errorf("a key starting 0x must be valid hex: %w", err)
		}
		if len(b) == 0 {
			return nil, fmt.Errorf("empty HMAC key")
		}
		return b, nil
	}
	if s == "" {
		return nil, fmt.Errorf("empty HMAC key")
	}
	return []byte(s), nil
}

// mergeIRTTKeys installs file over db. Caller holds irttKeyMu.
func mergeIRTTKeys() {
	if len(irttFileKeys)+len(irttDBKeys) == 0 {
		irttHMACKeys.Store(nil)
		return
	}
	m := make(map[string][]byte, len(irttFileKeys)+len(irttDBKeys))
	for k, v := range irttDBKeys {
		m[k] = v
	}
	for k, v := range irttFileKeys {
		m[k] = v
	}
	irttHMACKeys.Store(&m)
}

func copyKeys(m map[string][]byte) map[string][]byte {
	cp := make(map[string][]byte, len(m))
	for k, v := range m {
		cp[k] = append([]byte(nil), v...)
	}
	return cp
}

// SetIRTTHMACKeys sets the keyfile part of the endpoint→key map every irtt
// probe authenticates with. A nil or empty map clears it. A copy is stored, so
// the caller may reuse or wipe the map and its values.
func SetIRTTHMACKeys(m map[string][]byte) {
	irttKeyMu.Lock()
	defer irttKeyMu.Unlock()
	irttFileKeys = copyKeys(m)
	mergeIRTTKeys()
}

// IRTTKeyfileHas reports whether the keyfile holds a key for the endpoint.
func IRTTKeyfileHas(endpoint string) bool {
	irttKeyMu.Lock()
	defer irttKeyMu.Unlock()
	_, ok := irttFileKeys[endpoint]
	return ok
}

// IRTTKeyLister is the part of the store RefreshIRTTKeys reads.
type IRTTKeyLister interface {
	ListIRTTKeys(ctx context.Context) ([]store.IRTTKey, error)
}

// RefreshIRTTKeys re-reads the database keys and re-merges. A row that does
// not parse is skipped and named (never its value); the rest still apply. On a
// read error the previous database keys stay in force.
func RefreshIRTTKeys(ctx context.Context, st IRTTKeyLister) error {
	rows, err := st.ListIRTTKeys(ctx)
	if err != nil {
		return err
	}
	db := make(map[string][]byte, len(rows))
	for _, r := range rows {
		b, err := ParseIRTTKey(r.Key)
		if err != nil {
			log.Printf("probe: irtt key for %s ignored: %v", r.Endpoint, err)
			continue
		}
		db[r.Endpoint] = b
	}
	irttKeyMu.Lock()
	defer irttKeyMu.Unlock()
	irttDBKeys = db
	mergeIRTTKeys()
	return nil
}

// irttKeyFor returns the HMAC key for an endpoint, or nil if none is configured.
func irttKeyFor(endpoint string) []byte {
	m := irttHMACKeys.Load()
	if m == nil {
		return nil
	}
	return (*m)[endpoint]
}
