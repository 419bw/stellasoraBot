package annsync

import (
	"encoding/json"
	"sort"
	"time"

	"xingta/internal/store"
)

// 这个文件是引擎给功能插件的**只读**面：calquery 靠 Status 判断"还在同步中"，
// calops 靠 ReadRecs / ReadPending 列待确认桶。功能不碰 Source，也不碰日历写侧。

// Status 是引擎的运行状况快照。LastSuccess 为零值 = 还没同步成功过一次。
type Status struct {
	Source      string    `json:"source"`
	Known       int       `json:"known"`
	PendingRefs int       `json:"pendingRefs"`
	Fails       int       `json:"fails"`
	LastSuccess time.Time `json:"lastSuccess"`
	LastFull    time.Time `json:"lastFull"`
	LastError   string    `json:"lastError"`
}

// ReadStatus 读某个源的同步状况。src 为源名（Source.Name()）。
func ReadStatus(doc store.Doc, src string) (Status, error) {
	var st state
	ok, err := doc.Get(nsSync, src+":meta", &st)
	if err != nil {
		return Status{}, err
	}
	if !ok {
		return Status{Source: src}, nil
	}
	return Status{
		Source:      src,
		Known:       len(st.Known),
		PendingRefs: len(st.Pending),
		Fails:       st.Fails,
		LastSuccess: st.LastSuccess,
		LastFull:    st.LastFull,
		LastError:   st.LastError,
	}, nil
}

// StatusReader 把 ReadStatus 包成一个可注入的小依赖。
// 功能（如 calquery 判断"还在同步中"）只需声明一个 Status() 方法，
// main 决定给不给——不必把整个 store.Doc 塞过去。
type StatusReader struct {
	doc store.Doc
	src string
}

func NewStatusReader(doc store.Doc, src string) StatusReader {
	return StatusReader{doc: doc, src: src}
}

func (r StatusReader) Status() (Status, error) { return ReadStatus(r.doc, r.src) }

// ReadRec 按 ID 点查一条活动记录。记录键就是 Rec.ID，运维命令（覆盖/确认）用它先取回
// 当前解析值，不必全表扫。
func ReadRec(doc store.Doc, id string) (Rec, bool, error) {
	var r Rec
	ok, err := doc.Get(nsActivity, id, &r)
	return r, ok, err
}

// ReadRecs 读活动记录，含不进日历的 pending 项。src 为空表示所有源。
func ReadRecs(doc store.Doc, src string) ([]Rec, error) {
	var out []Rec
	err := doc.Scan(nsActivity, srcPrefix(src), func(_ string, raw []byte) error {
		var r Rec
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	sortRecs(out)
	return out, err
}

// ReadPending 只读待确认桶：状态不是 ok / fuzzy_start 的记录，
// 它们不进日历，需要人工看一眼原文片段再决定覆盖或确认。
func ReadPending(doc store.Doc, src string) ([]Rec, error) {
	all, err := ReadRecs(doc, src)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, r := range all {
		if !r.InCalendar() {
			out = append(out, r)
		}
	}
	return out, nil
}

// ReadItems 读条目快照。整条可疑（Suspect）但一个子活动都没解析出来的公告，
// 只会出现在这里——待确认桶要连它一起列，否则这类漏抓是静默的。
func ReadItems(doc store.Doc, src string) ([]Item, error) {
	var out []Item
	err := doc.Scan(nsNews, srcPrefix(src), func(_ string, raw []byte) error {
		var it Item
		if err := json.Unmarshal(raw, &it); err != nil {
			return err
		}
		out = append(out, it)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.ID < out[j].Ref.ID })
	return out, err
}

func srcPrefix(src string) string {
	if src == "" {
		return ""
	}
	return src + ":"
}

func sortRecs(list []Rec) {
	sort.Slice(list, func(i, j int) bool {
		if !list[i].Start.Equal(list[j].Start) {
			return list[i].Start.Before(list[j].Start)
		}
		return list[i].ID < list[j].ID
	})
}
