package qq

import (
	"context"
	"strings"
	"testing"
	"time"
)

// mergeCount 数 mock 收到的 /files 合并次数：一次完整上传 = 一次合并。
func mergeCount(mm *mediaMock) int {
	n := 0
	mm.mu.Lock()
	defer mm.mu.Unlock()
	for _, p := range mm.paths {
		if strings.HasSuffix(p, "/files") {
			n++
		}
	}
	return n
}

func cachedClient(t *testing.T) (*mediaMock, *MediaCache) {
	t.Helper()
	mm, srv := newMediaMock(t, "app", "secret", 1<<20)
	return mm, NewMediaCache(mediaClient(t, srv), 0)
}

// TestMediaCacheReusesIdenticalBytes：同一段字节同一目标，第二次不该再走上传四步。
func TestMediaCacheReusesIdenticalBytes(t *testing.T) {
	mm, mc := cachedClient(t)
	data := []byte("same-image-bytes")

	r1, err := mc.UploadGroupImage(context.Background(), "G-1", "calendar.png", data)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := mc.UploadGroupImage(context.Background(), "G-1", "calendar.png", data)
	if err != nil {
		t.Fatal(err)
	}
	if got := mergeCount(mm); got != 1 {
		t.Fatalf("同字节同目标该只传一次，合并了 %d 次", got)
	}
	if r1.Cached {
		t.Error("第一次上传不该带 Cached 标记")
	}
	if !r2.Cached {
		t.Error("第二次命中缓存该带 Cached 标记，否则发送侧不知道能走兜底")
	}
	if r2.FileInfo != r1.FileInfo {
		t.Errorf("命中缓存的 file_info 该与上次一致：%q vs %q", r2.FileInfo, r1.FileInfo)
	}
}

// TestMediaCacheDistinctBytesUploadEach：字节不一样就是另一份文件，必须重传。
func TestMediaCacheDistinctBytesUploadEach(t *testing.T) {
	mm, mc := cachedClient(t)
	for _, data := range [][]byte{[]byte("bytes-A"), []byte("bytes-B"), []byte("bytes-A")} {
		if _, err := mc.UploadGroupImage(context.Background(), "G-1", "calendar.png", data); err != nil {
			t.Fatal(err)
		}
	}
	if got := mergeCount(mm); got != 2 {
		t.Fatalf("A、B、A 该传两次（A 第二次命中），合并了 %d 次", got)
	}
}

// TestMediaCacheHonorsExpiry：过了缓存有效期（平台 ttl 与自定上限取小）必须重传。
func TestMediaCacheHonorsExpiry(t *testing.T) {
	mm, srv := newMediaMock(t, "app", "secret", 1<<20)
	mc := NewMediaCache(mediaClient(t, srv), 30*time.Second)
	data := []byte("expiring")

	if _, err := mc.UploadGroupImage(context.Background(), "G-1", "calendar.png", data); err != nil {
		t.Fatal(err)
	}
	if got := mergeCount(mm); got != 1 {
		t.Fatalf("首次该传一次，合并了 %d 次", got)
	}
	// 把时钟拨过 30s 上限（mock 返回的 ttl=300 会被压到 30s）。
	mc.now = func() time.Time { return time.Now().Add(31 * time.Second) }
	if _, err := mc.UploadGroupImage(context.Background(), "G-1", "calendar.png", data); err != nil {
		t.Fatal(err)
	}
	if got := mergeCount(mm); got != 2 {
		t.Fatalf("过期后该重传，合并了 %d 次", got)
	}
}

// TestMediaCacheIsolationPerSceneAndTarget：群/单聊与不同目标各自独立，不许串用。
func TestMediaCacheIsolationPerSceneAndTarget(t *testing.T) {
	mm, mc := cachedClient(t)
	data := []byte("share-me")
	ctx := context.Background()

	if _, err := mc.UploadGroupImage(ctx, "ID-1", "calendar.png", data); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.UploadC2CImage(ctx, "ID-1", "calendar.png", data); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.UploadGroupImage(ctx, "ID-2", "calendar.png", data); err != nil {
		t.Fatal(err)
	}
	// 同一目标同场景的第二次：命中。
	r, err := mc.UploadGroupImage(ctx, "ID-1", "calendar.png", data)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Cached {
		t.Error("同群同字节第三次该命中缓存")
	}
	if got := mergeCount(mm); got != 3 {
		t.Fatalf("三个互不相干的键各传一次，合并了 %d 次", got)
	}
}

// TestMediaCacheForgetDropsEntry：被平台判死的引用摘掉后，下一趟必须重传。
func TestMediaCacheForgetDropsEntry(t *testing.T) {
	mm, mc := cachedClient(t)
	data := []byte("poisoned")

	r1, err := mc.UploadGroupImage(context.Background(), "G-1", "calendar.png", data)
	if err != nil {
		t.Fatal(err)
	}
	mc.Forget(r1)
	if _, err := mc.UploadGroupImage(context.Background(), "G-1", "calendar.png", data); err != nil {
		t.Fatal(err)
	}
	if got := mergeCount(mm); got != 2 {
		t.Fatalf("Forget 之后该重传，合并了 %d 次", got)
	}
}

// TestIsPlatformRejection：只有"平台明确回拒"才算可重试，超时/5xx 不算。
func TestIsPlatformRejection(t *testing.T) {
	rejected := []error{
		&APIError{HTTPStatus: 400, Message: "坏请求"},
		&APIError{HTTPStatus: 200, Code: 11244, Message: "带 err_code 的 200"},
	}
	for _, err := range rejected {
		if !IsPlatformRejection(err) {
			t.Errorf("%v 该算平台明确回拒", err)
		}
	}
	notRejected := []error{
		&APIError{HTTPStatus: 502, Message: "上游挂了"},
		&APIError{HTTPStatus: 504, Message: "超时"},
		errTestNetwork, // 网络错：可能已送达
	}
	for _, err := range notRejected {
		if IsPlatformRejection(err) {
			t.Errorf("%v 不该算平台明确回拒", err)
		}
	}
}

type errNetwork struct{ msg string }

func (e errNetwork) Error() string { return e.msg }

var errTestNetwork = errNetwork{msg: "connection reset"}
