package calposter

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// DataMark 是模板里放数据集的位置（`/*__DATA__*/`）。
const DataMark = "/*__DATA__*/"

// Record 是一条日历记录，字段名与模板一一对应。
type Record struct {
	ID    string `json:"id"`
	Name  string `json:"n"`
	Label string `json:"lb"`
	Start string `json:"s"`
	End   string `json:"e"`
	// StartKind 为 "fuzzy" 表示原文只给了"维护结束后"这类下界，渲染按开闸估计摆。
	StartKind string `json:"st,omitempty"`
	// Raw 是原文片段，hover 与人工核对用。
	Raw    string `json:"f"`
	Poster string `json:"poster,omitempty"`
	Tint   string `json:"tint"`
	// Source 是这条记录的出处口径，取值由调用方与模板约定，渲染器原样带上不解释。
	Source string `json:"src"`
	Ref    string `json:"ref"`
	// ClaimStart/ClaimEnd 是尾段窗口，空表示没有。
	ClaimStart string `json:"cs,omitempty"`
	ClaimEnd   string `json:"ce,omitempty"`
}

// Window 是这一张图对应的时间窗。
type Window struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Start  string `json:"act0"`
	End    string `json:"act1"`
	Until  string `json:"redeem"`
	Source string `json:"src"`
}

// Dataset 是一次出图的全部输入。时间一律 "2006-01-02 15:04"（同一时区，由调用方保证）。
type Dataset struct {
	Now       string   `json:"now"`
	OpenMs    int64    `json:"openOffsetMs"`
	Records   []Record `json:"records"`
	Windows   []Window `json:"versions"`
	Repeating []string `json:"monthly"`
}

// TimeLayout 是数据集里所有时间字段的格式（同一时区，由调用方保证）。
const TimeLayout = "2006-01-02 15:04"

// Page 把数据集注进模板，返回一整页 HTML。
//
// JSON 里的 < > & 一律转义：字段值来自公告原文，出现 "</script>" 会把脚本段截断。
func Page(d Dataset, tpl []byte) ([]byte, error) {
	body, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("序列化数据集: %w", err)
	}
	i := bytes.Index(tpl, []byte(DataMark))
	if i < 0 {
		return nil, fmt.Errorf("模板里没有数据占位 %s", DataMark)
	}
	out := make([]byte, 0, len(tpl)+len(body)+32)
	out = append(out, tpl[:i]...)
	out = append(out, "const DATA = "...)
	for _, c := range body {
		switch c {
		case '<':
			out = append(out, `\u003c`...)
		case '>':
			out = append(out, `\u003e`...)
		case '&':
			out = append(out, `\u0026`...)
		default:
			out = append(out, c)
		}
	}
	out = append(out, ';')
	out = append(out, tpl[i+len(DataMark):]...)
	return out, nil
}
