package calposter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPageInjectsDataset(t *testing.T) {
	tpl := []byte("before " + DataMark + " after")
	d := Dataset{Now: "2026-09-08 20:00", OpenMs: 61200000,
		Windows: []Window{{Key: "202609071600", Name: "V", Start: "2026-09-08 00:00", Until: "2026-09-29 10:59"}}}
	out, err := Page(d, tpl)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	const head = "before const DATA = "
	if !strings.HasPrefix(s, head) || !strings.HasSuffix(s, "; after") {
		t.Fatalf("注入形式不对: %q", s)
	}
	var back Dataset
	if err := json.Unmarshal([]byte(s[len(head):len(s)-len("; after")]), &back); err != nil {
		t.Fatalf("注入的不是合法 JSON: %v\n%s", err, s)
	}
	if back.OpenMs != 61200000 || back.Windows[0].Key != "202609071600" {
		t.Errorf("回读不一致: %+v", back)
	}
	// 线格式键名不能漂：页面按这些名字取值。
	for _, k := range []string{`"openOffsetMs"`, `"versions"`, `"act0"`, `"redeem"`, `"monthly"`} {
		if !strings.Contains(s, k) {
			t.Errorf("缺少模板要用的键 %s", k)
		}
	}
	// 原文里带 </script> 也不能把脚本段截断。
	d.Records = []Record{{ID: "1", Raw: "</script><script>alert(1)</script>"}}
	bad, err := Page(d, []byte("<script>"+DataMark+"</script>"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(bad), "</script>"); n != 1 {
		t.Errorf("转义后仍有 %d 个 </script>，脚本段会被截断", n)
	}
	if !strings.Contains(string(bad), `\u003c/script`) {
		t.Errorf("没把 < 转义: %s", bad)
	}
	if _, err := Page(d, []byte("没有占位")); err == nil {
		t.Error("模板里没有占位却成功了")
	}
}

func TestTemplateIsEmbedded(t *testing.T) {
	if len(defaultTemplate) < 1000 {
		t.Fatalf("模板没打进来（%d 字节）", len(defaultTemplate))
	}
	if !strings.Contains(string(defaultTemplate), DataMark) {
		t.Error("内嵌模板里没有数据占位")
	}
	if !strings.Contains(string(defaultTemplate), "location.hash") {
		t.Error("内嵌模板不像那份日历页")
	}
}
