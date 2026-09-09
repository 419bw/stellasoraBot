package qq

import (
	"context"
	"sync"
	"time"
)

// MediaCache 在平台说的有效期内按内容复用 file_info：同一段字节发给同一目标时，
// 跳过 upload_prepare → PUT → part_finish → files 那四步，直接拿上次的引用去发。
//
// 键 = 场景|目标|sha1(字节)。日历图这类内容按 5 分钟桶重渲、桶间字节经常原样，
// 拿内容哈希当键让"能不能复用"由字节说了算：一样就复用、不一样就重传，不用猜
// 也绝不会把过期画面发出去。
//
// 平台规定用群接口上传的文件只能发群，所以键里带场景与目标，跨场景绝不共享。
// 内存里放、不落盘：几分钟十几分钟的东西，重启后大概率已作废。
type MediaCache struct {
	*Client

	maxTTL time.Duration // 平台 ttl 之外的自定上限
	now    func() time.Time

	mu    sync.Mutex
	items map[string]cacheEntry
}

type cacheEntry struct {
	ref      MediaRef
	expireAt time.Time
}

// DefaultMediaTTL 是默认自定上限。平台实测回 ttl=24h，官方文档示例却写 300 秒，
// 差得离谱说明这个数不能全信，压个上限兜底：真失效了还有发送失败摘缓存重传。
const DefaultMediaTTL = 30 * time.Minute

// mediaCacheMax 是条目上限（每条 1 KB 上下），超了丢最早过期的一条。
const mediaCacheMax = 32

func NewMediaCache(c *Client, maxTTL time.Duration) *MediaCache {
	if maxTTL <= 0 {
		maxTTL = DefaultMediaTTL
	}
	return &MediaCache{
		Client: c,
		maxTTL: maxTTL,
		now:    time.Now,
		items:  map[string]cacheEntry{},
	}
}

// UploadGroupImage 与 *Client 同签名，命中缓存就跳过上传四步。Cached 标记打在返回的
// MediaRef 上，发送逻辑据此决定被拒后要不要摘缓存重传。
func (m *MediaCache) UploadGroupImage(ctx context.Context, groupOpenID, fileName string, data []byte) (MediaRef, error) {
	return m.image(ctx, "群", groupOpenID, fileName, data, m.Client.UploadGroupImage)
}

// UploadC2CImage 是单聊版，缓存与群聊分开。
func (m *MediaCache) UploadC2CImage(ctx context.Context, userOpenID, fileName string, data []byte) (MediaRef, error) {
	return m.image(ctx, "单聊", userOpenID, fileName, data, m.Client.UploadC2CImage)
}

func (m *MediaCache) image(ctx context.Context, scene, target, fileName string, data []byte,
	fresh func(context.Context, string, string, []byte) (MediaRef, error)) (MediaRef, error) {
	key := scene + "|" + target + "|" + hexOfSHA1(data)

	m.mu.Lock()
	if e, ok := m.items[key]; ok && m.now().Before(e.expireAt) {
		m.mu.Unlock()
		ref := e.ref
		ref.Cached = true
		return ref, nil
	}
	m.mu.Unlock()

	ref, err := fresh(ctx, target, fileName, data)
	if err != nil {
		return MediaRef{}, err
	}
	m.put(key, ref)
	return ref, nil
}

func (m *MediaCache) put(key string, ref MediaRef) {
	ttl := ref.TTL
	if ttl <= 0 || ttl > m.maxTTL {
		ttl = m.maxTTL
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	if len(m.items) >= mediaCacheMax {
		oldest := ""
		for k, v := range m.items {
			if oldest == "" || v.expireAt.Before(m.items[oldest].expireAt) {
				oldest = k
			}
		}
		delete(m.items, oldest)
	}
	m.items[key] = cacheEntry{ref: ref, expireAt: m.now().Add(ttl)}
}

func (m *MediaCache) pruneLocked() {
	for k, v := range m.items {
		if !m.now().Before(v.expireAt) {
			delete(m.items, k)
		}
	}
}

// Forget 把一份被平台判死的 file_info 从缓存里摘掉。没缓存的调用方（或没命中过）
// 调它也是安全空操作。
func (m *MediaCache) Forget(ref MediaRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.items {
		if v.ref.FileInfo == ref.FileInfo {
			delete(m.items, k)
			return
		}
	}
}
