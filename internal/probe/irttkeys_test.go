package probe

import (
	"bytes"
	"context"
	"testing"

	"github.com/timdebruijn/smokeng/internal/store"
)

func TestParseIRTTKey(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    []byte
		wantErr bool
	}{
		{"secret", []byte("secret"), false},
		{"0xdeadBEEF", []byte{0xde, 0xad, 0xbe, 0xef}, false},
		{"0Xab", []byte("0Xab"), false}, // irtt only knows lower-case 0x
		{"0x", nil, true},
		{"0xzz", nil, true},
		{"0xabc", nil, true}, // odd length
		{"", nil, true},
	} {
		got, err := ParseIRTTKey(c.in)
		if (err != nil) != c.wantErr || !bytes.Equal(got, c.want) {
			t.Errorf("ParseIRTTKey(%q) = %x, %v; want %x, err=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

type fakeKeyLister []store.IRTTKey

func (f fakeKeyLister) ListIRTTKeys(context.Context) ([]store.IRTTKey, error) { return f, nil }

func TestIRTTKeyMergeKeyfileWins(t *testing.T) {
	t.Cleanup(func() {
		SetIRTTHMACKeys(nil)
		RefreshIRTTKeys(context.Background(), fakeKeyLister(nil))
	})
	SetIRTTHMACKeys(map[string][]byte{"a:2112": []byte("file"), "f:2112": []byte("only-file")})
	err := RefreshIRTTKeys(context.Background(), fakeKeyLister{
		{Endpoint: "a:2112", Key: "db"},
		{Endpoint: "d:2112", Key: "0x01ff"},
		{Endpoint: "bad:2112", Key: "0xzz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for ep, want := range map[string][]byte{
		"a:2112": []byte("file"), "f:2112": []byte("only-file"),
		"d:2112": {0x01, 0xff}, "bad:2112": nil, "none:2112": nil,
	} {
		if got := irttKeyFor(ep); !bytes.Equal(got, want) {
			t.Errorf("irttKeyFor(%s) = %x, want %x", ep, got, want)
		}
	}
	if !IRTTKeyfileHas("a:2112") || IRTTKeyfileHas("d:2112") {
		t.Error("IRTTKeyfileHas wrong")
	}
	// Dropping the db rows leaves the keyfile; re-setting the keyfile keeps the db part.
	RefreshIRTTKeys(context.Background(), fakeKeyLister(nil))
	if irttKeyFor("d:2112") != nil || string(irttKeyFor("a:2112")) != "file" {
		t.Error("refresh with no db rows broke the merge")
	}
}
