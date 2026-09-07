package storetest

import (
	"errors"
	"testing"

	"xingta/internal/store"
)

type contractPayload struct {
	N      int
	Tags   []string
	Nested struct {
		S string
	}
}

// RunDocContract 把 Doc 契约跑一遍。任何 Doc 实现（bbolt、内存、将来的别的引擎）
// 都必须过这套，功能包才能放心只对着接口写代码。
func RunDocContract(t *testing.T, open func(t *testing.T) store.Doc) {
	t.Helper()

	t.Run("PutGet 往返", func(t *testing.T) {
		d := open(t)
		defer d.Close()
		in := contractPayload{N: 7, Tags: []string{"a", "b"}}
		in.Nested.S = "深"
		if err := d.Put("ns1", "k1", in); err != nil {
			t.Fatalf("Put: %v", err)
		}
		var out contractPayload
		ok, err := d.Get("ns1", "k1", &out)
		if err != nil || !ok {
			t.Fatalf("Get = %v, %v; 想要 true, nil", ok, err)
		}
		if out.N != 7 || len(out.Tags) != 2 || out.Nested.S != "深" {
			t.Errorf("往返后内容变了: %+v", out)
		}
	})

	t.Run("Get 不存在的键", func(t *testing.T) {
		d := open(t)
		defer d.Close()
		var out contractPayload
		ok, err := d.Get("nosuch", "nosuch", &out)
		if ok || err != nil {
			t.Errorf("Get 缺失键 = %v, %v; 想要 false, nil", ok, err)
		}
	})

	t.Run("Scan 前缀与字节序", func(t *testing.T) {
		d := open(t)
		defer d.Close()
		for _, k := range []string{"b1", "a2", "a1", "a10"} {
			if err := d.Put("ns", k, map[string]int{"k": 1}); err != nil {
				t.Fatalf("Put %s: %v", k, err)
			}
		}
		var got []string
		var kept [][]byte
		err := d.Scan("ns", "a", func(key string, raw []byte) error {
			got = append(got, key)
			kept = append(kept, raw) // 契约：raw 在 fn 返回后仍可用
			return nil
		})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		want := []string{"a1", "a10", "a2"} // 字节序，不是字典序
		if len(got) != len(want) {
			t.Fatalf("Scan 得到 %v, 想要 %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("第 %d 个键 = %s, 想要 %s", i, got[i], want[i])
			}
			if len(kept[i]) == 0 {
				t.Errorf("键 %s 的 raw 为空", got[i])
			}
		}
	})

	t.Run("Scan 的 fn 报错即中止", func(t *testing.T) {
		d := open(t)
		defer d.Close()
		for _, k := range []string{"a1", "a2", "a3"} {
			if err := d.Put("ns", k, 1); err != nil {
				t.Fatal(err)
			}
		}
		boom := errors.New("boom")
		seen := 0
		err := d.Scan("ns", "a", func(string, []byte) error {
			seen++
			return boom
		})
		if !errors.Is(err, boom) {
			t.Errorf("Scan err = %v, 想要原样返回 fn 的错误", err)
		}
		if seen != 1 {
			t.Errorf("fn 被调了 %d 次, 想要 1 次就中止", seen)
		}
	})

	t.Run("Delete 后再 Get", func(t *testing.T) {
		d := open(t)
		defer d.Close()
		if err := d.Put("ns", "k", 1); err != nil {
			t.Fatal(err)
		}
		if err := d.Delete("ns", "k"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := d.Delete("ns", "k"); err != nil {
			t.Errorf("Delete 不存在的键报错: %v", err)
		}
		var out int
		if ok, _ := d.Get("ns", "k", &out); ok {
			t.Error("删除后仍能 Get 到")
		}
	})

	t.Run("命名空间隔离", func(t *testing.T) {
		d := open(t)
		defer d.Close()
		if err := d.Put("alpha", "same", 1); err != nil {
			t.Fatal(err)
		}
		if err := d.Put("beta", "same", 2); err != nil {
			t.Fatal(err)
		}
		var a, b int
		if ok, _ := d.Get("alpha", "same", &a); !ok || a != 1 {
			t.Errorf("alpha/same = %d, %v", a, ok)
		}
		if ok, _ := d.Get("beta", "same", &b); !ok || b != 2 {
			t.Errorf("beta/same = %d, %v", b, ok)
		}
	})

	t.Run("Batch 内可见自己的写", func(t *testing.T) {
		d := open(t)
		defer d.Close()
		err := d.Batch(func(b store.Batch) error {
			if err := b.Put("ns", "k", 42); err != nil {
				return err
			}
			var v int
			ok, err := b.Get("ns", "k", &v)
			if err != nil || !ok || v != 42 {
				t.Errorf("事务内读自己的写 = %d, %v, %v", v, ok, err)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("Batch: %v", err)
		}
	})

	t.Run("Batch 出错全部回滚", func(t *testing.T) {
		d := open(t)
		defer d.Close()
		boom := errors.New("boom")
		err := d.Batch(func(b store.Batch) error {
			if err := b.Put("ns", "first", 1); err != nil {
				return err
			}
			if err := b.Put("ns", "second", 2); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("Batch err = %v, 想要 %v", err, boom)
		}
		var v int
		for _, k := range []string{"first", "second"} {
			if ok, _ := d.Get("ns", k, &v); ok {
				t.Errorf("回滚失败：%s 仍可读", k)
			}
		}
	})
}
