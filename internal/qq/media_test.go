package qq

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 官方文档的响应示例（富媒体 autogen 页），字段名与嵌套形状照抄，只有 presigned_url
// 改指向测试服务器、block_size 改小以便真的切出 3 片。自造形状会让测试全绿而契约已错。
const filesRespFixture = `{"file_uuid":"uuid_a1b2c3d4e5f6","file_info":"AE86C5D3F0E14B238C656C0F6DD1D0479C","ttl":300}`

type mediaMock struct {
	mu sync.Mutex

	blockSize int
	// omitFileInfo 让合并接口回一个没有 file_info 的 200，用来验字段守卫
	omitFileInfo bool

	prepBody  map[string]any
	mergeBody map[string]any
	finishes  []map[string]any
	puts      []recordedPut
	paths     []string
}

type recordedPut struct {
	token string
	data  []byte
}

func newMediaMock(t *testing.T, appID, secret string, blockSize int) (*mediaMock, *httptest.Server) {
	t.Helper()
	mm := &mediaMock{blockSize: blockSize}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			data, _ := readAllForTest(r)
			mm.mu.Lock()
			mm.puts = append(mm.puts, recordedPut{token: r.Header.Get("Authorization"), data: data})
			mm.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		switch {
		case r.URL.Path == "/app/getAppAccessToken":
			writeJSON(w, map[string]any{"access_token": "AT-" + appID, "expires_in": "7200"})
		case strings.HasSuffix(r.URL.Path, "/upload_prepare"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			parts := make([]map[string]any, 0, 3)
			size, _ := strconv.Atoi(body["file_size"].(string))
			n := (size + blockSize - 1) / blockSize
			for i := 0; i < n; i++ {
				parts = append(parts, map[string]any{
					"index":         i,
					"presigned_url": srv.URL + "/upload?partNumber=" + strconv.Itoa(i+1),
					"block_size":    strconv.Itoa(blockSize),
				})
			}
			mm.mu.Lock()
			mm.prepBody = body
			mm.paths = append(mm.paths, r.URL.Path)
			mm.mu.Unlock()
			writeJSON(w, map[string]any{
				"upload_id":  "upload_a1b2c3d4e5f6",
				"block_size": strconv.Itoa(blockSize),
				"parts":      parts,
				"upload_config": map[string]any{
					"concurrency":   1,
					"retry_timeout": 300,
					"retry_delay":   1,
				},
			})
		case strings.HasSuffix(r.URL.Path, "/upload_part_finish"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mm.mu.Lock()
			mm.finishes = append(mm.finishes, body)
			mm.paths = append(mm.paths, r.URL.Path)
			mm.mu.Unlock()
			writeJSON(w, map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/files"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mm.mu.Lock()
			mm.mergeBody = body
			mm.paths = append(mm.paths, r.URL.Path)
			omit := mm.omitFileInfo
			mm.mu.Unlock()
			if omit {
				writeJSON(w, map[string]any{"file_uuid": "uuid_x"})
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(filesRespFixture))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return mm, srv
}

func readAllForTest(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return readLimited(r.Body, 1<<20)
}

func mediaClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return NewClientAt("app", "secret", srv.URL)
}

// TestUploadImageChunks 盯的是整条分片链路：预上传字段齐全且长度按字符串发、
// 每片真的按 block_size 切开送到预签名 URL（且不带机器人 token）、每片都有完成确认、
// 最后合并请求带 upload_id 而不带 url。
func TestUploadImageChunks(t *testing.T) {
	const blockSize = 1024
	data := make([]byte, 2500)
	for i := range data {
		data[i] = byte(i % 251)
	}
	mm, srv := newMediaMock(t, "app", "secret", blockSize)
	c := mediaClient(t, srv)

	ref, err := c.UploadGroupImage(context.Background(), "G-1", "calendar.png", data)
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if ref.FileInfo == "" || ref.FileUUID == "" {
		t.Fatalf("返回缺字段: %+v", ref)
	}
	if ref.TTL != 300*time.Second {
		t.Errorf("ttl 解析成了 %v，期望 300s", ref.TTL)
	}

	want := []string{"/v2/groups/G-1/upload_prepare", "/v2/groups/G-1/upload_part_finish",
		"/v2/groups/G-1/upload_part_finish", "/v2/groups/G-1/upload_part_finish", "/v2/groups/G-1/files"}
	if strings.Join(mm.paths, ",") != strings.Join(want, ",") {
		t.Errorf("调用序列不对\n got %v\nwant %v", mm.paths, want)
	}

	if mm.prepBody["file_size"] != "2500" {
		t.Errorf("file_size 应为字符串 \"2500\"，实得 %#v", mm.prepBody["file_size"])
	}
	if mm.prepBody["file_type"] != float64(fileTypeImage) {
		t.Errorf("file_type 应为 %d，实得 %#v", fileTypeImage, mm.prepBody["file_type"])
	}
	if mm.prepBody["file_name"] != "calendar.png" {
		t.Errorf("file_name 实得 %#v", mm.prepBody["file_name"])
	}
	// 文件不足 10002432 字节时，md5_10m 就等于整文件 MD5
	if want := hexOfMD5(data); mm.prepBody["md5"] != want || mm.prepBody["md5_10m"] != want {
		t.Errorf("md5/md5_10m 不对: %v / %v，期望 %s", mm.prepBody["md5"], mm.prepBody["md5_10m"], want)
	}
	if want := hexOfSHA1(data); mm.prepBody["sha1"] != want {
		t.Errorf("sha1 不对: %v，期望 %s", mm.prepBody["sha1"], want)
	}

	if len(mm.puts) != 3 {
		t.Fatalf("预签名 PUT 次数 = %d，期望 3", len(mm.puts))
	}
	for i, p := range mm.puts {
		// 预签名 URL 自带签名，再挂一个 QQBot token 会让 COS 报 SignatureDoesNotMatch
		if p.token != "" {
			t.Errorf("分片 %d 的 PUT 带了 Authorization: %s", i, p.token)
		}
		start, end := i*blockSize, (i+1)*blockSize
		if end > len(data) {
			end = len(data)
		}
		if string(p.data) != string(data[start:end]) {
			t.Errorf("分片 %d 内容不对，长度 %d，期望 %d", i, len(p.data), end-start)
		}
	}
	if len(mm.finishes) != 3 {
		t.Fatalf("分片完成确认次数 = %d，期望 3", len(mm.finishes))
	}
	for i, f := range mm.finishes {
		if f["upload_id"] != "upload_a1b2c3d4e5f6" {
			t.Errorf("分片 %d 的 upload_id 不对: %#v", i, f["upload_id"])
		}
		if f["part_index"] != float64(i) {
			t.Errorf("part_index = %#v，期望 %d", f["part_index"], i)
		}
		start, end := i*blockSize, (i+1)*blockSize
		if end > len(data) {
			end = len(data)
		}
		if got, want := f["block_size"], strconv.Itoa(end-start); got != want {
			t.Errorf("分片 %d 的 block_size = %#v，期望 %s（应为该片实际长度）", i, got, want)
		}
		if got, want := f["md5"], hexOfMD5(data[start:end]); got != want {
			t.Errorf("分片 %d 的 md5 = %v，期望 %s", i, got, want)
		}
	}
	if _, ok := mm.mergeBody["url"]; ok {
		t.Errorf("分片合并不该带 url，实得 %#v", mm.mergeBody["url"])
	}
	if mm.mergeBody["upload_id"] != "upload_a1b2c3d4e5f6" {
		t.Errorf("合并缺 upload_id: %#v", mm.mergeBody["upload_id"])
	}
	// srv_send_msg=true 会占主动消息频次，我们必须显式 false
	if v, ok := mm.mergeBody["srv_send_msg"]; !ok || v != false {
		t.Errorf("srv_send_msg = %#v (present=%v)，期望显式 false", mm.mergeBody["srv_send_msg"], ok)
	}
}

// TestUploadImageRejectsEmptyInfo 挡住"HTTP 200 但字段对不上被当成成功"。
func TestUploadImageRejectsEmptyInfo(t *testing.T) {
	mm, srv := newMediaMock(t, "app", "secret", 1<<20)
	mm.omitFileInfo = true
	c := mediaClient(t, srv)

	_, err := c.UploadGroupImage(context.Background(), "G-1", "x.png", []byte("abc"))
	if err == nil || !strings.Contains(err.Error(), "file_info") {
		t.Fatalf("应因缺 file_info 报错，实得 %v", err)
	}
}

// TestUploadC2CUsesUserEndpoint 记的是"上传的文件不能跨场景使用"这条约束在代码里的落点。
func TestUploadC2CUsesUserEndpoint(t *testing.T) {
	mm, srv := newMediaMock(t, "app", "secret", 1<<20)
	c := mediaClient(t, srv)

	if _, err := c.UploadC2CImage(context.Background(), "U-9", "x.png", []byte("abc")); err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	for _, p := range mm.paths {
		if !strings.HasPrefix(p, "/v2/users/U-9/") {
			t.Errorf("单聊上传走到了 %s", p)
		}
	}
}

func TestUploadImageGuardsInput(t *testing.T) {
	_, srv := newMediaMock(t, "app", "secret", 1<<20)
	c := mediaClient(t, srv)
	if _, err := c.UploadGroupImage(context.Background(), "", "x.png", []byte("abc")); err == nil {
		t.Error("空 openid 应当被挡")
	}
	if _, err := c.UploadGroupImage(context.Background(), "G-1", "x.png", nil); err == nil {
		t.Error("空文件应当被挡，不必白跑一次预上传")
	}
}
