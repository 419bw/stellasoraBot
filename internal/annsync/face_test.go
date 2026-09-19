package annsync

import (
	"testing"
	"time"

	"xingta/internal/store/storetest"
)

// 只读面（StatusReader / ReadRec / ReadStatus）是 calquery、calops 与 main 之间
// 唯一的接缝：这些读错一个字段，"日历同步中"的提示就会永久说谎。

func TestStatusReaderDelegatesToReadStatus(t *testing.T) {
	doc := storetest.NewMem()

	st, err := NewStatusReader(doc, "src").Status()
	if err != nil {
		t.Fatalf("空库读状态: %v", err)
	}
	if st.Source != "src" || st.Known != 0 || !st.LastSuccess.IsZero() {
		t.Fatalf("空库状态应只有源名，got %+v", st)
	}

	ts := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	if err := doc.Put(nsSync, "src:meta", state{
		Source:      "src",
		Known:       map[string]string{"r1": "h1", "r2": "h2"},
		Pending:     map[string]bool{"r2": true},
		Fails:       3,
		LastSuccess: ts,
		LastError:   "拉取超时",
	}); err != nil {
		t.Fatal(err)
	}

	st, err = NewStatusReader(doc, "src").Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Known != 2 || st.PendingRefs != 1 || st.Fails != 3 ||
		!st.LastSuccess.Equal(ts) || st.LastError != "拉取超时" {
		t.Errorf("回读状态 = %+v，与写入不符", st)
	}
}

func TestReadRecAndPendingPartition(t *testing.T) {
	doc := storetest.NewMem()
	t1 := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(24 * time.Hour)

	if err := doc.Put(nsActivity, "src:a2", Rec{ID: "src:a2", SourceID: "src", Status: StatusOK, Start: t2}); err != nil {
		t.Fatal(err)
	}
	if err := doc.Put(nsActivity, "src:a1", Rec{ID: "src:a1", SourceID: "src", Status: StatusPending, Start: t1}); err != nil {
		t.Fatal(err)
	}

	r, ok, err := ReadRec(doc, "src:a1")
	if err != nil || !ok || r.Start != t1 {
		t.Fatalf("ReadRec 命中失败: ok=%v err=%v r=%+v", ok, err, r)
	}
	if _, ok, err := ReadRec(doc, "src:missing"); err != nil || ok {
		t.Errorf("不存在的键: ok=%v err=%v，want false/nil", ok, err)
	}

	all, err := ReadRecs(doc, "src")
	if err != nil || len(all) != 2 || all[0].ID != "src:a1" {
		t.Fatalf("ReadRecs = %+v err=%v，want 按 Start 升序两条", all, err)
	}
	// 跨源隔离：另一个源的记录不得混进这个源的视图
	if err := doc.Put(nsActivity, "other:b1", Rec{ID: "other:b1", SourceID: "other", Status: StatusOK, Start: t1}); err != nil {
		t.Fatal(err)
	}
	if all, err = ReadRecs(doc, "src"); err != nil || len(all) != 2 {
		t.Errorf("加了对端后 ReadRecs(src) = %d 条 err=%v，want 仍是 2", len(all), err)
	}

	pend, err := ReadPending(doc, "src")
	if err != nil || len(pend) != 1 || pend[0].ID != "src:a1" {
		t.Errorf("ReadPending = %+v err=%v，want 只有待确认的 a1", pend, err)
	}
}

func TestReadStatusPropagatesCorruptBytes(t *testing.T) {
	doc := storetest.NewMem()
	if err := doc.Put(nsSync, "bad:meta", "不是 state 的 JSON"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStatus(doc, "bad"); err == nil {
		t.Error("坏字节被静默当成了空状态：必须报错，否则同步状态面板会假装健康")
	}
}
