package biliwatch

// DynamicFeedResp 是 B站 space dynamic feed 接口响应结构。
type DynamicFeedResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		HasMore bool          `json:"has_more"`
		Items   []DynamicItem `json:"items"`
		Offset  string        `json:"offset"`
	} `json:"data"`
}

// DynamicItem 代表一条 B站动态条目。
type DynamicItem struct {
	IdStr   string `json:"id_str"`
	Type    string `json:"type"`
	Visible bool   `json:"visible"`
	Modules struct {
		ModuleAuthor struct {
			Mid     int64  `json:"mid"`
			Name    string `json:"name"`
			Face    string `json:"face"`
			PubTime string `json:"pub_time"`
			PubTs   int64  `json:"pub_ts"`
			PubAction string `json:"pub_action"`
			Vip     *struct {
				NicknameColor string `json:"nickname_color"`
				Label         struct {
					Text string `json:"text"`
				} `json:"label"`
			} `json:"vip"`
		} `json:"module_author"`
		ModuleDynamic struct {
			Desc *struct {
				Text          string     `json:"text"`
				RichTextNodes []RichNode `json:"rich_text_nodes"`
			} `json:"desc"`
			Major *struct {
				Type string `json:"type"`
				Opus *struct {
					Summary struct {
						Text          string     `json:"text"`
						RichTextNodes []RichNode `json:"rich_text_nodes"`
					} `json:"summary"`
					Pics []PicInfo `json:"pics"`
				} `json:"opus"`
				Draw *struct {
					Items []PicInfo `json:"items"`
				} `json:"draw"`
				Archive *struct {
					Bvid     string `json:"bvid"`
					Title    string `json:"title"`
					Cover    string `json:"cover"`
					Desc     string `json:"desc"`
					Duration string `json:"duration_text"`
					Stat     struct {
						Play string `json:"play"`
						Danmaku string `json:"danmaku"`
					} `json:"stat"`
				} `json:"archive"`
			} `json:"major"`
			Additional *struct {
				Type   string `json:"type"`
				Common *struct {
					HeadText string `json:"head_text"`
					Title    string `json:"title"`
					Desc1    string `json:"desc1"`
					Desc2    string `json:"desc2"`
					Cover    string `json:"cover"`
					Button   *struct {
						JumpStyle struct {
							Text string `json:"text"`
						} `json:"jump_style"`
					} `json:"button"`
				} `json:"common"`
			} `json:"additional"`
		} `json:"module_dynamic"`
		ModuleStat struct {
			Forward struct {
				Count int `json:"count"`
			} `json:"forward"`
			Comment struct {
				Count int `json:"count"`
			} `json:"comment"`
			Like struct {
				Count int `json:"count"`
			} `json:"like"`
		} `json:"module_stat"`
	} `json:"modules"`
	Orig *DynamicItem `json:"orig,omitempty"`
}

// RichNode 是富文本节点。
type RichNode struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	OrigText string `json:"orig_text"`
	JumpUrl  string `json:"jump_url"`
}

// PicInfo 是图片元信息。
type PicInfo struct {
	Url    string  `json:"url"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Size   float64 `json:"size"`
}
