package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/timdebruijn/smokeng/internal/probe"
)

// The keyfile reads a key the way `irtt server --hmac` does, so a 0x key is
// hex; an invalid one stops the start rather than silently never matching.
func TestIRTTKeyfileDecodesHexKeys(t *testing.T) {
	t.Cleanup(func() { probe.SetIRTTHMACKeys(nil) })
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "keys.toml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := loadIRTTHMACKeys(write("\"a:2112\" = \"0xdeadbeef\"\n\"b:2112\" = \"plain\"\n")); err != nil {
		t.Fatal(err)
	}
	if !probe.IRTTKeyfileHas("a:2112") || !probe.IRTTKeyfileHas("b:2112") {
		t.Error("keys not loaded")
	}
	for _, bad := range []string{"\"a:2112\" = \"0xzz\"\n", "\"a:2112\" = \"\"\n"} {
		if err := loadIRTTHMACKeys(write(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
