package aichat

import (
	"fmt"
	"net/url"
	"strings"

	"xingta/internal/config"
)

// DeployConfig 是本功能的部署参数契约：config/aichat.yml 的唯一读者。
//
// 密钥不在这里：它是凭据，只从 creds.json 读（同 biliwatch 的 Cookie），
// 配置文件一个密钥都不装。
type DeployConfig struct {
	Endpoint        string     `yaml:"endpoint"`
	Model           string     `yaml:"model"`
	Timeout         config.Dur `yaml:"timeout"`
	Proxy           string     `yaml:"proxy"`
	Cooldown        config.Dur `yaml:"cooldown"`
	MaxPerMin       int        `yaml:"max_per_min"`
	MaxOutputTokens int        `yaml:"max_output_tokens"`
	Temperature     float64    `yaml:"temperature"`
	MaxPromptEvents int        `yaml:"max_prompt_events"`
	CDReply         string     `yaml:"cd_reply"`
}

// LoadDeployConfig 把每个数值的"配错了会怎样"当场判掉，不留到第一条群消息那一刻：
//   - endpoint/model 留空 = 每条闲话都白跑一趟然后静默，最难查的那种坏法；
//   - timeout/cooldown 必须为正：0 会让 context.WithTimeout 立刻到期，
//     冷却为 0 则等于没有冷却，两个人同时开口就能把上游问穿；
//   - max_output_tokens 与 max_prompt_events 至少为 1：0 会让回复被截成空串、
//     事实列表一段都不给，模型只能靠编；
//   - temperature 判的是 0~2 这个区间，0 是合法值（完全不要扰动），
//     负数与 3 这种只能是笔误；
//   - cd_reply 必须含字面量 %d：填文案的人改了措辞把占位符弄丢，
//     剩下的就该是启动时报错，而不是回一句"请 秒后再来"。
func LoadDeployConfig(path string) (DeployConfig, *config.File, error) {
	var d DeployConfig
	f, err := config.Load(path, &d)
	if err != nil {
		return DeployConfig{}, nil, err
	}
	var bad config.Bad
	bad.NonEmpty("endpoint", d.Endpoint)
	bad.NonEmpty("model", d.Model)
	bad.Pos("timeout", d.Timeout.Duration)
	bad.Pos("cooldown", d.Cooldown.Duration)
	bad.PosInt("max_per_min", d.MaxPerMin)
	bad.PosInt("max_output_tokens", d.MaxOutputTokens)
	bad.PosInt("max_prompt_events", d.MaxPromptEvents)
	bad.NonEmpty("cd_reply", d.CDReply)
	if d.Temperature < 0 || d.Temperature > 2 {
		bad.Other(fmt.Errorf("temperature 要在 0 到 2 之间，现在是 %g（0 是合法值，别当缺省填）", d.Temperature))
	}
	if !strings.Contains(d.CDReply, "%d") {
		bad.Other(fmt.Errorf("cd_reply 里没有 %%d：剩余秒数没地方放，现在填的是 %q", d.CDReply))
	}
	if p := strings.TrimSpace(d.Proxy); p != "" {
		u, err := url.Parse(p)
		if err != nil {
			bad.Other(fmt.Errorf("proxy %q 不是个能用的地址: %w", p, err))
		} else if !proxyScheme(u.Scheme) {
			bad.Other(fmt.Errorf("proxy 的协议是 %q，只认 http / https / socks5 / socks5h", u.Scheme))
		}
	}
	if err := bad.Err(path); err != nil {
		return DeployConfig{}, nil, err
	}
	return d, f, nil
}

// proxyScheme 与 net/http 的 Transport 支持面一致（见 net/http/transport.go：
// "http"、"https"、"socks5"、"socks5h"，空协议按 http 处理）。
func proxyScheme(scheme string) bool {
	switch scheme {
	case "http", "https", "socks5", "socks5h", "":
		return true
	default:
		return false
	}
}

// ToConfig 只盖数值：Chat/APIKey/Zone/Now/Status 这些接线依赖由调用方给。
// Chat 留 nil 时本包按上面这份配置自建，那是接线不是部署参数。
func (d DeployConfig) ToConfig(c Config) Config {
	c.Endpoint = d.Endpoint
	c.Model = d.Model
	c.Timeout = d.Timeout.Duration
	c.Proxy = strings.TrimSpace(d.Proxy)
	c.Cooldown = d.Cooldown.Duration
	c.MaxPerMin = d.MaxPerMin
	c.MaxOutputTokens = d.MaxOutputTokens
	c.Temperature = d.Temperature
	c.MaxPromptEvents = d.MaxPromptEvents
	c.CDReply = d.CDReply
	return c
}
