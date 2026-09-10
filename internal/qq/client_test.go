package qq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockPlatform struct {
	mu sync.Mutex

	appID     string
	secret    string
	issued    []string
	tokenReqs int

	// rejectTokensLeft 表示还有几次请求要先以 401 拒绝，用于验证凭证刷新重试
	rejectTokensLeft int

	// tokenCode 非零时，取 token 直接返回 HTTP 200 + 只有 code 字段的失败体（真实平台行为）
	tokenCode int

	calls      []recordedCall
	gotHeaders []string
}

type recordedCall struct {
	path  string
	body  map[string]any
	token string
}

// 官方 autogen/users_me.get 页「响应示例」原样抄录。
// 用它而不是自造形状，是为了让字段名对着真实契约校验 —— 上一版就是自造了 appid/bot_name
// 才让测试全绿却漏掉 Me() 一直返回空结构体。
const usersMeFixture = `{"id":"5777414462219517083","username":"阳光小助手","avatar":"https://thirdqq.qlogo.cn/g?b=oidb&k=AbCdEfGhIjKlMnOpQrStUv&kti=xyzABC&s=0&t=1781676795","bot":true,"union_openid":"9F2E872045CCCC5948BEAF5B5FCCDF22","union_user_account":"","share_url":"https://qun.qq.com/qunpro/robot/qunshare?robot_uin=3889007780&robot_appid=102083127&biz_type=0","welcome_msg":"欢迎加入我们的群聊"}`

func newMockPlatform(t *testing.T, appID, secret string) (*mockPlatform, *httptest.Server) {
	t.Helper()
	mp := &mockPlatform{appID: appID, secret: secret}
	srv := httptest.NewServer(http.HandlerFunc(mp.serve))
	t.Cleanup(srv.Close)
	return mp, srv
}

func (mp *mockPlatform) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/app/getAppAccessToken":
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mp.mu.Lock()
		mp.tokenReqs++
		mp.issued = append(mp.issued, "tok")
		fakeCode := mp.tokenCode
		mp.mu.Unlock()
		if fakeCode != 0 {
			writeJSON(w, map[string]any{"code": fakeCode, "message": "appid invalid"})
			return
		}
		if req["appId"] != mp.appID || req["clientSecret"] != mp.secret {
			// 官方文档参数表用的是 appId / clientSecret 的驼峰写法
			writeJSON(w, map[string]any{"err_code": 100016, "message": "invalid appid or secret"})
			return
		}
		// 返回示例里 expires_in 是字符串，参数表里写的是 number —— 按字符串发以贴近真实
		writeJSON(w, map[string]any{"access_token": "AT-" + mp.appID, "expires_in": "7200"})
		return

	case "/users/@me":
		mp.mu.Lock()
		mp.calls = append(mp.calls, recordedCall{path: r.URL.Path, token: r.Header.Get("Authorization")})
		mp.mu.Unlock()
		if r.Header.Get("Authorization") != "QQBot AT-"+mp.appID {
			http.Error(w, `{"err_code":11244,"message":"token invalid"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(usersMeFixture))
		return

	default:
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mp.mu.Lock()
		mp.calls = append(mp.calls, recordedCall{path: r.URL.EscapedPath(), body: body, token: r.Header.Get("Authorization")})
		reject := mp.rejectTokensLeft > 0
		if reject {
			mp.rejectTokensLeft--
		}
		mp.mu.Unlock()

		if reject {
			http.Error(w, `{"err_code":11244,"message":"token expire or not exist"}`, http.StatusUnauthorized)
			return
		}
		// 平台也会用 HTTP 200 + err_code 表达业务失败（msg_id 过期即为一例）
		if ref, ok := body["message_reference"].(map[string]any); ok &&
			ref["message_id"] == "simulate_expired_msg_id" {
			writeJSON(w, map[string]any{"err_code": 40034005, "message": "回复消息msg_id已过期", "trace_id": "abc"})
			return
		}
		writeJSON(w, map[string]any{"id": "ROBOT1.0_test", "timestamp": "2026-07-21T10:30:00+08:00"})
	}
}

func (mp *mockPlatform) sendCalls() []recordedCall {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	return append([]recordedCall(nil), mp.calls...)
}

func (mp *mockPlatform) tokenRequests() int {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	return mp.tokenReqs
}

func newTestClient(t *testing.T, appID, secret string) (*Client, *mockPlatform) {
	t.Helper()
	mp, srv := newMockPlatform(t, appID, secret)
	return NewClientAt(appID, secret, srv.URL), mp
}

func TestTokenUsesDocumentedFieldNamesAndAuthHeader(t *testing.T) {
	c, mp := newTestClient(t, "123456", "sec")
	u, err := c.Me(context.Background())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if u.ID != "5777414462219517083" || u.Username != "阳光小助手" || !u.Bot {
		t.Errorf("未能按官方响应示例解析 /users/@me: %+v", u)
	}
	if got := mp.tokenRequests(); got != 1 {
		t.Errorf("取 token 次数 = %d, want 1", got)
	}
}

func TestTokenIsCachedAcrossCalls(t *testing.T) {
	c, mp := newTestClient(t, "123456", "sec")
	for i := 0; i < 3; i++ {
		if _, err := c.Me(context.Background()); err != nil {
			t.Fatalf("Me %d: %v", i, err)
		}
	}
	if got := mp.tokenRequests(); got != 1 {
		t.Errorf("3 次调用后取 token 次数 = %d, want 1", got)
	}
}

// 官方规则：到期前 60s 内重新获取才会拿到新 token，所以提前量之内必须重取。
func TestTokenRefetchedInsideExpiryWindow(t *testing.T) {
	c, mp := newTestClient(t, "123456", "sec")
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.tokens.now = func() time.Time {
		return time.Now().Add(7200*time.Second - 30*time.Second) // 距过期只剩 30s
	}
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := mp.tokenRequests(); got != 2 {
		t.Errorf("进入 60s 提前量后取 token 次数 = %d, want 2", got)
	}
}

func TestStringAndNumberExpiresInBothAccepted(t *testing.T) {
	for _, raw := range []string{`{"access_token":"a","expires_in":"7200"}`, `{"access_token":"a","expires_in":7200}`} {
		var tr tokenResponse
		if err := json.Unmarshal([]byte(raw), &tr); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if int64(tr.ExpiresIn) != 7200 {
			t.Errorf("%s: expires_in = %d, want 7200", raw, int64(tr.ExpiresIn))
		}
	}
	var bad tokenResponse
	if err := json.Unmarshal([]byte(`{"access_token":"a","expires_in":"x"}`), &bad); err == nil {
		t.Error("非数字 expires_in: err = nil, want error")
	}
}

func TestRetryOnceAfterTokenRejected(t *testing.T) {
	c, mp := newTestClient(t, "123456", "sec")
	mp.mu.Lock()
	mp.rejectTokensLeft = 1
	mp.mu.Unlock()

	if _, err := c.SendGroupMessage(context.Background(), "GROUP_OPENID",
		SendRequest{MsgType: MsgTypeText, Content: "hi"}); err != nil {
		t.Fatalf("重试后仍失败: %v", err)
	}
	if got := mp.tokenRequests(); got != 2 {
		t.Errorf("取 token 次数 = %d, want 2（首次 + 401 后重取）", got)
	}
	var sent *recordedCall
	for i := range mp.sendCalls() {
		call := mp.sendCalls()[i]
		if strings.Contains(call.path, "/messages") {
			sent = &call
		}
	}
	if sent == nil {
		t.Fatal("重放后没有发出任何群消息请求")
	}
	if sent.body["content"] != "hi" {
		t.Errorf("重放的 body content = %v, want hi", sent.body["content"])
	}
}

func TestErrCodeOnHTTP200IsTreatedAsFailure(t *testing.T) {
	c, _ := newTestClient(t, "123456", "sec")
	req := SendRequest{MsgType: MsgTypeText, Content: "hi", MsgID: "ROBOT1.0_old", MsgSeq: 1}
	// 借 mock 的旁路字段让平台回 HTTP 200 + err_code
	req.Reference = &MessageReference{MessageID: "simulate_expired_msg_id"}
	_, err := c.SendGroupMessage(context.Background(), "GID", req)
	if err == nil {
		t.Fatal("err = nil, want err_code 40034005")
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("错误类型 %T, want *APIError", err)
	}
	if apiErr.ErrCode != 40034005 {
		t.Errorf("ErrCode = %d, want 40034005", apiErr.ErrCode)
	}
	if apiErr.HTTPStatus != http.StatusOK {
		t.Errorf("HTTPStatus = %d, want 200", apiErr.HTTPStatus)
	}
}

func asAPIError(err error, out **APIError) bool {
	if e, ok := err.(*APIError); ok {
		*out = e
		return true
	}
	return false
}

func TestSendGroupMessageBodyShape(t *testing.T) {
	c, mp := newTestClient(t, "123456", "sec")
	if _, err := c.SendGroupReply(context.Background(), "GRP_OPENID_1", "ROBOT1.0_src",
		SendRequest{MsgType: MsgTypeText, Content: "活动还有 24 小时结束"}); err != nil {
		t.Fatal(err)
	}
	calls := mp.sendCalls()
	if len(calls) != 1 {
		t.Fatalf("请求数 = %d, want 1", len(calls))
	}
	body := calls[0].body
	want := map[string]any{
		"msg_type": float64(0),
		"content":  "活动还有 24 小时结束",
		"msg_id":   "ROBOT1.0_src",
		"msg_seq":  float64(1),
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("body[%q] = %v, want %v", k, body[k], v)
		}
	}
	if len(body) != len(want) {
		t.Errorf("请求体多出字段: %v", body)
	}
	if !strings.Contains(calls[0].path, "/v2/groups/GRP_OPENID_1/messages") {
		t.Errorf("path = %s", calls[0].path)
	}
}

// 平台对相同 msg_id + msg_seq 重复发送会失败，且群聊每条消息最多回 5 次。
func TestMsgSeqIncrementsAndReplyLimitEnforced(t *testing.T) {
	c, mp := newTestClient(t, "123456", "sec")
	for i := 1; i <= MaxGroupReplies; i++ {
		if _, err := c.SendGroupReply(context.Background(), "GID", "MID",
			SendRequest{MsgType: MsgTypeText, Content: "r"}); err != nil {
			t.Fatalf("第 %d 次回复失败: %v", i, err)
		}
	}
	for i, call := range mp.sendCalls() {
		if got := call.body["msg_seq"]; got != float64(i+1) {
			t.Errorf("第 %d 次请求 msg_seq = %v, want %d", i+1, got, i+1)
		}
	}
	_, err := c.SendGroupReply(context.Background(), "GID", "MID",
		SendRequest{MsgType: MsgTypeText, Content: "r"})
	if err == nil {
		t.Fatal("超出回复上限却返回 nil error")
	}
	if !strings.Contains(err.Error(), "上限") {
		t.Errorf("错误信息 = %v, 期望提到上限", err)
	}
	if got := len(mp.sendCalls()); got != MaxGroupReplies {
		t.Errorf("第 6 次仍然打到了平台: 请求数 = %d, want %d", got, MaxGroupReplies)
	}
}

func TestValidateRejectsDocumentedFieldConflicts(t *testing.T) {
	cases := []struct {
		name string
		req  SendRequest
	}{
		{"markdown 时 content 非空", SendRequest{MsgType: MsgTypeMarkdown, Content: "x", Markdown: &Markdown{Content: "m"}}},
		{"markdown 缺内容", SendRequest{MsgType: MsgTypeMarkdown}},
		{"文本带 markdown", SendRequest{MsgType: MsgTypeText, Content: "x", Markdown: &Markdown{Content: "m"}}},
		{"文本空内容", SendRequest{MsgType: MsgTypeText}},
		{"富媒体缺 file_info", SendRequest{MsgType: MsgTypeMedia}},
		{"msg_id 与 event_id 同时给", SendRequest{MsgType: MsgTypeText, Content: "x", MsgID: "m", EventID: "e"}},
		{"未知 msg_type", SendRequest{MsgType: 99, Content: "x"}},
	}
	for _, tc := range cases {
		req := tc.req
		if err := req.validate(); err == nil {
			t.Errorf("%s: validate 放过了非法请求", tc.name)
		}
	}
	ok := SendRequest{MsgType: MsgTypeText, Content: "x", MsgID: "m", MsgSeq: 1}
	if err := ok.validate(); err != nil {
		t.Errorf("合法请求被拒: %v", err)
	}
}

// 正确的不变量是：openid 里的分隔符必须编码成 %2F，使它无法注入新的路径段。
// 解码后的文本里出现 ".." 是无害的，因为那已经是一个整体段内的字符。
func TestOpenIDCannotInjectPathSegments(t *testing.T) {
	c, mp := newTestClient(t, "123456", "sec")
	if _, err := c.SendGroupMessage(context.Background(), "abc/../../evil",
		SendRequest{MsgType: MsgTypeText, Content: "x"}); err != nil {
		t.Fatal(err)
	}
	calls := mp.sendCalls()
	wire := calls[len(calls)-1].path
	if !strings.Contains(wire, "%2F") {
		t.Errorf("分隔符未编码，存在路径注入风险: %s", wire)
	}
	if got := strings.Count(wire, "/"); got != 4 {
		t.Errorf("路径段数被改写（字面 / 有 %d 个，want 4）: %s", got, wire)
	}
}

// 回归：真实平台在 appId 无效时回 HTTP 200 + {"code":100007,"message":"appid invalid"}，
// 只有遗留的 code 字段、没有 err_code。早前版本会把它吞成"返回空凭证"，丢掉真实原因。
func TestTokenFailureWithLegacyCodeFieldIsSurfaced(t *testing.T) {
	const secret = "REAL_SECRET_MUST_NEVER_APPEAR_IN_ERROR"
	mp, srv := newMockPlatform(t, "BAD_APPID", secret)
	mp.mu.Lock()
	mp.tokenCode = 100007
	mp.mu.Unlock()

	_, err := NewClientAt("BAD_APPID", secret, srv.URL).Me(context.Background())
	if err == nil {
		t.Fatal("err = nil, want code 100007")
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("错误类型 %T, want *APIError (%v)", err, err)
	}
	if apiErr.Code != 100007 {
		t.Errorf("Code = %d, want 100007", apiErr.Code)
	}
	if apiErr.HTTPStatus != http.StatusOK {
		t.Errorf("HTTPStatus = %d, want 200", apiErr.HTTPStatus)
	}
	if !strings.Contains(err.Error(), "100007") {
		t.Errorf("错误信息未透出平台 code: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("错误信息泄露了 clientSecret: %v", err)
	}
}

// 变异负对照：HTTP 200 但响应体没有 id（例如平台改了字段名），
// 必须报错，不能像上一版那样静默返回一个全空结构体让调用方以为拿到了身份。
func TestMeErrorsWhenResponseLacksID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if strings.HasSuffix(r.URL.Path, "getAppAccessToken") {
			_, _ = w.Write([]byte(`{"access_token":"AT","expires_in":"7200"}`))
			return
		}
		_, _ = w.Write([]byte(`{"appid":"123","bot_name":"x"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := NewClientAt("id", "sec", srv.URL).Me(context.Background())
	if err == nil {
		t.Fatal("err = nil, want 响应结构不符的错误")
	}
	if !strings.Contains(err.Error(), "结构") {
		t.Errorf("err = %v, 期望提示响应结构不符", err)
	}
}

// 验证被动回复计数器：
// 1. 相同 msg_id 自动递增 seq；达到 limit 报错；
// 2. 不同 msg_id 计数隔离；
// 3. 超过 2 小时的历史消息记录在下一次清理周期被自动淘汰，防止长期运行内存单调爬升；
// 4. 2 小时以内的活跃消息保留。
func TestReplierSequenceAndExpiration(t *testing.T) {
	curr := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return curr }

	r := newReplierWithClock(now)

	// 1. 基本递增与上限测试
	for i := 1; i <= 3; i++ {
		seq, err := r.next("msgA", 3)
		if err != nil {
			t.Fatalf("第 %d 次 next 失败: %v", i, err)
		}
		if seq != i {
			t.Errorf("seq = %d, want %d", seq, i)
		}
	}
	if _, err := r.next("msgA", 3); err == nil {
		t.Errorf("超过上限未报错")
	}

	// 2. 独立消息隔离
	seqB, err := r.next("msgB", 5)
	if err != nil || seqB != 1 {
		t.Errorf("msgB seq = %d (err=%v), want 1", seqB, err)
	}

	// 3. 时间快进：30 分钟后回复 msgC
	curr = curr.Add(30 * time.Minute)
	seqC, err := r.next("msgC", 5)
	if err != nil || seqC != 1 {
		t.Errorf("msgC seq = %d (err=%v), want 1", seqC, err)
	}

	// 此时 map 中有 msgA (t0), msgB (t0), msgC (t0+30m)
	r.mu.Lock()
	if len(r.seq) != 3 {
		t.Errorf("map 长度 = %d, want 3", len(r.seq))
	}
	r.mu.Unlock()

	// 4. 时间快进到 t0 + 2h + 1m（此时 msgA/msgB 均已超过 2 小时，msgC 只过了 1.5 小时）
	curr = time.Date(2026, 9, 10, 14, 1, 0, 0, time.UTC)
	// 触发一次 next，应当带动 lazy 清理
	seqD, err := r.next("msgD", 5)
	if err != nil || seqD != 1 {
		t.Errorf("msgD seq = %d (err=%v), want 1", seqD, err)
	}

	r.mu.Lock()
	_, hasA := r.seq["msgA"]
	_, hasB := r.seq["msgB"]
	_, hasC := r.seq["msgC"]
	_, hasD := r.seq["msgD"]
	r.mu.Unlock()

	if hasA || hasB {
		t.Errorf("超过 2 小时的 msgA/msgB 应当被清理淘汰: hasA=%v, hasB=%v", hasA, hasB)
	}
	if !hasC {
		t.Errorf("未满 2 小时的 msgC 应当依然保留")
	}
	if !hasD {
		t.Errorf("刚插入的 msgD 应当保留")
	}
}
