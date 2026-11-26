package store

import (
	"context"
	"testing"
	"time"
)

func TestIRTTKeyRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	now := time.Unix(1_800_000_000, 0)

	if _, ok, err := s.GetIRTTKey(ctx, "a:2112"); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	if err := s.PutIRTTKey(ctx, "a:2112", "one", "tim", now); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIRTTKey(ctx, "a:2112", "two", "ann", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIRTTKey(ctx, "[::1]:2112", "v6", "tim", now); err != nil {
		t.Fatal(err)
	}
	k, ok, err := s.GetIRTTKey(ctx, "a:2112")
	if err != nil || !ok || k.Key != "two" || k.UpdatedBy != "ann" || k.UpdatedAt != now.Add(time.Minute).Unix() {
		t.Fatalf("get after replace: %+v ok=%v err=%v", k, ok, err)
	}
	all, err := s.ListIRTTKeys(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("list: %+v err=%v", all, err)
	}
	if err := s.DeleteIRTTKey(ctx, "a:2112"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetIRTTKey(ctx, "a:2112"); ok {
		t.Fatal("still there after delete")
	}
	if err := s.PutIRTTKey(ctx, "a:2112", "", "x", now); err == nil {
		t.Fatal("empty key accepted")
	}
}
