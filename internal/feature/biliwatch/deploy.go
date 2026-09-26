package biliwatch

import "xingta/internal/config"

// DeployConfig 是本功能的部署参数契约：config/biliwatch.yml 的唯一读者。
//
// Cookie 不在这里：它是凭据，只从 creds.json 读，配置文件一个密钥都不装。
type DeployConfig struct {
	Interval config.Dur `yaml:"interval"`
	UID      string     `yaml:"uid"`
}

// LoadDeployConfig 两项都必须给：Interval 为 0 会让 time.NewTicker 直接 panic，
// 而 panic 发生在 Start 起的后台轮里，进程没有守护，表现为服务静默停摆。
// UID 留空则是"谁都不监听"——一个空转的轮询循环，比报错更难查。
func LoadDeployConfig(path string) (DeployConfig, *config.File, error) {
	var d DeployConfig
	f, err := config.Load(path, &d)
	if err != nil {
		return DeployConfig{}, nil, err
	}
	var bad config.Bad
	bad.Pos("interval", d.Interval.Duration)
	bad.NonEmpty("uid", d.UID)
	if err := bad.Err(path); err != nil {
		return DeployConfig{}, nil, err
	}
	return d, f, nil
}

// ToConfig 只盖数值：Doc/Cap/Fetcher/Cookie/Logf 这些接线依赖由调用方给。
// Fetcher 留 nil 时本包仍会自建 HTTP 客户端，那是接线不是部署参数。
func (d DeployConfig) ToConfig(c Config) Config {
	c.Interval = d.Interval.Duration
	c.UID = d.UID
	return c
}
