package main

import (
	"strings"
	"testing"
	"time"
)

// 主入口里只有两个手写的解析件：时区与提醒目标前缀。
// 这两个错了都是静默的——时区偏 1 小时不会报错，只会让所有活动时间都错；
// 前缀写错不会报错，只会让提醒一直投不出去、被队列重试到丢弃。所以钉死它们。

func TestParseZoneOffsets(t *testing.T) {
	cases := []struct {
		in   string
		want int // 相对 UTC 的秒数
	}{
		{"+08:00", 8 * 3600},
		{"+0800", 8 * 3600},
		{"8", 8 * 3600},
		{"+8", 8 * 3600},
		{"-07:30", -(7*3600 + 30*60)},
		{"-0730", -(7*3600 + 30*60)},
		{"+00:00", 0},
		{"+14:00", 14 * 3600},
	}
	probe := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	for _, c := range cases {
		loc, err := parseZone(c.in)
		if err != nil {
			t.Errorf("parseZone(%q): %v", c.in, err)
			continue
		}
		if got := loc.String(); got != c.in {
			t.Errorf("parseZone(%q) 的名字 = %q：日志里要能看出用的是哪个时区", c.in, got)
		}
		_, offset := probe.In(loc).Zone()
		if offset != c.want {
			t.Errorf("parseZone(%q) 的偏移 = %d 秒, want %d", c.in, offset, c.want)
		}
	}
}

func TestParseZoneNamedLocation(t *testing.T) {
	if _, err := time.LoadLocation("Asia/Tokyo"); err != nil {
		t.Skip("本机没有时区数据库，跳过命名时区用例")
	}
	loc, err := parseZone("Asia/Tokyo")
	if err != nil {
		t.Fatalf("parseZone(Asia/Tokyo): %v", err)
	}
	_, offset := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC).In(loc).Zone()
	if offset != 9*3600 {
		t.Errorf("Asia/Tokyo 的偏移 = %d 秒, want %d", offset, 9*3600)
	}
}

func TestParseZoneRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "  ", "abc", "+24:00", "+08:60", "0x12", "+", "-"} {
		if loc, err := parseZone(in); err == nil {
			t.Errorf("parseZone(%q) 竟然成功了（%v）：看不懂的时区必须报错，不能悄悄当成 UTC", in, loc)
		}
	}
}

func TestSplitTarget(t *testing.T) {
	cases := []struct {
		in    string
		id    string
		group bool
	}{
		{"g:GROUP_OPENID_1", "GROUP_OPENID_1", true},
		{"u:USER_OPENID_9", "USER_OPENID_9", false},
	}
	for _, c := range cases {
		id, group, err := splitTarget(c.in)
		if err != nil {
			t.Errorf("splitTarget(%q): %v", c.in, err)
			continue
		}
		if id != c.id || group != c.group {
			t.Errorf("splitTarget(%q) = %q, %v; want %q, %v", c.in, id, group, c.id, c.group)
		}
	}

	for _, in := range []string{"", "GROUP_OPENID_1", "g:", "u:", "G:GROUP1", "group:G1", "g:G1 ", "g: G1"} {
		if id, group, err := splitTarget(in); err == nil {
			t.Errorf("splitTarget(%q) = %q, %v：没有前缀就分不清群还是单聊，必须报错", in, id, group)
		}
	}
}

func TestParseTargetsTrimsAndValidates(t *testing.T) {
	got, err := parseTargets(" g:A1 , u:U2 ,, u:U3 ")
	if err != nil {
		t.Fatalf("parseTargets: %v", err)
	}
	want := []string{"g:A1", "u:U2", "u:U3"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("parseTargets = %v, want %v", got, want)
	}

	empty, err := parseTargets("")
	if err != nil || len(empty) != 0 {
		t.Errorf("空目标该是「只记日志」而不是错误：%v, %v", empty, err)
	}

	if _, err := parseTargets("g:A1,写错了"); err == nil {
		t.Error("混进一个没有前缀的目标却没报错：线上就是一直投不出去还查不出原因")
	} else if !strings.Contains(err.Error(), "写错了") {
		t.Errorf("错误里没点名是哪个目标：%v", err)
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList("a, ,b,, c "); strings.Join(got, "|") != "a|b|c" {
		t.Errorf("splitList = %v", got)
	}
	if got := splitList(""); len(got) != 0 {
		t.Errorf("空串该切出 0 项，实际 %v", got)
	}
}
