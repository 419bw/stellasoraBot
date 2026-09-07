package qq

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
)

const (
	HeaderSignature = "X-Signature-Ed25519"
	HeaderTimestamp = "X-Signature-Timestamp"
)

var ErrEmptySecret = errors.New("qq: bot secret is empty")

func seedFromSecret(secret string) ([]byte, error) {
	if secret == "" {
		return nil, ErrEmptySecret
	}
	seed := secret
	for len(seed) < ed25519.SeedSize {
		seed += seed
	}
	return []byte(seed[:ed25519.SeedSize]), nil
}

func keysFromSecret(secret string) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	seed, err := seedFromSecret(secret)
	if err != nil {
		return nil, nil, err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(ed25519.PublicKey), priv, nil
}

// Sign 按 timestamp+body 顺序拼接后签名，返回 hex 字符串。
// 回调校验握手复用同一函数，把 plain_token 当作 body 传入即可。
func Sign(secret, timestamp string, body []byte) (string, error) {
	if timestamp == "" {
		return "", errors.New("qq: timestamp is empty")
	}
	_, priv, err := keysFromSecret(secret)
	if err != nil {
		return "", err
	}
	var content bytes.Buffer
	content.WriteString(timestamp)
	content.Write(body)
	return hex.EncodeToString(ed25519.Sign(priv, content.Bytes())), nil
}

// Verify 校验回调请求签名。返回 false 表示应当拒绝该请求。
func Verify(secret, timestamp, signature string, body []byte) (bool, error) {
	if timestamp == "" {
		return false, nil
	}
	pub, _, err := keysFromSecret(secret)
	if err != nil {
		return false, err
	}
	sig, err := hex.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false, nil
	}
	// 官方签名算法要求丢弃最高 3 位非零的签名，否则同一消息存在可延展的等价签名
	if sig[ed25519.SignatureSize-1]&224 != 0 {
		return false, nil
	}
	var content bytes.Buffer
	content.WriteString(timestamp)
	content.Write(body)
	return ed25519.Verify(pub, content.Bytes(), sig), nil
}
