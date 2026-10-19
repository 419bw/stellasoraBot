package annsync

import (
	"reflect"
	"strings"
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

// ReadActiveRecs 是海报出图的取数面：底单 + 人工覆盖。管理员 hide 掉的活动不许
// 只从文字日历消失、却照样画进海报；改过的时间标题两边必须同时生效。同一条用例
// 也钉住另一半——底单永远不被覆盖改写（calops 的「原解析」读的是它）。
func TestReadActiveRecsAppliesOverrides(t *testing.T) {
	t1 := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(24 * time.Hour)
	t3 := t2.Add(24 * time.Hour)

	mk := func(id, title string, s, e time.Time, status string) Rec {
		src := id
		if i := strings.IndexByte(id, ':'); i >= 0 {
			src = id[:i]
		}
		return Rec{ID: id, SourceID: src, Title: title, Status: status, Start: s, End: e}
	}

	cases := []struct {
		name  string
		query string // 传给 ReadActiveRecs 的源；"" = 所有源
		recs  []Rec
		ovs   map[string]Override

		want    []string // 生效态里的 ID（按序）
		wantRaw []string // 底单里的 ID（按序）

		check    func(t *testing.T, got []Rec) // 生效态字段
		checkRaw func(t *testing.T, raw []Rec) // 底单字段
	}{
		{
			name:  "改时间与改标题都算进去",
			query: "src",
			recs:  []Rec{mk("src:a1", "招募A", t1, t2, StatusOK)},
			ovs:   map[string]Override{"src:a1": {Title: "招募A（改期）", Start: t2, End: t3}},
			want:  []string{"src:a1"}, wantRaw: []string{"src:a1"},
			check: func(t *testing.T, got []Rec) {
				if got[0].Title != "招募A（改期）" || !got[0].Start.Equal(t2) || !got[0].End.Equal(t3) {
					t.Errorf("生效态 = %+v，want 标题与窗口都是覆盖后的值", got[0])
				}
			},
			checkRaw: func(t *testing.T, raw []Rec) {
				if raw[0].Title != "招募A" || !raw[0].Start.Equal(t1) || !raw[0].End.Equal(t2) {
					t.Errorf("底单被覆盖改写了：%+v，want 仍是官网原值", raw[0])
				}
			},
		},
		{
			name:  "hide 的撤掉",
			query: "src",
			recs:  []Rec{mk("src:a2", "販售B", t1, t2, StatusOK)},
			ovs:   map[string]Override{"src:a2": {Hide: true}},
			want:  nil, wantRaw: []string{"src:a2"},
		},
		{
			name:  "pending 不进生效态",
			query: "src",
			recs:  []Rec{mk("src:a3", "待确认C", t1, t2, StatusPending)},
			want:  nil, wantRaw: []string{"src:a3"},
		},
		{
			name:  "覆盖写坏的窗口不显示",
			query: "src",
			recs:  []Rec{mk("src:a4", "倒挂D", t1, t2, StatusOK)},
			ovs:   map[string]Override{"src:a4": {Start: t3, End: t1}},
			want:  nil, wantRaw: []string{"src:a4"},
		},
		{
			name:  "三元组重复只留一条",
			query: "src",
			recs: []Rec{
				mk("src:a5", "同窗活动", t1, t2, StatusOK),
				mk("src:a6", "同窗活动", t1, t2, StatusOK),
			},
			want: []string{"src:a5"}, wantRaw: []string{"src:a5", "src:a6"},
		},
		{
			name:  "跨源隔离：只问一个源",
			query: "srcA",
			recs: []Rec{
				mk("srcA:a1", "甲", t1, t2, StatusOK),
				mk("srcB:b1", "乙", t1, t2, StatusOK),
			},
			want: []string{"srcA:a1"}, wantRaw: []string{"srcA:a1"},
		},
		{
			name:  "源名为空读所有源",
			query: "",
			recs: []Rec{
				mk("srcA:a1", "甲", t1, t2, StatusOK),
				mk("srcB:b1", "乙", t1, t2, StatusOK),
			},
			want: []string{"srcA:a1", "srcB:b1"}, wantRaw: []string{"srcA:a1", "srcB:b1"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := storetest.NewMem()
			for _, r := range c.recs {
				if err := doc.Put(nsActivity, r.ID, r); err != nil {
					t.Fatal(err)
				}
			}
			for id, o := range c.ovs {
				if err := doc.Put(overrideNS, id, o); err != nil {
					t.Fatal(err)
				}
			}

			got, err := ReadActiveRecs(doc, c.query)
			if err != nil {
				t.Fatalf("ReadActiveRecs(%q): %v", c.query, err)
			}
			if ids := idsOfRecs(got); !reflect.DeepEqual(ids, c.want) {
				t.Errorf("生效态 ID = %v, want %v", ids, c.want)
			}
			if c.check != nil {
				c.check(t, got)
			}

			raw, err := ReadRecs(doc, c.query)
			if err != nil {
				t.Fatalf("ReadRecs(%q): %v", c.query, err)
			}
			if ids := idsOfRecs(raw); !reflect.DeepEqual(ids, c.wantRaw) {
				t.Errorf("底单 ID = %v, want %v", ids, c.wantRaw)
			}
			if c.checkRaw != nil {
				c.checkRaw(t, raw)
			}
		})
	}
}

func idsOfRecs(recs []Rec) []string {
	if len(recs) == 0 {
		return nil // 空与 nil 在这类断言里必须可区分，见 reflect.DeepEqual 的语义
	}
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.ID)
	}
	return out
}
