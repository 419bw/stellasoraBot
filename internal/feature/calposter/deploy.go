package calposter

import (
	"xingta/internal/config"
)

// DeployConfig 是本功能的部署参数契约：config/calposter.yml 的唯一读者。
//
// 校验与语义同住在这里。"workers 为 0 会把那一轮永久卡住而不是退化成串行"
// （inlineArt 用无缓冲 channel 派发，没人消费就一直等着）、"pushDelay 的 0 是
// 定义出来的合法值 = 开闸即推"—— 这些只有本包说得清，放到组装层就变成抄一份的猜测。
type DeployConfig struct {
	OpenAt     config.Dur `yaml:"openAt"`
	PushDelay  config.Dur `yaml:"pushDelay"`
	Warm       config.Dur `yaml:"warm"`
	Workers    int        `yaml:"workers"`
	PerImage   config.Dur `yaml:"perImage"`
	RetryAfter config.Dur `yaml:"retryAfter"`
}

// LoadDeployConfig 读这份 YAML 并挡掉会让进程挂死或捶站的值。
// 文件路径由 main 给：部署布局是组装层的事，本包只认这个形状。
func LoadDeployConfig(path string) (DeployConfig, *config.File, error) {
	var d DeployConfig
	f, err := config.Load(path, &d)
	if err != nil {
		return DeployConfig{}, nil, err
	}
	var bad config.Bad
	bad.Pos("openAt", d.OpenAt.Duration)
	bad.Pos("warm", d.Warm.Duration)
	bad.PosInt("workers", d.Workers)
	bad.Pos("perImage", d.PerImage.Duration)
	bad.Pos("retryAfter", d.RetryAfter.Duration)
	// pushDelay 故意不查：0 = 开闸即推，不是没填。
	if err := bad.Err(path); err != nil {
		return DeployConfig{}, nil, err
	}
	return d, f, nil
}

// ToConfig 把部署值盖到已经接好线的 Config 上：Doc/Records/Cap/Client/Reg/Zone/Logf
// 这些依赖一律不动，这里只搬数值。
func (d DeployConfig) ToConfig(c Config) Config {
	c.OpenAt = d.OpenAt.Duration
	c.PushDelay = d.PushDelay.Duration
	c.Warm = d.Warm.Duration
	c.Workers = d.Workers
	c.PerImage = d.PerImage.Duration
	c.RetryAfter = d.RetryAfter.Duration
	return c
}
