// Package storetest 提供 store.Doc 的内存实现与契约测试，供功能包当假件用。
// 功能包的测试不该真开 bbolt：慢、且会把存储实现的细节渗进功能的断言里。
package storetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"xingta/internal/store"
)

// MemDoc 是 store.Doc 的内存实现。Batch 语义与 bbolt 版一致：
// fn 返回非 nil 则整批丢弃；并发写者被串行化（bbolt 的写事务也是这样排队的），
// 否则两个写者各自复制一份再换回，会互相抹掉对方的写入。
type MemDoc struct {
	mu     sync.Mutex // 保护 ns 与 closed
	wmu    sync.Mutex // 串行化写者
	ns     map[string]map[string][]byte
	closed bool
}

var errClosed = errors.New("storetest: closed")

func NewMem() *MemDoc {
	return &MemDoc{ns: map[string]map[string][]byte{}}
}

func (m *MemDoc) Put(ns, key string, v any) error {
	return m.Batch(func(b store.Batch) error { return b.Put(ns, key, v) })
}

func (m *MemDoc) Get(ns, key string, v any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false, errClosed
	}
	raw, ok := m.ns[ns][key]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, fmt.Errorf("storetest: 解码 %s/%s: %w", ns, key, err)
	}
	return true, nil
}

func (m *MemDoc) Scan(ns, prefix string, fn func(key string, raw []byte) error) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errClosed
	}
	keys := make([]string, 0, len(m.ns[ns]))
	for k := range m.ns[ns] {
		if bytes.HasPrefix([]byte(k), []byte(prefix)) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	snapshot := make([][]byte, len(keys))
	for i, k := range keys {
		snapshot[i] = append([]byte(nil), m.ns[ns][k]...)
	}
	m.mu.Unlock()

	for i, k := range keys {
		if err := fn(k, snapshot[i]); err != nil {
			return err
		}
	}
	return nil
}

func (m *MemDoc) Delete(ns, key string) error {
	return m.Batch(func(b store.Batch) error { return b.Delete(ns, key) })
}

func (m *MemDoc) Batch(fn func(b store.Batch) error) error {
	m.wmu.Lock()
	defer m.wmu.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errClosed
	}
	work := map[string]map[string][]byte{}
	for n, kv := range m.ns {
		cp := make(map[string][]byte, len(kv))
		for k, v := range kv {
			cp[k] = append([]byte(nil), v...)
		}
		work[n] = cp
	}
	m.mu.Unlock()

	if err := fn(memBatch{m: work}); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errClosed
	}
	m.ns = work
	return nil
}

func (m *MemDoc) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

type memBatch struct{ m map[string]map[string][]byte }

func (b memBatch) Put(ns, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("storetest: 编码 %s/%s: %w", ns, key, err)
	}
	if b.m[ns] == nil {
		b.m[ns] = map[string][]byte{}
	}
	b.m[ns][key] = raw
	return nil
}

func (b memBatch) Get(ns, key string, v any) (bool, error) {
	raw, ok := b.m[ns][key]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, fmt.Errorf("storetest: 解码 %s/%s: %w", ns, key, err)
	}
	return true, nil
}

func (b memBatch) Scan(ns, prefix string, fn func(key string, raw []byte) error) error {
	keys := make([]string, 0, len(b.m[ns]))
	for k := range b.m[ns] {
		if bytes.HasPrefix([]byte(k), []byte(prefix)) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := fn(k, append([]byte(nil), b.m[ns][k]...)); err != nil {
			return err
		}
	}
	return nil
}

func (b memBatch) Delete(ns, key string) error {
	delete(b.m[ns], key)
	return nil
}
