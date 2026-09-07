package stellasora_test

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"xingta/internal/annsync"
	"xingta/internal/stellasora"
)

var testZone = time.FixedZone("CST", 8*3600)

// loadDetail 读一条真实公告夹具（文件名里的数字就是官网公告号）。
func loadDetail(t *testing.T, id int64) stellasora.NewsBody {
	t.Helper()
	raw, err := os.ReadFile("testdata/detail_" + strconv.FormatInt(id, 10) + ".json")
	if err != nil {
		t.Fatalf("读夹具: %v", err)
	}
	var wrap struct {
		Data stellasora.Detail `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		t.Fatalf("夹具解码: %v", err)
	}
	return wrap.Data.News
}

// syn 用合成正文造一条公告（线上没有的形状才这么做，注释里写清为什么）。
func syn(title, body string) stellasora.NewsBody {
	return stellasora.NewsBody{ID: 1, Title: title, Content: body, PublishTime: 1}
}

// at 解析表里的时间常量；写错就是测试自身的问题，直接 panic。
func at(s string) time.Time {
	tt, err := time.ParseInLocation("2006-01-02 15:04", s, testZone)
	if err != nil {
		panic(err)
	}
	return tt
}

// 规则 v2 的正面表。每一条都是线上真实公告，断言的是"日历上应当出现什么"。
func TestParseRealAnnouncements(t *testing.T) {
	cases := []struct {
		id      int64
		name    string
		label   string
		start   string
		end     string
		status  string
		suspect bool
		why     string
	}{
		{
			// ▌开放时间 下面并列着"活动时间"与"活动奖励领取时间"两行。
			// 旧实现会把两行都当活动，日历上出现两个缭乱碎晶球。
			id: 4506, name: "缭乱碎晶球", label: "活动时间",
			start: "2026-09-01 12:00", end: "2026-09-08 10:59", status: annsync.StatusOK,
			why: "主窗口取行内前缀是「活动时间」的那一行",
		},
		{
			// 三个窗口：活动 / 排名奖励结算 / 排名奖励领取，只有第一个是活动本身。
			id: 4481, name: "联合讨伐", label: "活动时间",
			start: "2026-09-01 04:00", end: "2026-09-29 22:59", status: annsync.StatusOK,
		},
		{
			// ▌活动时间 独立成行，日期在下一行裸写：标签只能从小节继承。
			id: 4492, name: "紧急悬赏", label: "活动时间",
			start: "2026-09-01 04:00", end: "2026-09-08 03:59", status: annsync.StatusOK,
		},
		{
			// 与 4492 同期、结束时刻不同：两条都要各自成立，不能互相顶掉。
			id: 4480, name: "攻略纪录大征集", label: "活动时间",
			start: "2026-09-01 12:00", end: "2026-09-08 10:59", status: annsync.StatusOK,
		},
		{
			// 起点写"维护结束后"：日期可信、时刻是估的，标 fuzzy_start。
			id: 4378, name: "碧浪晕七彩", label: "招募时间",
			start: "2026-08-18 00:00", end: "2026-09-08 10:59", status: annsync.StatusFuzzyStart,
		},
		{
			// 起点写"公测开启后"：相对起点是开放写法而不是闭表。全量实测时这条
			// （连同 1751、1732）因为词表里没有"公测开启后"而整条漏抓。
			id: 1750, name: "夜月倾流，闪烁刀影里", label: "招募时间",
			start: "2025-10-20 00:00", end: "2025-11-11 03:59", status: annsync.StatusFuzzyStart,
		},
		{
			// 标题只写「…」说明，尾巴没有"活动说明/限时招募"字样：
			// 按后缀白名单取名字会漏掉它，按引号取则不会。
			id: 4156, name: "出击海滨阵地！倾尽全力的碎瓜一击", label: "活动时间",
			start: "2026-08-11 12:00", end: "2026-09-08 10:59", status: annsync.StatusOK,
		},
		{
			// 整条只有▌售卖时间：付费礼包不是活动，静默丢弃。
			// 报警会让每个版本都多一条无意义的待确认。
			id: 4507, name: "创业激励基金",
		},
		{
			// 版本一览页：标题是方括号，正文只有一张长图。
			id: 4357,
		},
		{
			// 版本内容一览：有「」名字，但正文连时间小节都没有 → 与档期无关，不报警。
			id: 3801, name: "枪林弹雨覆黄沙",
		},
		{
			// 诺瓦异闻：写"开放后常驻"，标题也没引号。
			id: 4490,
		},
	}

	for _, c := range cases {
		t.Run(strconv.FormatInt(c.id, 10), func(t *testing.T) {
			r := stellasora.Parse(loadDetail(t, c.id), testZone)
			if r.Name != c.name {
				t.Errorf("活动名 = %q, want %q", r.Name, c.name)
			}
			if c.end == "" {
				if len(r.Primary) != 0 {
					t.Errorf("不该有主窗口，得到 %+v", r.Primary)
				}
				if r.Suspect() != c.suspect {
					t.Errorf("Suspect = %v, want %v（Note=%q Weird=%v）",
						r.Suspect(), c.suspect, r.Note(), r.Weird)
				}
				return
			}
			if len(r.Primary) != 1 {
				t.Fatalf("主窗口数 = %d, want 1（%+v）", len(r.Primary), r.Primary)
			}
			iv := r.Primary[0]
			if iv.Tag != c.label {
				t.Errorf("标签 = %q, want %q", iv.Tag, c.label)
			}
			if !iv.Start.Equal(at(c.start)) {
				t.Errorf("开始 = %s, want %s", iv.Start.Format("2006-01-02 15:04"), c.start)
			}
			if !iv.End.Equal(at(c.end)) {
				t.Errorf("结束 = %s, want %s", iv.End.Format("2006-01-02 15:04"), c.end)
			}
			if iv.Status != c.status {
				t.Errorf("状态 = %q, want %q", iv.Status, c.status)
			}
			if iv.Line == "" {
				t.Error("原文行没带上：待确认桶要靠它给人核对")
			}
			if r.Suspect() {
				t.Errorf("干净的公告被报警了: %s", r.Note())
			}
		})
	}
}

// 附属窗口词表是"一个活动列成两行"和"商店活动混进来"的总开关，逐词钉住。
func TestSecondaryWindowsAreNeverAdopted(t *testing.T) {
	for _, tag := range []string{
		"活动奖励领取时间", "排名奖励领取时间", "排名奖励结算时间",
		"活动商店与奖励兑换时间", "奖励领取时间", "售卖时间",
		"上架时间", "返场时间",
	} {
		r := stellasora.Parse(syn("「某活动」活动说明",
			"<p>▌开放时间：</p><p>"+tag+"：2026/09/01 12:00 ~ 2026/09/08 10:59</p>"), testZone)
		if len(r.Primary) != 0 {
			t.Errorf("%q 被当成主窗口: %+v", tag, r.Primary)
		}
		if len(r.Secondary) != 1 {
			t.Errorf("%q 没归进附属窗口: %d 个", tag, len(r.Secondary))
		}
	}
}

// 线上真实形状：「[盛夏限定招募礼券]使用有效期至2026/08/11 10:59，届时系统将回收」——
// 这是道具期限不是活动档期，既不该成为窗口，也不该因为"有个日期没认出来"而报警。
// （4019 与 3995 两条公告里都有这句。）
func TestItemExpiryLineIsNeitherWindowNorAlarm(t *testing.T) {
	r := stellasora.Parse(syn("「下水之前别忘热身！」限时招募开启",
		`<p>▌招募时间</p><p>2026/07/21 维护结束后 ~ 2026/08/11 10:59</p>`+
			`<p>*[盛夏限定招募礼券]使用有效期至2026/08/11 10:59，届时系统将回收未使用的[盛夏限定招募礼券]，请魔王大人注意。</p>`), testZone)

	if len(r.Primary) != 1 || len(r.Secondary) != 0 {
		t.Errorf("窗口分类错了: Primary=%+v Secondary=%+v", r.Primary, r.Secondary)
	}
	if len(r.Weird) != 0 {
		t.Errorf("期限行被当成没认出的日期: %v", r.Weird)
	}
	if r.Suspect() {
		t.Errorf("不该报警: %s", r.Note())
	}
}

// 主窗口多于一个 = 汇总公告，不该由我们挑一个当活动。
// 线上 74 条全部恰好一个，所以这个分支只能用合成正文钉住行为。
func TestMultiplePrimariesGoToReviewNotCalendar(t *testing.T) {
	r := stellasora.Parse(syn("「两期活动」活动说明",
		`<p>▌活动时间</p><p>2026/09/01 12:00 ~ 2026/09/08 10:59</p>`+
			`<p>▌第二期时间</p><p>2026/09/10 12:00 ~ 2026/09/20 10:59</p>`), testZone)

	if len(r.Primary) != 2 {
		t.Fatalf("主窗口数 = %d, want 2", len(r.Primary))
	}
	if !r.Suspect() {
		t.Error("多个主窗口必须进待确认：否则日历上凭空少一个活动且没人知道")
	}
	if len(r.ExtraPrimary) != 1 {
		t.Errorf("ExtraPrimary = %d, want 1（要能向人说清还有几个）", len(r.ExtraPrimary))
	}
	if want := "正文有 2 个主时间窗口，像是汇总公告，未采信"; r.Note() != want {
		t.Errorf("Note = %q, want %q", r.Note(), want)
	}
}

// 时间小节在、日期却画在图片里 —— 这类漏抓最容易静默，必须报警。
func TestTimeSectionWithoutAnyDateIsSuspect(t *testing.T) {
	r := stellasora.Parse(syn("「只在图里写时间」活动说明",
		`<p>参与活动可获得奖励</p><p>▌活动时间</p><p><img src="a.jpg"></p><p>▌参与条件</p>`), testZone)

	if len(r.Primary) != 0 {
		t.Fatalf("不该产出主窗口: %+v", r.Primary)
	}
	if !r.Suspect() {
		t.Errorf("该报警却没有（Weird=%v TimeLabel=%v）", r.Weird, r.TimeLabel)
	}
	if r.Note() == "" {
		t.Error("报警却没带说明，待确认桶里只会是一句套话")
	}
}

// 「秘纹常驻招募」这类说明性文字里也有"常驻"二字，不能把真漏抓一起抹掉：
// 只有同一行还带着日期时才算"这条是常驻内容"。
func TestMentionOfPermanentDoesNotSilenceRealMiss(t *testing.T) {
	r := stellasora.Parse(syn("「 talking 活动」活动说明",
		`<p>本期活动结束后，5星秘纹「伴我航行」不会立即进入「秘纹常驻招募」。</p>`+
			`<p>▌活动时间</p><p><img src="a.jpg"></p>`), testZone)

	if !r.Suspect() {
		t.Errorf("被「常驻」二字误判成无需人工：SaysPermanent=%v Note=%q", r.SaysPermanent, r.Note())
	}
}

// 变异负对照：把原文改坏一点点，解析器必须「降级并报警」，不能猜出一个像样但错的时间。
func TestMutationTableDegradesInsteadOfGuessing(t *testing.T) {
	if got := stellasora.Parse(syn("「基准」活动说明",
		`<p>▌开放时间：</p><p>活动时间：2026/09/01 12:00 ~ 2026/09/08 10:59</p>`), testZone); len(got.Primary) != 1 {
		t.Fatalf("基准正文自己都没解析出来: %+v", got)
	}

	cases := []struct {
		name    string
		body    string
		wantEnd time.Time // 零值 = 不该采信
		status  string
		suspect bool
	}{
		{"结束日期省年份", `<p>活动时间：2026/09/01 12:00 ~ 09/08 10:59</p>`, at("2026-09-08 10:59"), annsync.StatusOK, false},
		{"日期用点分隔", `<p>活动时间：2026.09.01 12:00 ~ 2026.09.08 10:59</p>`, at("2026-09-08 10:59"), annsync.StatusOK, false},
		{"日期用斜杠带空格", `<p>活动时间：2026 / 09 / 01 12:00 ~ 2026 / 09 / 08 10:59</p>`, time.Time{}, "", true},
		{"波浪号换全角", `<p>活动时间：2026/09/01 12:00 ～ 2026/09/08 10:59</p>`, at("2026-09-08 10:59"), annsync.StatusOK, false},
		{"时刻只到小时", `<p>活动时间：2026/09/01 12:00 ~ 2026/09/08 10:59 </p>`, at("2026-09-08 10:59"), annsync.StatusOK, false},
		// 起止倒挂：见 TestInvertedRangeBecomesPendingRecord（它产出 pending 事件，不是丢弃）。
		// 两位年份：不借年猜。
		{"两位年份", `<p>活动时间：26/09/01 12:00 ~ 26/09/08 10:59</p>`, time.Time{}, "", true},
		{"只有起点", `<p>活动时间：2026/09/01 12:00 起</p>`, time.Time{}, "", true},
		{"只有终点", `<p>活动时间：至 2026/09/08 10:59</p>`, time.Time{}, "", true},
		// 分隔符换成破折号：形状不认识，宁可报警。
		{"破折号分隔", `<p>活动时间：2026/09/01 12:00 — 2026/09/08 10:59</p>`, time.Time{}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := stellasora.Parse(syn("「变异」活动说明", c.body), testZone)
			if c.wantEnd.IsZero() {
				if len(r.Primary) != 0 {
					t.Errorf("不该采信，得到 %+v", r.Primary[0])
				}
				if c.suspect && !r.Suspect() {
					t.Errorf("该报警却没报（Weird=%v）", r.Weird)
				}
				return
			}
			if len(r.Primary) != 1 {
				t.Fatalf("主窗口数 = %d, want 1（%+v）", len(r.Primary), r.Primary)
			}
			iv := r.Primary[0]
			if !iv.End.Equal(c.wantEnd) {
				t.Errorf("结束 = %s, want %s", iv.End.Format("2006-01-02 15:04"), c.wantEnd.Format("2006-01-02 15:04"))
			}
			if c.status != "" && iv.Status != c.status {
				t.Errorf("状态 = %q, want %q", iv.Status, c.status)
			}
		})
	}
}

// 起止倒挂（官方写错或我们读错）产出 pending 事件：引擎不会让它进日历，
// 但记录会留在待确认桶里喊人。悄悄丢弃才是最坏的（日历上凭空少一个活动）。
func TestInvertedRangeBecomesPendingRecord(t *testing.T) {
	r := stellasora.Parse(syn("「写反了」活动说明",
		`<p>活动时间：2026/09/08 10:59 ~ 2026/09/01 12:00</p>`), testZone)

	if len(r.Primary) != 1 {
		t.Fatalf("主窗口数 = %d, want 1（pending 也要留下，别静默丢）", len(r.Primary))
	}
	if got := r.Primary[0].Status; got != annsync.StatusPending {
		t.Errorf("状态 = %q, want pending", got)
	}
	if r.Suspect() {
		t.Errorf("不该在条目级重复报警（记录级已经是 pending）：%s", r.Note())
	}
}

// 繁简混排：同一套 CMS 在繁中站用繁体词，词表两头都要认，否则正文整条被丢。
func TestTraditionalVariantsAlsoParse(t *testing.T) {
	r := stellasora.Parse(syn("「繁中形状」活動說明",
		`<p>▌開放時間：</p><p>活動時間：2026/09/01 12:00 ~ 2026/09/08 10:59</p>`+
			`<p>活動獎勵領取時間：2026/09/01 12:00 ~ 2026/09/11 03:59</p>`), testZone)

	if len(r.Primary) != 1 {
		t.Fatalf("繁体正文没解析出主窗口: %+v", r)
	}
	if len(r.Secondary) != 1 {
		t.Errorf("繁体「領取時間」没归进附属窗口: %+v", r.Secondary)
	}
}

// 名字只来自标题：正文里出现的角色名、卡池名、商城入口绝不能成为活动名。
func TestNameComesFromTitleOnly(t *testing.T) {
	r := stellasora.Parse(syn("「真活动名」活动说明",
		`<p>全新五星旅人「薇洛(盛夏)」与秘纹「浮光掠影」登场。</p>`+
			`<p>「时装橱窗」新增礼包。</p>`+
			`<p>▌活动时间</p><p>2026/09/01 12:00 ~ 2026/09/08 10:59</p>`), testZone)

	if r.Name != "真活动名" {
		t.Errorf("活动名 = %q, want 真活动名", r.Name)
	}
	if len(r.Primary) != 1 {
		t.Fatalf("主窗口 = %d, want 1", len(r.Primary))
	}
	// 散文里的三个「」都不该另开窗口。
	if n := len(r.Primary) + len(r.Secondary); n != 1 {
		t.Errorf("窗口总数 = %d, want 1（Secondary=%+v）", n, r.Secondary)
	}
}

// 标题取不到引号 → 与活动无关（维护说明、周边上新、支付中心…），
// 而且这类公告即便正文写着时间也不进待确认，否则每天公告都在桶里堆噪音。
func TestTitleWithoutQuotesIsNotAnAnnouncement(t *testing.T) {
	body := `<p>▌活动时间</p><p>2026/09/01 12:00 ~ 2026/09/08 10:59</p>`
	for _, title := range []string{
		"08月31日在线闪断更新",
		"《星塔旅人》 × 星际传奇 联动活动开启！",
		"创作团招募开启！",
		"[欢歌劲浪·闪耀假日惊涛探险！]版本一览",
		"「」空引号",
	} {
		r := stellasora.Parse(syn(title, body), testZone)
		if r.Suspect() {
			t.Errorf("%q 被判成待确认：标题不像活动公告就该整条不看", title)
		}
		if title != "「」空引号" && r.Name != "" {
			t.Errorf("%q 取到了名字 %q", title, r.Name)
		}
	}
}

// 名字抽取的形状边界。
func TestNameShapes(t *testing.T) {
	cases := []struct{ title, want string }{
		{"「缭乱碎晶球」活动说明", "缭乱碎晶球"},
		{"「出击海滨阵地！倾尽全力的碎瓜一击」说明", "出击海滨阵地！倾尽全力的碎瓜一击"},
		{"  「前后有空格」限时招募开启\t", "前后有空格"},
		{"『单书名号也算』活动说明", "单书名号也算"},
		{"公告「第一段」与「第二段」说明", "第一段"},
		{"「超长名字" + repeat("啊", 41) + "」活动说明", ""},
		{"「未闭合的活动说明", ""},
		{"没有引号的标题", ""},
		{"「」空引号", ""},
	}
	for _, c := range cases {
		t.Run(c.title, func(t *testing.T) {
			r := stellasora.Parse(syn(c.title,
				`<p>▌活动时间</p><p>2026/09/01 12:00 ~ 2026/09/08 10:59</p>`), testZone)
			if r.Name != c.want {
				t.Errorf("名字 = %q, want %q", r.Name, c.want)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// HTML 实体与 &nbsp; 都在真实正文里出现过。
func TestHTMLEntitiesDoNotBreakIntervals(t *testing.T) {
	r := stellasora.Parse(syn("「实体」活动说明",
		`<div class="announcement"><p>权限等级&ge;5</p>`+
			`<p>▌活动时间</p><p>2026/09/01&nbsp;12:00 ~ 2026/09/08 10:59</p></div>`), testZone)
	if len(r.Primary) != 1 {
		t.Fatalf("实体没解好，没解析出窗口: %+v", r)
	}
	if !r.Primary[0].End.Equal(at("2026-09-08 10:59")) {
		t.Errorf("结束 = %s", r.Primary[0].End)
	}
}

// 列表夹具逐行解码：形状自检（缺字段/零值必须在这里就炸，不要拖到同步里）。
func TestFixtureListRowsDecode(t *testing.T) {
	rows := readFixture(t, "list.json")["data"].(map[string]any)["rows"].([]any)
	if len(rows) == 0 {
		t.Fatal("list.json 没有行")
	}
	seen := map[int64]bool{}
	for _, raw := range rows {
		b, _ := json.Marshal(raw)
		var it stellasora.ListItem
		if err := json.Unmarshal(b, &it); err != nil {
			t.Fatalf("列表行解码: %v", err)
		}
		if it.ID == 0 || it.Title == "" || it.PublishTime == 0 {
			t.Fatalf("列表行形状不符: %+v", it)
		}
		if seen[it.ID] {
			t.Errorf("公告 %d 在夹具里重复", it.ID)
		}
		seen[it.ID] = true
	}
}
