package annsync

// 白盒例外：退避曲线、增量挑选、投影这三个纯函数是引擎的核心判断，
// 而它们刻意不导出——导出只会把基础设施的公开面撑大，功能插件并不需要它们。

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func refOf(id string) Ref { return Ref{ID: id, Hash: sha256.Sum256([]byte(id))} }

func hexOf(r Ref) string { return hex.EncodeToString(r.Hash[:]) }

func refIDs(list []Ref) []string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r.ID)
	}
	return out
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	cases := []struct {
		fails    int
		base     time.Duration
		interval time.Duration
		want     time.Duration
	}{
		{0, time.Minute, 30 * time.Minute, time.Minute}, // 0 当 1 处理：第一次失败也要退避
		{1, time.Minute, 30 * time.Minute, time.Minute},
		{2, time.Minute, 30 * time.Minute, 2 * time.Minute},
		{3, time.Minute, 30 * time.Minute, 4 * time.Minute},
		{7, time.Minute, 30 * time.Minute, 64 * time.Minute},
		{8, time.Minute, 30 * time.Minute, 2 * time.Hour}, // 128m 超过上限
		{99, time.Minute, 30 * time.Minute, 2 * time.Hour},
		{99, time.Minute, 3 * time.Hour, 3 * time.Hour}, // 轮询本身比 2h 长时以间隔为上限
		{3, 10 * time.Millisecond, time.Hour, 40 * time.Millisecond},
		{40, 10 * time.Millisecond, time.Hour, 2 * time.Hour},
	}
	for _, c := range cases {
		if got := backoff(c.fails, c.base, c.interval); got != c.want {
			t.Errorf("backoff(%d, %s, %s) = %s, want %s", c.fails, c.base, c.interval, got, c.want)
		}
	}
}

func TestPickSelectsOnlyWhatNeedsFetching(t *testing.T) {
	known := state{Known: map[string]string{}, Pending: map[string]bool{}}
	a, b, c, d := refOf("a"), refOf("b"), refOf("c"), refOf("d")
	known.Known["a"] = hexOf(a) // 指纹未变
	known.Known["b"] = "stale"  // 指纹变了
	known.Known["c"] = hexOf(c) // 未变但名下有待确认
	known.Pending["c"] = true
	// d 从没见过

	all := []Ref{a, b, c, d}
	if got, want := refIDs(pick(all, known, false)), []string{"b", "c", "d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("增量挑选 = %v, want %v", got, want)
	}
	if got := pick(all, known, true); len(got) != 4 {
		t.Errorf("全量校准应重抓所有 4 条，实际 %d 条：%v", len(got), refIDs(got))
	}
}

func TestProjectAppliesOverridesAndSkipsUnusable(t *testing.T) {
	recs := []Rec{
		{ID: "s:1:0", SourceID: "s", Title: "干净", Start: t0, End: t0.Add(time.Hour), Status: StatusOK},
		{ID: "s:2:0", SourceID: "s", Title: "左端模糊", Start: t0, End: t0.Add(2 * time.Hour), Status: StatusFuzzyStart},
		{ID: "s:3:0", SourceID: "s", Title: "说不清", Start: t0, End: t0.Add(3 * time.Hour), Status: StatusPending},
		{ID: "s:4:0", SourceID: "s", Title: "被改期", Start: t0, End: t0.Add(4 * time.Hour), Status: StatusOK},
		{ID: "s:5:0", SourceID: "s", Title: "被隐藏", Start: t0, End: t0.Add(5 * time.Hour), Status: StatusOK},
		{ID: "s:6:0", SourceID: "s", Title: "被改坏", Start: t0, End: t0.Add(6 * time.Hour), Status: StatusOK},
		{ID: "s:7:0", SourceID: "s", Title: "干净", Start: t0, End: t0.Add(time.Hour), Status: StatusOK},
	}
	ov := map[string]Override{
		"s:4:0": {Title: "被改期（人工）", End: t0.Add(40 * time.Hour)},
		"s:5:0": {Hide: true},
		"s:6:0": {End: t0.Add(-time.Hour)}, // 结束早于开始：宁可不显示
	}

	acts, dups := project(recs, ov)

	got := map[string]string{}
	for _, a := range acts {
		got[a.Title] = a.ID
	}
	for _, title := range []string{"干净", "左端模糊", "被改期（人工）"} {
		if _, ok := got[title]; !ok {
			t.Errorf("投影结果缺少 %q，实得 %v", title, got)
		}
	}
	for _, title := range []string{"说不清", "被隐藏", "被改坏"} {
		if id, ok := got[title]; ok {
			t.Errorf("%q 不该进日历（id=%s）", title, id)
		}
	}
	if len(acts) != 3 {
		t.Errorf("投影条数 = %d, want 3（三元组重复的那条应被挡下）：%v", len(acts), acts)
	}
	if len(dups) != 1 || dups[0].ID != "s:7:0" {
		t.Errorf("重复项 = %+v, want 只有 s:7:0（这条要告警，不是静默丢弃）", dups)
	}
	for _, a := range acts {
		if a.ID == "s:4:0" && !a.End.Equal(t0.Add(40*time.Hour)) {
			t.Errorf("覆盖没改到结束时间：%v", a.End)
		}
		if a.Game != "s" {
			t.Errorf("Game = %q, want 源名 s", a.Game)
		}
	}
}

func TestProjectWithoutOverridesKeepsEverythingUsable(t *testing.T) {
	recs := []Rec{{ID: "s:1:0", Title: "干净", Start: t0, End: t0.Add(time.Hour), Status: StatusOK}}
	acts, dups := project(recs, nil)
	if len(acts) != 1 || len(dups) != 0 {
		t.Fatalf("acts=%v dups=%v, want 1 条 0 重复", acts, dups)
	}
}

func TestMissingIDs(t *testing.T) {
	got := missingIDs([]string{"a", "b", "c"}, []string{"b", "d"})
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("missingIDs = %v, want [a c]", got)
	}
	if got := missingIDs(nil, []string{"a"}); len(got) != 0 {
		t.Errorf("首轮（lastIDs 为空）不该产生删除：%v", got)
	}
}

func TestWantsRetry(t *testing.T) {
	cases := []struct {
		what string
		item Item
		want bool
	}{
		{"干净", Item{Events: []Event{{Status: StatusOK}}}, false},
		{"左端模糊", Item{Events: []Event{{Status: StatusFuzzyStart}}}, false},
		{"有待确认", Item{Events: []Event{{Status: StatusOK}, {Status: StatusPending}}}, true},
		{"整条可疑", Item{Events: []Event{{Status: StatusOK}}, Suspect: true}, true},
		{"空状态也算待确认", Item{Events: []Event{{}}}, true},
		// 真官网全量实测：28 条半年前解析不出来的旧公告会因上面几条每轮重抓，永远抓不完。
		{"刚发的照抓", Item{Suspect: true, Published: t0}, true},
		{"发布满 RetryAge 的不再抓", Item{Suspect: true, Published: t0.Add(-RetryAge)}, false},
		{"发布时间未知的照抓", Item{Suspect: true}, true},
		{"旧的待确认记录也不再抓", Item{
			Events: []Event{{Status: StatusPending}}, Published: t0.Add(-RetryAge - time.Hour),
		}, false},
	}
	for _, c := range cases {
		if got := wantsRetry(c.item, t0); got != c.want {
			t.Errorf("%s: wantsRetry = %v, want %v", c.what, got, c.want)
		}
	}
}

func TestRecIDIsSourceScoped(t *testing.T) {
	if got := recID("stellasora", "4435", 2); got != "stellasora:4435:2" {
		t.Errorf("recID = %q", got)
	}
}
