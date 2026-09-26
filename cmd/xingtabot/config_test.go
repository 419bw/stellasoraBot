package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"xingta/internal/annsync"
	"xingta/internal/config"
	"xingta/internal/feature/biliwatch"
	"xingta/internal/feature/calexpiry"
	"xingta/internal/feature/calops"
	"xingta/internal/feature/calposter"
	"xingta/internal/feature/calquery"
)

// 仓库根那份 config.yml 是部署的唯一源，而"唯一源"得有机器保证：结构与文件必须双向
// 对得上。少一个键、多一个键、值写成空，LoadRun 一律拒绝加载 —— 所以这条测试做的事
// 就是把仓库里那份文件也走一遍同样的门，防止"代码加了参数、部署文件没加"这种分家。
func TestShippedConfigYMLPassesTheLoader(t *testing.T) {
	if _, _, err := config.LoadRun("../../config.yml"); err != nil {
		t.Fatalf("仓库根的 config.yml 读不过： %v", err)
	}
}

// 注释不参与加载（少一句不影响程序怎么跑），但仓库这份是要入库的门面：由 CI 确认它
// 没退化成一片裸键值对。这里借 Dump 的输出查，不另开一条取注释的口子。
func TestShippedConfigYMLKeepsItsComments(t *testing.T) {
	_, f, err := config.LoadRun("../../config.yml")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	f.Dump(func(format string, args ...any) {
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
	})
	if len(lines) == 0 {
		t.Fatal("一行都没打，说明结构体是空的")
	}
	for _, l := range lines {
		if !strings.Contains(l, "｜") {
			t.Errorf("这行没有注释跟着：%q", l)
		}
	}
}

// featureFiles 是"功能名 → 读它那份配置"的对照表，只给下面的锁用。
// 它不是第二份接线：main.go 里那几行 LoadDeployConfig 才是真正的读点，
// 这张表的价值恰恰是"第三只眼"——三处集合要一致，漏登记任何一处都会红。
var featureFiles = map[string]func(path string) (*config.File, error){
	"annsync":   func(p string) (*config.File, error) { _, f, err := annsync.LoadDeployConfig(p); return f, err },
	"biliwatch": func(p string) (*config.File, error) { _, f, err := biliwatch.LoadDeployConfig(p); return f, err },
	"calexpiry": func(p string) (*config.File, error) { _, f, err := calexpiry.LoadDeployConfig(p); return f, err },
	"calops":    func(p string) (*config.File, error) { _, f, err := calops.LoadDeployConfig(p); return f, err },
	"calposter": func(p string) (*config.File, error) { _, f, err := calposter.LoadDeployConfig(p); return f, err },
	"calquery":  func(p string) (*config.File, error) { _, f, err := calquery.LoadDeployConfig(p); return f, err },
}

// 三份清单必须一致：config/ 目录里实际有哪些文件、main.go 里 featureConfigPath
// 被喂了哪些名字、这张表登记了哪些功能。
//
// 为什么值得钉：仓内契约是"关掉一个功能 = 删掉 main.go 里对应那行 Register"，
// 分文件之后那行之外还多了一份它自己的配置。删干净要动两处，只删一处会留下孤儿文件
// （没人读它，于是没人会发现它过期）；新增功能只加了文件没接线，则是"看起来配了其实
// 没生效"。两种都是静默漂移，靠人记不住。
func TestFeatureConfigFilesMatchTheWiring(t *testing.T) {
	onDisk := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			t.Errorf("config/ 里只该放 <功能>.yml，多了个 %q", e.Name())
			continue
		}
		onDisk[strings.TrimSuffix(e.Name(), ".yml")] = true
	}

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	wired := map[string]bool{}
	for _, m := range regexp.MustCompile(`featureConfigPath\("([^"]+)"\)`).FindAllStringSubmatch(string(src), -1) {
		wired[m[1]] = true
	}

	for name := range onDisk {
		if !wired[name] {
			t.Errorf("config/%s.yml 没有任何代码读它：要么接线漏了，要么这功能是关掉的，文件该一起删", name)
		}
		if _, ok := featureFiles[name]; !ok {
			t.Errorf("%s 没有登记进 featureFiles，下面的加载检查会跳过它", name)
		}
	}
	for name := range wired {
		if !onDisk[name] {
			t.Errorf("main.go 读 config/%s.yml，仓库里却没有这份文件：手机上它就是启动失败", name)
		}
	}
	for name := range featureFiles {
		if !onDisk[name] {
			t.Errorf("featureFiles 登记了 %s，config/ 里没有这个文件", name)
		}
	}
}

// 入库那几份功能配置同样要过各自的加载器，而且每个值都得带着注释 ——
// 理由与 config.yml 那两条一致：这是要给人改的文件，裸键值对等于把语义又搬回代码里。
func TestShippedFeatureConfigsLoadWithComments(t *testing.T) {
	names := make([]string, 0, len(featureFiles))
	for name := range featureFiles {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", "config", name+".yml")
			f, err := featureFiles[name](path)
			if err != nil {
				t.Fatalf("读不过： %v", err)
			}
			var lines []string
			f.Dump(func(format string, args ...any) {
				lines = append(lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
			})
			if len(lines) == 0 {
				t.Fatal("一行都没打，说明这个功能的 DeployConfig 是空的")
			}
			for _, l := range lines {
				if !strings.Contains(l, "｜") {
					t.Errorf("这行没有注释跟着：%q", l)
				}
			}
		})
	}
}
