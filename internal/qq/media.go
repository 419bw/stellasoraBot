package qq

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// 富媒体上传。官方文档《富媒体消息概述》（上次更新 2026-07-21）+ autogen 端点页。
//
// 两条上传路径里只有分片上传能用：URL 直传要求文件公网可访问，我们那张日历图是本地
// 现画的。所以流程固定四步：
//
//	upload_prepare → 逐片 PUT 预签名 URL → upload_part_finish → files(upload_id)
//
// 返回的 file_info 是不透明字符串，发消息时原样塞进 media.file_info（msg_type=7）。
// 它带 ttl（实测本 bot 上传返回 24h，官方文档示例只写 300 秒）且**不能跨场景复用**
// （群上传的只能发群）。同场景、没过期就能重复拿去发——MediaCache 就是干这个的。
// 谁用谁传的调用方仍要"拿到就发"，不要在队列里排队等过期。

// file_type 取值，来自「文件类型与限制」。
const (
	fileTypeImage = 1
)

// md5_10m 的窗口：文件前 10002432 字节（约 9.54 MB）的 MD5，服务端用它做秒传判断。
const md5QuickPrefix = 10002432

// 预上传与分片完成接口 10 QPS、合并接口 50 QPS，我们一次发送各调一遍，用不到并发。
// 分片默认 5 MB，我们的图约 330-530 KB，实际只有一个分片。
const defaultBlockSize = 5 << 20

// MediaRef 是一次上传的结果。
type MediaRef struct {
	FileUUID string
	FileInfo string
	TTL      time.Duration // 0 = 平台说可长期使用
	// Cached 由 MediaCache 填：这份引用来自本地缓存而不是刚走完四步上传。
	// 平台侧没有这个字段，发送逻辑靠它决定"被拒后要不要摘缓存重传"。
	Cached bool
}

type preparePart struct {
	Index        int    `json:"index"`
	PresignedURL string `json:"presigned_url"`
	BlockSize    string `json:"block_size"`
}

type prepareResp struct {
	UploadID  string        `json:"upload_id"`
	BlockSize string        `json:"block_size"`
	Parts     []preparePart `json:"parts"`
}

type filesResp struct {
	FileUUID string `json:"file_uuid"`
	FileInfo string `json:"file_info"`
	TTL      int    `json:"ttl"`
}

// UploadGroupImage 把一张图片上传到群，返回可直接用于 SendGroupMessage 的引用。
func (c *Client) UploadGroupImage(ctx context.Context, groupOpenID, fileName string, data []byte) (MediaRef, error) {
	return c.uploadImage(ctx, "群", "groups", groupOpenID, fileName, data)
}

// UploadC2CImage 是单聊版。端点与群聊相互独立，上传过的文件不能跨场景发送。
func (c *Client) UploadC2CImage(ctx context.Context, userOpenID, fileName string, data []byte) (MediaRef, error) {
	return c.uploadImage(ctx, "单聊", "users", userOpenID, fileName, data)
}

func (c *Client) uploadImage(ctx context.Context, scene, segment, target, fileName string, data []byte) (MediaRef, error) {
	if target == "" {
		return MediaRef{}, fmt.Errorf("qq: %s上传缺少目标 openid", scene)
	}
	if len(data) == 0 {
		return MediaRef{}, fmt.Errorf("qq: %s上传 %s 内容为空", scene, fileName)
	}
	base := "/v2/" + segment + "/" + url.PathEscape(target)

	prep, err := c.uploadPrepare(ctx, scene, base+"/upload_prepare", fileName, data)
	if err != nil {
		return MediaRef{}, err
	}
	if len(prep.Parts) == 0 {
		return MediaRef{}, fmt.Errorf("qq: %s预上传没有返回分片列表", scene)
	}
	blockSize := atoiDefault(prep.BlockSize, defaultBlockSize)
	for i, part := range prep.Parts {
		start := i * blockSize
		if start >= len(data) {
			return MediaRef{}, fmt.Errorf("qq: %s预上传声明的分片数超出文件长度", scene)
		}
		end := start + blockSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[start:end]
		if err := putChunk(ctx, part.PresignedURL, chunk); err != nil {
			return MediaRef{}, fmt.Errorf("qq: %s分片 %d 上传失败: %w", scene, part.Index, err)
		}
		if err := c.uploadPartFinish(ctx, scene, base+"/upload_part_finish", prep.UploadID, part, chunk); err != nil {
			return MediaRef{}, err
		}
	}

	var res filesResp
	body := map[string]any{
		"file_type":    fileTypeImage,
		"file_name":    fileName,
		"upload_id":    prep.UploadID,
		"srv_send_msg": false,
	}
	if err := c.do(ctx, scene+"富媒体合并", http.MethodPost, base+"/files", body, &res); err != nil {
		return MediaRef{}, err
	}
	// 与 Me 同理：HTTP 200 但字段对不上时，别让"结构变更"被吞成"上传成功"。
	if res.FileInfo == "" {
		return MediaRef{}, fmt.Errorf("qq: %s上传接口未返回 file_info", scene)
	}
	return MediaRef{FileUUID: res.FileUUID, FileInfo: res.FileInfo, TTL: time.Duration(res.TTL) * time.Second}, nil
}

func (c *Client) uploadPrepare(ctx context.Context, scene, path, fileName string, data []byte) (prepareResp, error) {
	var res prepareResp
	body := map[string]any{
		"file_type": fileTypeImage,
		// 文档把这两个长度字段定义成字符串，按字符串发。
		"file_size": strconv.Itoa(len(data)),
		"file_name": fileName,
		"md5":       hexOfMD5(data),
		"sha1":      hexOfSHA1(data),
		"md5_10m":   hexOfMD5(prefix(data, md5QuickPrefix)),
	}
	if err := c.do(ctx, scene+"富媒体预上传", http.MethodPost, path, body, &res); err != nil {
		return prepareResp{}, err
	}
	if res.UploadID == "" {
		return prepareResp{}, fmt.Errorf("qq: %s预上传未返回 upload_id", scene)
	}
	return res, nil
}

func (c *Client) uploadPartFinish(ctx context.Context, scene, path, uploadID string, part preparePart, chunk []byte) error {
	body := map[string]any{
		"upload_id":  uploadID,
		"part_index": part.Index,
		"block_size": strconv.Itoa(len(chunk)),
		"md5":        hexOfMD5(chunk),
	}
	// 该接口成功时响应体为空，out 传 nil 才不会把空 body 当成解析失败。
	return c.do(ctx, scene+"分片完成", http.MethodPost, path, body, nil)
}

// putChunk 走 COS 预签名 URL：它不需要 access_token，也不该吃 client.do 那套
// token 失效重试，所以单独用一次裸请求。
func putChunk(ctx context.Context, presignedURL string, chunk []byte) error {
	if presignedURL == "" {
		return fmt.Errorf("qq: 预签名上传 URL 为空")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, presignedURL, bytes.NewReader(chunk))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(chunk))
	resp, err := chunkClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := readLimited(resp.Body, maxResponseBytes)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("qq: 预签名 PUT http=%d body=%.200q", resp.StatusCode, data)
	}
	return nil
}

var chunkClient = &http.Client{Timeout: 60 * time.Second}

func prefix(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func hexOfMD5(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func hexOfSHA1(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

func atoiDefault(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return def
}
