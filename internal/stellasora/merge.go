package stellasora

import (
	"regexp"
	"strings"
	"time"

	"xingta/internal/annsync"
)

// 跨条合并：把不同来源给出的同一条活动并成一行。
//
// 判同口径是全量实测校准过的（349 篇、99 次配对全部正确）：**同名且窗口交并比
// ≥0.8 才算同一条**。两个方向都不能放宽——
//   - 只看名字：复玩模式隔两个月再开（终焉绝响、命运星牌局都是）窗口几乎不重叠，
//     那是两条真实档期，按名字互删会把新一期整个删没；
//   - 只看窗口：不同活动同档期太常见（版本首日一堆活动同时开）。
//
// 来源优先级 solo > version > summary：独立公告的活动图最准；「[X]活动一览」就是
// 版本主活动自己的公告（带版本主视觉），用它盖掉维护公告切出的同一条（无海报、
// 起点模糊的斜纹行）；维护公告切分垫底。被删侧的引用 ID 记进保留侧的 TwinRef，
// 回溯"这条档期最早是哪篇公告给出的"。
//
// 只有 summary 侧的（独立公告还没发）：保留，海报空。等独立公告发出后的下一轮
// 全量投影自然替换成 solo 侧——引擎投影是"先清本源旧记录再写新记录"，不留孤儿。
var _ annsync.Merger = (*Source)(nil)

// summaryIoU 是"同一条活动"的窗口重合阈值。0.8 取自实测：官方微调时间的幅度
// 远小于此，复玩两期的重合远小于此，中间是空档。
const summaryIoU = 0.8

type evPos struct{ item, ev int }

func (s *Source) Merge(all []annsync.Item) []annsync.Item {
	// 胜者侧索引：solo 最高、version 其次；summary 最后逐条查这索引。
	solo := map[string][]evPos{}
	vers := map[string][]evPos{}
	for i := range all {
		for j := range all[i].Events {
			n := normEventName(all[i].Events[j].Title)
			switch all[i].Events[j].Provenance {
			case ProvSolo:
				solo[n] = append(solo[n], evPos{i, j})
			case ProvVersion:
				vers[n] = append(vers[n], evPos{i, j})
			}
		}
	}

	drop := map[evPos]bool{}

	// version 让位：真被同名同窗的 solo 覆盖时（实测库中未出现，属保险），solo 赢。
	for n, ps := range vers {
		for _, p := range ps {
			ev := &all[p.item].Events[p.ev]
			for _, q := range solo[n] {
				w := &all[q.item].Events[q.ev]
				if ioU(ev.Start, ev.End, w.Start, w.End) >= summaryIoU {
					w.TwinRef = all[p.item].Ref.ID
					drop[p] = true
					break
				}
			}
		}
	}
	liveVers := map[string][]evPos{}
	for n, ps := range vers {
		for _, p := range ps {
			if !drop[p] {
				liveVers[n] = append(liveVers[n], p)
			}
		}
	}

	// 依次处理 summary 事件：与任一 solo、存活 version 或**已保留的** summary
	// 同名且窗口重合 → 删。
	var keptSumm map[string][]evPos
	for i := range all {
		for j := range all[i].Events {
			ev := &all[i].Events[j]
			if ev.Provenance != ProvSummary {
				continue
			}
			n := normEventName(ev.Title)
			hit := evPos{-1, -1}
			for _, p := range solo[n] {
				if ioU(ev.Start, ev.End, all[p.item].Events[p.ev].Start, all[p.item].Events[p.ev].End) >= summaryIoU {
					hit = p
					break
				}
			}
			if hit.item < 0 {
				for _, p := range liveVers[n] {
					if ioU(ev.Start, ev.End, all[p.item].Events[p.ev].Start, all[p.item].Events[p.ev].End) >= summaryIoU {
						hit = p
						break
					}
				}
			}
			if hit.item < 0 {
				for _, p := range keptSumm[n] {
					// 同名同窗的条目在同一条公告内也该并（4550 剧情段与活动段各出现
					// 一次「奋斗吧！」）；只跳过自己这个位置。
					if p.item == i && p.ev == j {
						continue
					}
					if ioU(ev.Start, ev.End, all[p.item].Events[p.ev].Start, all[p.item].Events[p.ev].End) >= summaryIoU {
						hit = p
						break
					}
				}
			}
			if hit.item >= 0 {
				all[hit.item].Events[hit.ev].TwinRef = all[i].Ref.ID
				drop[evPos{i, j}] = true
			} else {
				if keptSumm == nil {
					keptSumm = map[string][]evPos{}
				}
				keptSumm[n] = append(keptSumm[n], evPos{i, j})
			}
		}
	}

	if len(drop) == 0 {
		return all
	}
	out := make([]annsync.Item, len(all))
	for i := range all {
		it := all[i]
		evs := make([]annsync.Event, 0, len(it.Events))
		for j, ev := range it.Events {
			if !drop[evPos{i, j}] {
				evs = append(evs, ev)
			}
		}
		it.Events = evs
		out[i] = it
	}
	return out
}

// normEventName 配对用的名字规范化：去空白与常见标点，剥引号。
// 「优雅起舞 千金驾到」与「优雅起舞 千金驾到！」（官方加叹号）必须配得上。
var punctRe = regexp.MustCompile(`[！!。，,、~\s]`)

func normEventName(s string) string {
	return strings.Trim(punctRe.ReplaceAllString(s, ""), "「」『』")
}

func ioU(s1, e1, s2, e2 time.Time) float64 {
	maxStart := s1
	if s2.After(maxStart) {
		maxStart = s2
	}
	minEnd := e1
	if e2.Before(minEnd) {
		minEnd = e2
	}
	inter := minEnd.Sub(maxStart)
	if inter <= 0 {
		return 0
	}
	union := e1.Sub(s1) + e2.Sub(s2) - inter
	if union <= 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
