package main

import (
	"encoding/json"
	"fmt"
	"os"

	"go.etcd.io/bbolt"
)

func main() {
	path := "data/xingta-live.db"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		fmt.Println("打开失败:", err)
		return
	}
	defer db.Close()

	var newsCount, actCount int
	var st struct {
		Source    string            `json:"source"`
		Known     map[string]string `json:"known"`
		Pending   map[string]bool   `json:"pending"`
		Fails     int               `json:"fails"`
		LastFull  string            `json:"lastSuccess"`
		LastError string            `json:"lastError"`
	}
	db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("sync"))
		if b != nil {
			b.ForEach(func(k, v []byte) error {
				json.Unmarshal(v, &st)
				return nil
			})
		}
		ns := tx.Bucket([]byte("news"))
		if ns != nil {
			ns.ForEach(func(k, v []byte) error { newsCount++; return nil })
		}
		act := tx.Bucket([]byte("activity"))
		if act != nil {
			act.ForEach(func(k, v []byte) error { actCount++; return nil })
		}
		return nil
	})
	fmt.Printf("news 条目: %d, activity 投影: %d\n", newsCount, actCount)
	fmt.Printf("已知公告: %d, 待确认: %d, 失败: %d\n", len(st.Known), len(st.Pending), st.Fails)
	fmt.Printf("LastSuccess: %s, LastError: %s\n", st.LastFull, st.LastError)
}
