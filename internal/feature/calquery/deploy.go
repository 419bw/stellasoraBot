package calquery

import (
	"fmt"

	"xingta/internal/config"
)

// DeployConfig 是本功能的部署参数契约：config/calquery.yml 的唯一读者。
//
// 这四项都会长给用户看：PageSize 决定每页几条，DefaultEndLead 与 DefaultSoonDays
// 被 Start 用 Sprintf 烘进「帮助」里的命令用法（配成 0 就会写出"默认 0 天"这种话），
// MaxDays 是「即将」能问到的最远天数。
type DeployConfig struct {
	PageSize        int        `yaml:"pageSize"`
	DefaultEndLead  config.Dur `yaml:"defaultEndLead"`
	DefaultSoonDays int        `yaml:"defaultSoonDays"`
	MaxDays         int        `yaml:"maxDays"`
}

// LoadDeployConfig 四项都必须为正：PageSize 为 0 会让分页的除法除零，
// 而崩的位置是用户敲「活动」的那一刻，不是启动那一刻。
func LoadDeployConfig(path string) (DeployConfig, *config.File, error) {
	var d DeployConfig
	f, err := config.Load(path, &d)
	if err != nil {
		return DeployConfig{}, nil, err
	}
	var bad config.Bad
	bad.PosInt("pageSize", d.PageSize)
	bad.Pos("defaultEndLead", d.DefaultEndLead.Duration)
	bad.PosInt("defaultSoonDays", d.DefaultSoonDays)
	bad.PosInt("maxDays", d.MaxDays)
	// 一条跨字段的：不带参数的「即将」问的就是 defaultSoonDays 天，上限比它还小
	// 就等于把默认查询截掉——这种组合只有一个解释：填错了。
	if d.MaxDays > 0 && d.MaxDays < d.DefaultSoonDays {
		bad.Other(fmt.Errorf("maxDays(%d) 小于 defaultSoonDays(%d)：默认查询会被自己的上限截掉",
			d.MaxDays, d.DefaultSoonDays))
	}
	if err := bad.Err(path); err != nil {
		return DeployConfig{}, nil, err
	}
	return d, f, nil
}

func (d DeployConfig) ToConfig(c Config) Config {
	c.PageSize = d.PageSize
	c.DefaultEndLead = d.DefaultEndLead.Duration
	c.DefaultSoonDays = d.DefaultSoonDays
	c.MaxDays = d.MaxDays
	return c
}
