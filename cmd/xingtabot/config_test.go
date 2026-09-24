package main

import (
	"fmt"
	"strings"
	"testing"

	"xingta/internal/config"
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
