package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/feature/calexpiry"
	"xingta/internal/feature/calops"
	"xingta/internal/feature/calposter"
	"xingta/internal/feature/calquery"
)

// writeFeatureYML 把一份功能配置落到临时目录，好让测试喂的值跟仓库那份脱钩。
func writeFeatureYML(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// 每份文件用自己的 LoadDeployConfig 读通，不代表组装时喂给了对的包：两个同名
// pageSize（calquery 一组给用户查活动、calops 一组给运维列存疑条目）接反了，两边
// 各自都还是合法 YAML，机制层的校验一个字都不会红。所以这里给每个功能一组互不
// 相同的可辨值，逐字段核对落点。
func TestDeployConfigFieldsLandInTheRightPackages(t *testing.T) {
	t.Run("annsync", func(t *testing.T) {
		dc, _, err := annsync.LoadDeployConfig(writeFeatureYML(t, "annsync", `
interval: 11m
fullEvery: 13h
minGap: 14s
backoffBase: 15s
retention: 2280h
`))
		if err != nil {
			t.Fatal(err)
		}
		c := dc.ToConfig(annsync.Config{})
		if c.Interval != 11*time.Minute {
			t.Errorf("Interval = %v，想要 11m", c.Interval)
		}
		if c.FullEvery != 13*time.Hour {
			t.Errorf("FullEvery = %v，想要 13h", c.FullEvery)
		}
		if c.MinGap != 14*time.Second {
			t.Errorf("MinGap = %v，想要 14s", c.MinGap)
		}
		if c.BackoffBase != 15*time.Second {
			t.Errorf("BackoffBase = %v，想要 15s", c.BackoffBase)
		}
		if c.Retention != 2280*time.Hour {
			t.Errorf("Retention = %v，想要 2280h", c.Retention)
		}
	})

	t.Run("calposter", func(t *testing.T) {
		dc, _, err := calposter.LoadDeployConfig(writeFeatureYML(t, "calposter", `
openAt: 17h
pushDelay: 31m
warm: 5m
workers: 13
perImage: 21s
retryAfter: 11m
`))
		if err != nil {
			t.Fatal(err)
		}
		c := dc.ToConfig(calposter.Config{})
		if c.OpenAt != 17*time.Hour {
			t.Errorf("OpenAt = %v，想要 17h", c.OpenAt)
		}
		if c.PushDelay != 31*time.Minute {
			t.Errorf("PushDelay = %v，想要 31m", c.PushDelay)
		}
		if c.Warm != 5*time.Minute {
			t.Errorf("Warm = %v，想要 5m", c.Warm)
		}
		if c.Workers != 13 {
			t.Errorf("Workers = %d，想要 13", c.Workers)
		}
		if c.PerImage != 21*time.Second {
			t.Errorf("PerImage = %v，想要 21s", c.PerImage)
		}
		if c.RetryAfter != 11*time.Minute {
			t.Errorf("RetryAfter = %v，想要 11m", c.RetryAfter)
		}
	})

	// pushDelay 的 0 是定义出来的合法值（开闸即推），不是"没填"。这一条单独钉：
	// 它是 calposter 校验里唯一的例外，被误当成漏填就会让手机那份配置起不来。
	t.Run("calposter/pushDelay=0", func(t *testing.T) {
		dc, _, err := calposter.LoadDeployConfig(writeFeatureYML(t, "calposter", `
openAt: 17h
pushDelay: 0s
warm: 5m
workers: 4
perImage: 20s
retryAfter: 10m
`))
		if err != nil {
			t.Fatal(err)
		}
		if got := dc.ToConfig(calposter.Config{}).PushDelay; got != 0 {
			t.Errorf("PushDelay = %v，想要 0", got)
		}
	})

	t.Run("calexpiry", func(t *testing.T) {
		dc, _, err := calexpiry.LoadDeployConfig(writeFeatureYML(t, "calexpiry", `
lead: 49h
every: 11m
`))
		if err != nil {
			t.Fatal(err)
		}
		c := dc.ToConfig(calexpiry.Config{})
		if c.Lead != 49*time.Hour {
			t.Errorf("Lead = %v，想要 49h", c.Lead)
		}
		if c.Every != 11*time.Minute {
			t.Errorf("Every = %v，想要 11m", c.Every)
		}
	})

	// 两个 pageSize 一起断言，且值不同：这条就是"接反了要红"的那把尺子。
	t.Run("calquery 与 calops 的 pageSize 不串", func(t *testing.T) {
		qdc, _, err := calquery.LoadDeployConfig(writeFeatureYML(t, "calquery", `
pageSize: 8
defaultEndLead: 41h
defaultSoonDays: 7
maxDays: 60
`))
		if err != nil {
			t.Fatal(err)
		}
		q := qdc.ToConfig(calquery.Config{})
		if q.PageSize != 8 {
			t.Errorf("calquery.PageSize = %d，想要 8", q.PageSize)
		}
		if q.DefaultEndLead != 41*time.Hour {
			t.Errorf("DefaultEndLead = %v，想要 41h", q.DefaultEndLead)
		}
		if q.DefaultSoonDays != 7 {
			t.Errorf("DefaultSoonDays = %d，想要 7", q.DefaultSoonDays)
		}
		if q.MaxDays != 60 {
			t.Errorf("MaxDays = %d，想要 60", q.MaxDays)
		}

		odc, _, err := calops.LoadDeployConfig(writeFeatureYML(t, "calops", `
pageSize: 6
`))
		if err != nil {
			t.Fatal(err)
		}
		o := odc.ToConfig(calops.Config{})
		if o.PageSize != 6 {
			t.Errorf("calops.PageSize = %d，想要 6", o.PageSize)
		}
	})
}

// ToConfig 只盖数值：接线依赖（Zone/Logf/Status/Refresh/…）由 main 给，功能包不该
// 把它们冲掉。冲掉一个的表现为"日历回复尾巴少了数据截至"这类不报错的行为缺失。
func TestToConfigLeavesWiringAlone(t *testing.T) {
	logf := func(string, ...any) {}
	zone := time.FixedZone("TEST", 9*3600)

	qdc, _, err := calquery.LoadDeployConfig(writeFeatureYML(t, "calquery", `
pageSize: 8
defaultEndLead: 48h
defaultSoonDays: 7
maxDays: 60
`))
	if err != nil {
		t.Fatal(err)
	}
	q := qdc.ToConfig(calquery.Config{Zone: zone, Logf: logf})
	if q.Zone != zone {
		t.Error("Zone 被 ToConfig 覆盖了")
	}
	if q.Logf == nil {
		t.Error("Logf 被 ToConfig 清成 nil 了")
	}
	q.Logf("")

	odc, _, err := calops.LoadDeployConfig(writeFeatureYML(t, "calops", "pageSize: 6\n"))
	if err != nil {
		t.Fatal(err)
	}
	if o := odc.ToConfig(calops.Config{Zone: zone, Source: "src"}); o.Zone != zone || o.Source != "src" {
		t.Error("calops 的接线依赖被 ToConfig 覆盖了")
	}

	edc, _, err := calexpiry.LoadDeployConfig(writeFeatureYML(t, "calexpiry", "lead: 48h\nevery: 10m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if e := edc.ToConfig(calexpiry.Config{Zone: zone}); e.Zone != zone {
		t.Error("calexpiry 的 Zone 被 ToConfig 覆盖了")
	}

	pdc, _, err := calposter.LoadDeployConfig(writeFeatureYML(t, "calposter", `
openAt: 17h
pushDelay: 30m
warm: 5m
workers: 4
perImage: 20s
retryAfter: 10m
`))
	if err != nil {
		t.Fatal(err)
	}
	if p := pdc.ToConfig(calposter.Config{Zone: zone, Label: "版本窗口公告"}); p.Zone != zone || p.Label != "版本窗口公告" {
		t.Error("calposter 的接线依赖被 ToConfig 覆盖了")
	}

	sdc, _, err := annsync.LoadDeployConfig(writeFeatureYML(t, "annsync", `
interval: 30m
fullEvery: 24h
minGap: 1s
backoffBase: 1m
retention: 2280h
`))
	if err != nil {
		t.Fatal(err)
	}
	if s := sdc.ToConfig(annsync.Config{Logf: logf}); s.Logf == nil {
		t.Error("annsync 的 Logf 被 ToConfig 清成 nil 了")
	}
}
