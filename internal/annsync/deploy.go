package annsync

import (
	"xingta/internal/config"
)

// DeployConfig 是同步引擎的部署参数契约：config/annsync.yml 的唯一读者。
//
// 五项一律要求正数，没有一项允许 0 —— 这不是形式主义：Interval 为 0 会让抓取循环
// 失去延时（sync.go 的 loop 是 `if wait > 0` 才建计时器），FullEvery 为 0 变成每轮
// 全量重抓，MinGap 为 0 取消两次详情请求之间的最小间隔，BackoffBase 为 0 让失败重试
// 零间隔。这几个数全是被官网限流实测顶出来的，配错了就是捶站。
type DeployConfig struct {
	Interval    config.Dur `yaml:"interval"`
	FullEvery   config.Dur `yaml:"fullEvery"`
	MinGap      config.Dur `yaml:"minGap"`
	BackoffBase config.Dur `yaml:"backoffBase"`
	Retention   config.Dur `yaml:"retention"`
}

func LoadDeployConfig(path string) (DeployConfig, *config.File, error) {
	var d DeployConfig
	f, err := config.Load(path, &d)
	if err != nil {
		return DeployConfig{}, nil, err
	}
	var bad config.Bad
	bad.Pos("interval", d.Interval.Duration)
	bad.Pos("fullEvery", d.FullEvery.Duration)
	bad.Pos("minGap", d.MinGap.Duration)
	bad.Pos("backoffBase", d.BackoffBase.Duration)
	bad.Pos("retention", d.Retention.Duration)
	if err := bad.Err(path); err != nil {
		return DeployConfig{}, nil, err
	}
	return d, f, nil
}

// ToConfig 只往接好线的 Config 上盖数值，Doc/日历写侧/Now/Logf 一律不动。
func (d DeployConfig) ToConfig(c Config) Config {
	c.Interval = d.Interval.Duration
	c.FullEvery = d.FullEvery.Duration
	c.MinGap = d.MinGap.Duration
	c.BackoffBase = d.BackoffBase.Duration
	c.Retention = d.Retention.Duration
	return c
}
