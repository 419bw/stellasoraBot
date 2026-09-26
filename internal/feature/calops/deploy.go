package calops

import "xingta/internal/config"

// DeployConfig 是本功能的部署参数契约：config/calops.yml 的唯一读者。
type DeployConfig struct {
	PageSize int `yaml:"pageSize"`
}

// LoadDeployConfig：PageSize 为 0 会让分页的除法除零，且崩在运维敲 review 那一刻。
func LoadDeployConfig(path string) (DeployConfig, *config.File, error) {
	var d DeployConfig
	f, err := config.Load(path, &d)
	if err != nil {
		return DeployConfig{}, nil, err
	}
	var bad config.Bad
	bad.PosInt("pageSize", d.PageSize)
	if err := bad.Err(path); err != nil {
		return DeployConfig{}, nil, err
	}
	return d, f, nil
}

func (d DeployConfig) ToConfig(c Config) Config {
	c.PageSize = d.PageSize
	return c
}
