package calendar

import "time"

// View 是功能插件能拿到的读面：只查不改。
// 功能包应当依赖这个接口而不是 *Store，否则连写侧方法都摸得到，分层就名存实亡。
type View interface {
	Get(id string) (Activity, bool)
	Active(now time.Time) []Activity
	Upcoming(now time.Time, horizon time.Duration) []Activity
	EndingWithin(now time.Time, lead time.Duration) []Activity
}

// Writer 是同步引擎拿到的写面。功能插件不持有它：改日历是同步链路的职责，
// 功能想"改期"应当走人工覆盖（存自己的命名空间），由下一轮投影收敛。
type Writer interface {
	Upsert(a Activity)
	BulkUpsert(list []Activity)
	Remove(id string) bool
	RemoveSet(ids []string) int
}

var (
	_ View   = (*Store)(nil)
	_ Writer = (*Store)(nil)
)
