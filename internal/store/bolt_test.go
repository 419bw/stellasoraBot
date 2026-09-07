package store_test

import (
	"path/filepath"
	"testing"

	"xingta/internal/store"
	"xingta/internal/store/storetest"
)

func openTempBolt(t *testing.T) store.Doc {
	t.Helper()
	d, err := store.OpenBolt(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	return d
}

func TestBoltDocContract(t *testing.T) {
	storetest.RunDocContract(t, func(t *testing.T) store.Doc {
		t.Helper()
		d := openTempBolt(t)
		t.Cleanup(func() { d.Close() })
		return d
	})
}

func TestBoltReopenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keep.db")
	d, err := store.OpenBolt(path)
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	if err := d.Put("ns", "k", map[string]int{"n": 3}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	d2, err := store.OpenBolt(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	var out map[string]int
	ok, err := d2.Get("ns", "k", &out)
	if err != nil || !ok || out["n"] != 3 {
		t.Errorf("重开后 Get = %v, %v, %v", out, ok, err)
	}
}

func TestBoltClosedDocRejectsCalls(t *testing.T) {
	d := openTempBolt(t)
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := d.Put("ns", "k", 1); err == nil {
		t.Error("Close 之后 Put 仍成功")
	}
	var out int
	if _, err := d.Get("ns", "k", &out); err == nil {
		t.Error("Close 之后 Get 仍成功")
	}
}
