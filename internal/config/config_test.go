package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sample 是一个功能该有的形状：几个扁平参数，没有嵌套。
type sample struct {
	Interval Dur      `yaml:"interval"`
	Workers  int      `yaml:"workers"`
	Label    string   `yaml:"label"`
	ZeroOkay Dur      `yaml:"zeroOkay"`
	Alternat []string `yaml:"alternat"`
	SkipMe   string   `yaml:"-"`
	unexp    int
}

// 注释两种位置都要能用：上一行（长说明）与同一行（短说明）。
const sampleBody = `
# 隔多久跑一轮。0 会把那一轮挤成紧循环。
interval: 5m
workers: 4 # 并发个数。0 会让那一轮永久卡住。
label: cn # 只是个名字。
zeroOkay: 0s # 允许留 0，0 有含义。
# 候选地址列表。
alternat:
  - a
  - b
`

func loadString(t *testing.T, body string) (*sample, *File, error) {
	t.Helper()
	var s sample
	f, err := Load(write(t, body), &s)
	return &s, f, err
}

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestLoadAcceptsWellFormedFile(t *testing.T) {
	s, f, err := loadString(t, sampleBody)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Interval.Duration != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", s.Interval.Duration)
	}
	if s.Workers != 4 || s.Label != "cn" || s.ZeroOkay.Duration != 0 {
		t.Errorf("workers/label/zeroOkay = %d/%q/%v", s.Workers, s.Label, s.ZeroOkay.Duration)
	}
	if len(s.Alternat) != 2 {
		t.Errorf("alternat = %q, want 两项", s.Alternat)
	}
	if got, want := strings.Join(f.keys, ","), "interval,workers,label,zeroOkay,alternat"; got != want {
		t.Errorf("文件里的键序 = %s, want %s", got, want)
	}
	if !strings.Contains(f.comments["workers"], "并发个数") {
		t.Errorf("同行注释没取到: %q", f.comments["workers"])
	}
}

// 下面每一条都对应配置文件里一种真实的写坏方式：尺子先要抓得住错。

func TestDurRejectsNonStringForms(t *testing.T) {
	for _, want := range []string{"300000000000", "true", "[1]", "{a: 1}"} {
		_, _, err := loadString(t, replaceValue(t, sampleBody, "interval", want))
		if err == nil {
			t.Errorf("时长写成 %s 竟然过了：两种写法并存迟早漂", want)
			continue
		}
		if !strings.Contains(err.Error(), "时长") {
			t.Errorf("时长写成 %s 的报错没点明写法问题: %v", want, err)
		}
	}
}

func TestDurRejectsUnparseableString(t *testing.T) {
	// ParseDuration 不认 d：95 天在文件里只能写 2280h，这里确认它确实响。
	_, _, err := loadString(t, replaceValue(t, sampleBody, "interval", "95d"))
	if err == nil || !strings.Contains(err.Error(), "看不懂") {
		t.Errorf("95d 该被拒: %v", err)
	}
}

func TestLoadRejectsNullValue(t *testing.T) {
	// `interval:` 后面什么都不写：YAML 判它 null，而 yaml.v3 对 null 不调自定义解码，
	// 0 会冒充"已配置"混到 time.NewTicker(0) 才炸。必须在加载时就拦。
	_, _, err := loadString(t, replaceValue(t, sampleBody, "interval", ""))
	if err == nil || !strings.Contains(err.Error(), "没有给值") {
		t.Errorf("空值该被拒: %v", err)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	body := sampleBody + "workres: 2 # 手抖多打了一行\n"
	_, _, err := loadString(t, body)
	if err == nil || !strings.Contains(err.Error(), "workres") {
		t.Errorf("拼错的键该被拒并点名: %v", err)
	}
}

func TestLoadRejectsMissingKey(t *testing.T) {
	// 删掉 workers 那一行：结构体声明了它，文件没给值，也没有默认值可退。
	_, _, err := loadString(t, dropLine(t, sampleBody, "workers:"))
	if err == nil || !strings.Contains(err.Error(), "workers") {
		t.Errorf("少一个键该被拒并点名: %v", err)
	}
}

func TestLoadRejectsDuplicateKey(t *testing.T) {
	body := sampleBody + "label: again # 又写一遍\n"
	_, _, err := loadString(t, body)
	if err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Errorf("重复键该被抓到（解进结构体那一路原生就报）: %v", err)
	}
}

func TestLoadRejectsSecondDocument(t *testing.T) {
	body := sampleBody + "\n---\nlabel: b\n"
	_, _, err := loadString(t, body)
	if err == nil || !strings.Contains(err.Error(), "多余的文档") {
		t.Errorf("第二个 YAML 文档该被抓到: %v", err)
	}
}

func TestLoadRejectsNestedStruct(t *testing.T) {
	type nested struct {
		Sub struct {
			A int `yaml:"a"`
		} `yaml:"sub"`
	}
	var n nested
	path := write(t, "sub: {a: 1} # x\n")
	if _, err := Load(path, &n); err == nil || !strings.Contains(err.Error(), "扁平") {
		t.Errorf("嵌套结构体该被拒（对账只做一层）: %v", err)
	}
}

func TestLoadRejectsReservedKeyInFeatureFile(t *testing.T) {
	// 功能文件里塞一个 db：总-分边界靠机制守，不靠人记。
	type featureLike struct {
		DB string `yaml:"db"`
	}
	var f featureLike
	_, err := Load(write(t, "db: data/x.db # 数据库路径\n"), &f)
	if err == nil || !strings.Contains(err.Error(), "总配置") {
		t.Errorf("保留键出现在功能文件里该被拒: %v", err)
	}
}

func TestLoadRejectsNonPointerTarget(t *testing.T) {
	if _, err := Load(write(t, sampleBody), sample{}); err == nil {
		t.Error("传值不传指针该被拒")
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	var s sample
	_, err := Load(filepath.Join(t.TempDir(), "nope.yml"), &s)
	if err == nil || !strings.Contains(err.Error(), "nope.yml") {
		t.Errorf("报错该带上找不到的文件名: %v", err)
	}
}

// ---- 总配置 ----

const runBody = `
# 凭据文件路径。填错就启动失败。
creds: creds.json
# bbolt 文件路径。
db: data/x.db
# 浏览器路径。留空 = 不出图。
chrome: ""
# 公告日期与回复时间用的时区。
tz: +08:00
# API 基地址。
api: https://api.bot.qq.com
# 公告源站点基地址。
source: https://example.cn
# 单聊管理员列表。
admin: []
`

func loadRun(t *testing.T, body string) (Run, error) {
	t.Helper()
	path := write(t, body)
	r, _, err := LoadRun(path)
	return r, err
}

func TestLoadRunAcceptsBlankChromeAndKeepsBareOffset(t *testing.T) {
	r, err := loadRun(t, runBody)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Chrome != "" {
		t.Errorf("chrome 留空是合法值（= 不出图），不该被改: %q", r.Chrome)
	}
	// +08:00 不加引号在 YAML 里必须是字符串：写成 08:00 那种形态会被当六十进制数。
	if r.TZ != "+08:00" {
		t.Errorf("tz = %q, want +08:00", r.TZ)
	}
	if r.DB != "data/x.db" || r.Creds != "creds.json" {
		t.Errorf("db/creds = %q/%q", r.DB, r.Creds)
	}
}

func TestLoadRunRejectsBlankRequiredStrings(t *testing.T) {
	// chrome 之外都不许留空：留空不是"缺省"，是少填了一格。
	for _, key := range []string{"creds", "db", "tz", "api", "source"} {
		_, err := loadRun(t, replaceValue(t, runBody, key, `""`))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s 留空该被拒并点名: %v", key, err)
		}
	}
}

func TestLoadRunFiltersEmptyAdminItem(t *testing.T) {
	r, err := loadRun(t, replaceValue(t, runBody, "admin", `["ok", "  ", "ok2"]`))
	if err != nil {
		t.Fatalf("LoadRun 失败: %v", err)
	}
	if len(r.Admin) != 2 || r.Admin[0] != "ok" || r.Admin[1] != "ok2" {
		t.Errorf("admin = %q, want [ok, ok2]", r.Admin)
	}
}

func TestLoadRunTrimsAdminItems(t *testing.T) {
	r, err := loadRun(t, replaceValue(t, runBody, "admin", `[" a ", "b "]`))
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if len(r.Admin) != 2 || r.Admin[0] != "a" || r.Admin[1] != "b" {
		t.Errorf("admin = %q, want 去空白的两项", r.Admin)
	}
}

// ---- 校验与导出 ----

func TestBadCollectsEveryProblem(t *testing.T) {
	var bad Bad
	bad.Pos("interval", 0)
	bad.PosInt("workers", 0)
	bad.NonEmpty("label", " ")
	err := bad.Err("config/x.yml")
	if err == nil {
		t.Fatal("三项都不合用却没报错")
	}
	for _, want := range []string{"interval", "workers", "label"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错该一次点名 %s，实际: %v", want, err)
		}
	}
	if err := (&Bad{}).Err("x"); err != nil {
		t.Errorf("一项都没有却报了: %v", err)
	}
}

func TestCleanListSemantics(t *testing.T) {
	got := CleanList([]string{"a", " b c ", "d"})
	if len(got) != 3 || got[1] != "b c" {
		t.Errorf("CleanList = %q；want [a, \"b c\", d]", got)
	}
	if got := CleanList(nil); got != nil {
		t.Errorf("空列表该原样给回 nil，得到 %q", got)
	}
	got = CleanList([]string{"", "  ", "x", "\t", "y"})
	if len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Errorf("空项与空白项该被过滤丢弃，得到 %q", got)
	}
}

func TestDumpPrintsValuesWithFirstClauseOfComment(t *testing.T) {
	_, f, err := loadString(t, sampleBody)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	f.Dump(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	want := []string{
		"s.interval = 5m0s｜隔多久跑一轮",
		"s.workers = 4｜并发个数",
		"s.label = cn｜只是个名字",
		"s.zeroOkay = 0s｜允许留 0，0 有含义",
		"s.alternat = a,b｜候选地址列表",
	}
	if len(lines) != len(want) {
		t.Fatalf("行数 = %d, want %d:\n%s", len(lines), len(want), strings.Join(lines, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("第 %d 行 = %q,\n want 前缀 %q", i+1, lines[i], w)
		}
	}
}

// ---- 改样例的小工具 ----

// replaceValue 换掉某个顶层键的值，保留它原来那一行的注释。
func replaceValue(t *testing.T, body, key, want string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, key+":") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(l, key+":"))
		comment := ""
		if j := strings.Index(rest, "#"); j >= 0 {
			comment = " " + rest[j:]
		}
		lines[i] = key + ": " + want + comment
		return strings.Join(lines, "\n")
	}
	t.Fatalf("样例里没有键 %s:\n%s", key, body)
	return ""
}

// dropLine 删掉以某前缀开头的那一行。
func dropLine(t *testing.T, body, prefix string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	out := lines[:0]
	dropped := false
	for _, l := range lines {
		if !dropped && strings.HasPrefix(l, prefix) {
			dropped = true
			continue
		}
		out = append(out, l)
	}
	if !dropped {
		t.Fatalf("样例里没有以 %q 开头的行:\n%s", prefix, body)
	}
	return strings.Join(out, "\n")
}
