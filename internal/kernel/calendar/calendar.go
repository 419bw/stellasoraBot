// Package calendar 保存活动时间区间并提供毫秒级内存查询。
package calendar

import (
	"sort"
	"sync"
	"time"
)

type Activity struct {
	ID    string
	Game  string
	Title string
	Start time.Time
	End   time.Time
	URL   string
}

func (a Activity) Active(now time.Time) bool {
	return !now.Before(a.Start) && now.Before(a.End)
}

type Store struct {
	mu      sync.RWMutex
	byID    map[string]Activity
	byStart []Activity // 按 Start 升序
}

func NewStore() *Store {
	return &Store{byID: make(map[string]Activity)}
}

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byID)
}

// BulkUpsert 用于爬虫一次性刷回整批公告：合并后只排一次序。
func (s *Store) BulkUpsert(list []Activity) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, a := range list {
		s.byID[a.ID] = a
	}
	s.rebuildLocked()
}

// Upsert 单条新增或改期。改期活动的时间可能前后移动，用二分定位插入点。
func (s *Store) Upsert(a Activity) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.byID[a.ID]; ok {
		for i := range s.byStart {
			if s.byStart[i].ID == a.ID {
				s.byStart = append(s.byStart[:i], s.byStart[i+1:]...)
				break
			}
		}
	}
	s.byID[a.ID] = a
	at := sort.Search(len(s.byStart), func(i int) bool { return !s.byStart[i].Start.Before(a.Start) })
	s.byStart = append(s.byStart, Activity{})
	copy(s.byStart[at+1:], s.byStart[at:])
	s.byStart[at] = a
}

func (s *Store) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.byID[id]; !ok {
		return false
	}
	delete(s.byID, id)
	for i := range s.byStart {
		if s.byStart[i].ID == id {
			s.byStart = append(s.byStart[:i], s.byStart[i+1:]...)
			break
		}
	}
	return true
}

// RemoveSet 一把锁内删完一批 id，只重建一次索引。
// 同步引擎每轮用「上轮全量 - 本轮全量」做差集清理，逐条 Remove 会重建 N 次。
// 返回实际删除的条数；不存在的 id 不计入。
func (s *Store) RemoveSet(ids []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, id := range ids {
		if _, ok := s.byID[id]; !ok {
			continue
		}
		delete(s.byID, id)
		n++
	}
	if n > 0 {
		s.rebuildLocked()
	}
	return n
}

func (s *Store) Get(id string) (Activity, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.byID[id]
	return a, ok
}

// startedNotAfter 返回第一个 Start 晚于 now 的下标，即 [0,k) 内的活动都已开始。
func (s *Store) startedNotAfter(now time.Time) int {
	return sort.Search(len(s.byStart), func(i int) bool { return s.byStart[i].Start.After(now) })
}

// Active 返回正在进行的活动。
func (s *Store) Active(now time.Time) []Activity {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []Activity
	for _, a := range s.byStart[:s.startedNotAfter(now)] {
		if a.End.After(now) {
			out = append(out, a)
		}
	}
	return out
}

// Upcoming 返回 horizon 之内即将开始、尚未开始的活动。
func (s *Store) Upcoming(now time.Time, horizon time.Duration) []Activity {
	s.mu.RLock()
	defer s.mu.RUnlock()

	limit := now.Add(horizon)
	var out []Activity
	for _, a := range s.byStart[s.startedNotAfter(now):] {
		if a.Start.After(limit) {
			break // 已按 Start 升序，后面更远
		}
		out = append(out, a)
	}
	return out
}

// EndingWithin 返回正在进行且在 lead 之内结束的活动，用于"快结束了"提醒。
func (s *Store) EndingWithin(now time.Time, lead time.Duration) []Activity {
	s.mu.RLock()
	defer s.mu.RUnlock()

	limit := now.Add(lead)
	var out []Activity
	for _, a := range s.byStart[:s.startedNotAfter(now)] {
		if a.End.After(now) && !a.End.After(limit) {
			out = append(out, a)
		}
	}
	return out
}

func (s *Store) rebuildLocked() {
	list := make([]Activity, 0, len(s.byID))
	for _, a := range s.byID {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].Start.Equal(list[j].Start) {
			return list[i].Start.Before(list[j].Start)
		}
		return list[i].ID < list[j].ID
	})
	s.byStart = list
}
