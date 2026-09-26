package calexpiry

import "xingta/internal/config"

// DeployConfig 是本功能的部署参数契约：config/calexpiry.yml 的唯一读者。
type DeployConfig struct {
	Lead  config.Dur `yaml:"lead"`
	Every config.Dur `yaml:"every"`
}

// LoadDeployConfig 两项都必须为正：Every 为 0 会让 time.NewTicker 直接 panic，
// Lead 为 0 则是"永远不提醒"这种没人会想要的静默失效。
func LoadDeployConfig(path string) (DeployConfig, *config.File, error) {
	var d DeployConfig
	f, err := config.Load(path, &d)
	if err != nil {
		return DeployConfig{}, nil, err
	}
	var bad config.Bad
	bad.Pos("lead", d.Lead.Duration)
	bad.Pos("every", d.Every.Duration)
	if err := bad.Err(path); err != nil {
		return DeployConfig{}, nil, err
	}
	return d, f, nil
}

func (d DeployConfig) ToConfig(c Config) Config {
	c.Lead = d.Lead.Duration
	c.Every = d.Every.Duration
	return c
}
