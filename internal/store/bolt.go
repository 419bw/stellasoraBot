package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// boltDoc 是 Doc 的 bbolt 实现：ns 即 bucket，key 原样存，值是 JSON 字节。
// 不开 NoSync——手机断电场景下丢一次写入的代价远高于这点 fsync。
type boltDoc struct{ db *bolt.DB }

// OpenBolt 打开（或创建）path 处的数据库。返回 Doc 而不是具体类型：
// 引擎细节到此为止，上层（包括 main）只应持有接口。
func OpenBolt(path string) (Doc, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("store: 打开 %s: %w", path, err)
	}
	return &boltDoc{db: db}, nil
}

func (d *boltDoc) Put(ns, key string, v any) error {
	return d.Batch(func(b Batch) error { return b.Put(ns, key, v) })
}

func (d *boltDoc) Get(ns, key string, v any) (bool, error) {
	var found bool
	err := d.db.View(func(tx *bolt.Tx) error {
		var gerr error
		found, gerr = txGet(tx, ns, key, v)
		return gerr
	})
	return found, err
}

func (d *boltDoc) Scan(ns, prefix string, fn func(key string, raw []byte) error) error {
	return d.db.View(func(tx *bolt.Tx) error { return txScan(tx, ns, prefix, fn) })
}

func (d *boltDoc) Delete(ns, key string) error {
	return d.Batch(func(b Batch) error { return b.Delete(ns, key) })
}

func (d *boltDoc) Batch(fn func(b Batch) error) error {
	return d.db.Update(func(tx *bolt.Tx) error { return fn(boltBatch{tx: tx}) })
}

func (d *boltDoc) Close() error { return d.db.Close() }

type boltBatch struct{ tx *bolt.Tx }

func (b boltBatch) Put(ns, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: 编码 %s/%s: %w", ns, key, err)
	}
	bkt, err := b.tx.CreateBucketIfNotExists([]byte(ns))
	if err != nil {
		return err
	}
	return bkt.Put([]byte(key), raw)
}

func (b boltBatch) Get(ns, key string, v any) (bool, error) { return txGet(b.tx, ns, key, v) }

func (b boltBatch) Scan(ns, prefix string, fn func(key string, raw []byte) error) error {
	return txScan(b.tx, ns, prefix, fn)
}

func (b boltBatch) Delete(ns, key string) error {
	bkt := b.tx.Bucket([]byte(ns))
	if bkt == nil {
		return nil
	}
	return bkt.Delete([]byte(key))
}

func txGet(tx *bolt.Tx, ns, key string, v any) (bool, error) {
	bkt := tx.Bucket([]byte(ns))
	if bkt == nil {
		return false, nil
	}
	raw := bkt.Get([]byte(key))
	if raw == nil {
		return false, nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, fmt.Errorf("store: 解码 %s/%s: %w", ns, key, err)
	}
	return true, nil
}

func txScan(tx *bolt.Tx, ns, prefix string, fn func(key string, raw []byte) error) error {
	bkt := tx.Bucket([]byte(ns))
	if bkt == nil {
		return nil
	}
	p := []byte(prefix)
	c := bkt.Cursor()
	for k, v := c.Seek(p); k != nil && bytes.HasPrefix(k, p); k, v = c.Next() {
		// 拷贝出来：bolt 的 v 只在事务内有效，而契约要求 fn 拿到的字节可以随时保留。
		if err := fn(string(k), append([]byte(nil), v...)); err != nil {
			return err
		}
	}
	return nil
}
