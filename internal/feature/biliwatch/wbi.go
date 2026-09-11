package biliwatch

import (
	"crypto/md5"
	"encoding/hex"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var mixinKeyEncTab = [...]int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35, 27, 43, 5, 49,
	33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13, 37, 48, 7, 16, 24, 55, 40,
	61, 26, 17, 0, 1, 60, 51, 30, 4, 22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11,
	36, 20, 34, 44, 52,
}

func getMixinKey(orig string) string {
	var sb strings.Builder
	for _, i := range mixinKeyEncTab {
		if i < len(orig) {
			sb.WriteByte(orig[i])
		}
	}
	res := sb.String()
	if len(res) > 32 {
		return res[:32]
	}
	return res
}

// sanitizeWbiValue 过滤 WBI 不允许的保留字符
func sanitizeWbiValue(val string) string {
	var sb strings.Builder
	for _, r := range val {
		switch r {
		case '!', '\'', '(', ')', '*':
			// drop or ignore
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// SignWbi 使用 imgKey 与 subKey 为参数列表计算 wts 与 w_rid 签名。
func SignWbi(params map[string]string, imgKey, subKey string, nowUnix int64) url.Values {
	if nowUnix <= 0 {
		nowUnix = time.Now().Unix()
	}
	mixinKey := getMixinKey(imgKey + subKey)

	// 复制并补充 wts
	p := make(map[string]string, len(params)+2)
	for k, v := range params {
		p[k] = sanitizeWbiValue(v)
	}
	p["wts"] = strconv.FormatInt(nowUnix, 10)

	// 字典序排序
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var queryParts []string
	values := url.Values{}
	for _, k := range keys {
		v := p[k]
		values.Set(k, v)
		queryParts = append(queryParts, url.QueryEscape(k)+"="+url.QueryEscape(v))
	}

	rawQuery := strings.Join(queryParts, "&")
	hash := md5.Sum([]byte(rawQuery + mixinKey))
	wRid := hex.EncodeToString(hash[:])

	values.Set("w_rid", wRid)
	return values
}

func getKeyFromURL(rawURL string) string {
	parts := strings.Split(rawURL, "/")
	if len(parts) == 0 {
		return ""
	}
	file := parts[len(parts)-1]
	dotIdx := strings.Index(file, ".")
	if dotIdx >= 0 {
		return file[:dotIdx]
	}
	return file
}
