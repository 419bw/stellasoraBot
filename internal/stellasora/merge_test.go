package stellasora_test

import (
	"testing"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/stellasora"
)

// 合并优先级 solo > version > summary：「[X]活动一览」产出的版本主活动事件盖掉
// 维护公告切出的同一条（那条无海报、起点模糊）；真独立公告出现时再盖过版本侧。
// 判同口径不变：规范化同名 + 窗口交并比 ≥0.8，复玩同名不同窗不并。
func TestMergeVersionTier(t *testing.T) {
	src := stellasora.New(stellasora.Config{})
	mk := func(ref, title, prov, poster, s, e string) annsync.Item {
		st, _ := time.ParseInLocation("2006-01-02 15:04", s, testZone)
		en, _ := time.ParseInLocation("2006-01-02 15:04", e, testZone)
		return annsync.Item{Ref: annsync.Ref{ID: ref}, Events: []annsync.Event{{
			Title: title, Provenance: prov, Poster: poster,
			Start: st, End: en, Status: annsync.StatusOK,
		}}}
	}

	t.Run("版本主活动覆盖维护切分", func(t *testing.T) {
		out := src.Merge([]annsync.Item{
			mk("4540", "奋斗吧！大小姐的旅人修炼手册", stellasora.ProvVersion, "ver.jpg", "2026-09-08 00:00", "2026-09-22 03:59"),
			mk("4550", "奋斗吧！大小姐的旅人修炼手册！", stellasora.ProvSummary, "", "2026-09-08 00:00", "2026-09-22 03:59"),
		})
		if n := len(out[1].Events); n != 0 {
			t.Fatalf("维护切分条目存活 %d 条, want 0（被版本主活动覆盖）", n)
		}
		ev := out[0].Events[0]
		if ev.TwinRef != "4550" {
			t.Errorf("TwinRef = %q, want 4550（回溯被覆盖侧）", ev.TwinRef)
		}
	})

	t.Run("独立公告盖过版本侧", func(t *testing.T) {
		out := src.Merge([]annsync.Item{
			mk("4356", "某版本主活动", stellasora.ProvVersion, "ver.jpg", "2026-09-08 00:00", "2026-09-22 03:59"),
			mk("4600", "某版本主活动！", stellasora.ProvSolo, "solo.jpg", "2026-09-08 12:00", "2026-09-22 03:59"),
		})
		if n := len(out[0].Events); n != 0 {
			t.Fatalf("版本事件存活 %d 条, want 0（被独立公告覆盖）", n)
		}
		if got := out[1].Events[0].TwinRef; got != "4356" {
			t.Errorf("TwinRef = %q, want 4356", got)
		}
	})

	t.Run("复玩同名不同窗不并", func(t *testing.T) {
		out := src.Merge([]annsync.Item{
			mk("a", "终焉绝响", stellasora.ProvVersion, "p", "2026-09-08 00:00", "2026-09-22 03:59"),
			mk("b", "终焉绝响", stellasora.ProvSummary, "", "2026-11-03 12:00", "2026-11-17 03:59"),
		})
		if len(out[0].Events) != 1 || len(out[1].Events) != 1 {
			t.Fatalf("同名不同窗被误并：%d/%d, want 1/1", len(out[0].Events), len(out[1].Events))
		}
	})
}
