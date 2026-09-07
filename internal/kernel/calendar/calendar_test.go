package calendar

import (
	"fmt"
	"testing"
	"time"
)

func act(id string, start, end time.Time) Activity {
	return Activity{ID: id, Game: "gs", Title: "t-" + id, Start: start, End: end}
}

func TestQueriesAroundNow(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	s := NewStore()
	s.BulkUpsert([]Activity{
		act("已结束", now.Add(-3*time.Hour), now.Add(-1*time.Hour)),
		act("进行中", now.Add(-1*time.Hour), now.Add(2*time.Hour)),
		act("快结束", now.Add(-2*time.Hour), now.Add(30*time.Minute)),
		act("一小时后开始", now.Add(time.Hour), now.Add(3*time.Hour)),
		act("十天后开始", now.Add(240*time.Hour), now.Add(300*time.Hour)),
	})

	active := ids(s.Active(now))
	assertSet(t, "Active", active, []string{"进行中", "快结束"})

	upcoming := ids(s.Upcoming(now, 24*time.Hour))
	assertSet(t, "Upcoming", upcoming, []string{"一小时后开始"})

	ending := ids(s.EndingWithin(now, time.Hour))
	assertSet(t, "EndingWithin", ending, []string{"快结束"})
}

func TestBoundaryTimes(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	s := NewStore()
	s.BulkUpsert([]Activity{
		act("恰好开始", now, now.Add(time.Hour)),
		act("恰好结束", now.Add(-time.Hour), now),
	})
	// Start==now 视为已开始，End==now 视为已结束
	assertSet(t, "Active", ids(s.Active(now)), []string{"恰好开始"})
}

func TestUpsertReschedules(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	s := NewStore()
	s.Upsert(act("维护", now.Add(time.Hour), now.Add(2*time.Hour)))
	if len(s.Upcoming(now, 90*time.Minute)) != 1 {
		t.Fatal("改期前应能查到即将开始的活动")
	}

	s.Upsert(act("维护", now.Add(10*time.Minute), now.Add(20*time.Minute)))
	if s.Len() != 1 {
		t.Fatalf("同 ID 改期后条目数 = %d, want 1", s.Len())
	}
	assertSet(t, "Active", ids(s.Active(now)), nil)
	assertSet(t, "改期后 Upcoming", ids(s.Upcoming(now, 30*time.Minute)), []string{"维护"})

	if !s.Remove("维护") {
		t.Error("Remove 返回 false")
	}
	if s.Len() != 0 || len(s.byStart) != 0 {
		t.Errorf("删除后仍残留: len=%d sorted=%d", s.Len(), len(s.byStart))
	}
}

// 排序不变量：任何写入组合后 byStart 必须真的按 Start 升序，否则二分前提不成立。
func TestByStartStaysSortedUnderMixedWrites(t *testing.T) {
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	s := NewStore()
	for i := 0; i < 300; i++ {
		switch i % 3 {
		case 0:
			s.BulkUpsert([]Activity{act(fmt.Sprintf("b%d", i), base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i+60)*time.Minute))})
		case 1:
			s.Upsert(act(fmt.Sprintf("u%d", i), base.Add(time.Duration(600-i)*time.Minute), base.Add(time.Duration(700)*time.Minute)))
		case 2:
			id := fmt.Sprintf("u%d", i-1)
			s.Remove(id)
		}
	}
	if err := sortedInvariant(s); err != nil {
		t.Error(err)
	}
	if s.Len() != len(s.byStart) {
		t.Errorf("map 与有序切片不一致: %d vs %d", s.Len(), len(s.byStart))
	}
}

func sortedInvariant(s *Store) error {
	for i := 1; i < len(s.byStart); i++ {
		if s.byStart[i-1].Start.After(s.byStart[i].Start) {
			return fmt.Errorf("byStart 在 %d 处乱序: %v 晚于 %v", i, s.byStart[i-1].Start, s.byStart[i].Start)
		}
	}
	return nil
}

func TestBulkUpsertReplacesExisting(t *testing.T) {
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	s := NewStore()
	s.Upsert(act("a", base, base.Add(time.Hour)))
	s.Upsert(act("b", base.Add(time.Hour), base.Add(2*time.Hour)))
	s.BulkUpsert([]Activity{act("a", base.Add(5*time.Hour), base.Add(6*time.Hour))})

	if s.Len() != 2 {
		t.Fatalf("条目数 = %d, want 2", s.Len())
	}
	got, _ := s.Get("a")
	if !got.Start.Equal(base.Add(5 * time.Hour)) {
		t.Errorf("BulkUpsert 未覆盖已有活动: %v", got.Start)
	}
	if err := sortedInvariant(s); err != nil {
		t.Error(err)
	}
}

func TestRemoveSetDeletesInOnePass(t *testing.T) {
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	s := NewStore()
	for _, id := range []string{"a", "b", "c", "d"} {
		s.Upsert(act(id, base, base.Add(time.Hour)))
	}

	if n := s.RemoveSet([]string{"a", "c", "ghost"}); n != 2 {
		t.Errorf("RemoveSet = %d, want 2（不存在的 id 不应计入）", n)
	}
	if s.Len() != 2 {
		t.Errorf("删除后条目数 = %d, want 2", s.Len())
	}
	assertSet(t, "Active 的 id", ids(s.Active(base.Add(30*time.Minute))), []string{"b", "d"})
	if err := sortedInvariant(s); err != nil {
		t.Error(err)
	}
	if n := s.RemoveSet(nil); n != 0 {
		t.Errorf("RemoveSet(nil) = %d, want 0", n)
	}
}

func ids(list []Activity) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.ID)
	}
	return out
}

func assertSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
		return
	}
	seen := make(map[string]bool, len(want))
	for _, w := range want {
		seen[w] = true
	}
	for _, g := range got {
		if !seen[g] {
			t.Errorf("%s 多出非预期条目 %q (got=%v want=%v)", what, g, got, want)
			return
		}
	}
}
