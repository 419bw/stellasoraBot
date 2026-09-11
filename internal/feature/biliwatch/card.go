package biliwatch

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"strings"
	"sync"
)

//go:embed template.html
var cardTemplateBytes []byte

var (
	parsedTmplOnce sync.Once
	parsedTmpl     *template.Template
	parseTmplErr   error
)

func getCardTemplate() (*template.Template, error) {
	parsedTmplOnce.Do(func() {
		parsedTmpl, parseTmplErr = template.New("bili_card").Parse(string(cardTemplateBytes))
	})
	return parsedTmpl, parseTmplErr
}

// RenderCardData 是注入 template.html 的视图片段数据。
type RenderCardData struct {
	ID          string
	Name        string
	NameColor   string
	Avatar      string
	Action      string
	Time        string
	VipLabel    string
	HTMLContent template.HTML
	Pics        []string
	PicsLayout  string
	Video       *VideoData
	OrigCard    *OrigCardData
	GameCard    *GameCardData
	Stats       StatsData
}

type VideoData struct {
	Cover        string
	Title        string
	Desc         string
	Duration     string
	PlayCount    string
	DanmakuCount string
}

type OrigCardData struct {
	AuthorName  string
	HTMLContent template.HTML
	Pics        []string
}

type GameCardData struct {
	Title    string
	Cover    string
	HeadText string
	Desc1    string
	Desc2    string
	BtnText  string
}

type StatsData struct {
	Forward int
	Comment int
	Like    int
}

func formatRichText(nodes []RichNode, fallback string) string {
	if len(nodes) == 0 {
		return html.EscapeString(fallback)
	}
	var sb strings.Builder
	for _, n := range nodes {
		switch n.Type {
		case "RICH_TEXT_NODE_TYPE_LOTTERY":
			sb.WriteString(fmt.Sprintf(`<span class="badge-lottery">🎁 %s</span>`, html.EscapeString(n.Text)))
		case "RICH_TEXT_NODE_TYPE_TOPIC":
			sb.WriteString(fmt.Sprintf(`<span class="tag-topic">%s</span>`, html.EscapeString(n.Text)))
		case "RICH_TEXT_NODE_TYPE_AT":
			sb.WriteString(fmt.Sprintf(`<span class="tag-at">%s</span>`, html.EscapeString(n.Text)))
		case "RICH_TEXT_NODE_TYPE_WEB":
			sb.WriteString(fmt.Sprintf(`<span class="tag-link">🔗 %s</span>`, html.EscapeString(n.Text)))
		default:
			sb.WriteString(html.EscapeString(n.Text))
		}
	}
	return sb.String()
}

// BuildCardPage 将动态项转为带有完整样式与自报尺寸脚本的 HTML 页面字节。
func BuildCardPage(item DynamicItem) ([]byte, error) {
	tmpl, err := getCardTemplate()
	if err != nil {
		return nil, fmt.Errorf("biliwatch: 准备模板失败: %w", err)
	}

	author := item.Modules.ModuleAuthor
	data := RenderCardData{
		ID:        item.IdStr,
		Name:      author.Name,
		NameColor: "#18191c",
		Avatar:    author.Face,
		Action:    author.PubAction,
		Time:      author.PubTime,
		VipLabel:  "官方认证",
		Stats: StatsData{
			Forward: toInt(item.Modules.ModuleStat.Forward.Count),
			Comment: toInt(item.Modules.ModuleStat.Comment.Count),
			Like:    toInt(item.Modules.ModuleStat.Like.Count),
		},
	}
	if author.Vip != nil && author.Vip.NicknameColor != "" {
		data.NameColor = author.Vip.NicknameColor
	}

	// 解析正文与富文本节点
	var nodes []RichNode
	var fallbackText string
	dyn := item.Modules.ModuleDynamic

	if dyn.Major != nil && dyn.Major.Opus != nil {
		nodes = dyn.Major.Opus.Summary.RichTextNodes
		fallbackText = dyn.Major.Opus.Summary.Text
		for _, pic := range dyn.Major.Opus.Pics {
			data.Pics = append(data.Pics, pic.Url)
		}
	} else if dyn.Desc != nil {
		nodes = dyn.Desc.RichTextNodes
		fallbackText = dyn.Desc.Text
	}

	if dyn.Major != nil && dyn.Major.Draw != nil {
		for _, pic := range dyn.Major.Draw.Items {
			data.Pics = append(data.Pics, pic.Url)
		}
	}

	if len(nodes) > 0 || fallbackText != "" {
		data.HTMLContent = template.HTML(formatRichText(nodes, fallbackText))
	}

	// 图片布局判断
	switch len(data.Pics) {
	case 1:
		data.PicsLayout = "single"
	case 2:
		data.PicsLayout = "grid-2"
	case 4:
		data.PicsLayout = "grid-4"
	default:
		data.PicsLayout = "grid-3"
	}

	// 视频投稿处理
	if dyn.Major != nil && dyn.Major.Archive != nil {
		arc := dyn.Major.Archive
		data.Video = &VideoData{
			Cover:        arc.Cover,
			Title:        arc.Title,
			Desc:         arc.Desc,
			Duration:     arc.Duration,
			PlayCount:    arc.Stat.Play,
			DanmakuCount: arc.Stat.Danmaku,
		}
	}

	// 转发原动态处理
	if item.Orig != nil {
		orig := item.Orig
		var origNodes []RichNode
		var origFallback string
		var origPics []string

		if orig.Modules.ModuleDynamic.Major != nil && orig.Modules.ModuleDynamic.Major.Opus != nil {
			origNodes = orig.Modules.ModuleDynamic.Major.Opus.Summary.RichTextNodes
			origFallback = orig.Modules.ModuleDynamic.Major.Opus.Summary.Text
			for _, p := range orig.Modules.ModuleDynamic.Major.Opus.Pics {
				origPics = append(origPics, p.Url)
			}
		} else if orig.Modules.ModuleDynamic.Desc != nil {
			origNodes = orig.Modules.ModuleDynamic.Desc.RichTextNodes
			origFallback = orig.Modules.ModuleDynamic.Desc.Text
		}
		if orig.Modules.ModuleDynamic.Major != nil && orig.Modules.ModuleDynamic.Major.Draw != nil {
			for _, p := range orig.Modules.ModuleDynamic.Major.Draw.Items {
				origPics = append(origPics, p.Url)
			}
		}

		data.OrigCard = &OrigCardData{
			AuthorName:  orig.Modules.ModuleAuthor.Name,
			HTMLContent: template.HTML(formatRichText(origNodes, origFallback)),
			Pics:        origPics,
		}
	}

	// 关联游戏/活动小卡片
	if dyn.Additional != nil && dyn.Additional.Common != nil {
		c := dyn.Additional.Common
		btnText := "进入"
		if c.Button != nil && c.Button.JumpStyle.Text != "" {
			btnText = c.Button.JumpStyle.Text
		}
		data.GameCard = &GameCardData{
			Title:    c.Title,
			Cover:    c.Cover,
			HeadText: c.HeadText,
			Desc1:    c.Desc1,
			Desc2:    c.Desc2,
			BtnText:  btnText,
		}
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("biliwatch: 模板执行失败: %w", err)
	}
	return buf.Bytes(), nil
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		var i int
		fmt.Sscanf(n, "%d", &i)
		return i
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}
