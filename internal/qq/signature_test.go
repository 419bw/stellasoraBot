package qq_test

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"

	. "xingta/internal/qq"
)

// 官方文档「安全和授权」页 DEMO 原样抄录：
// https://bot.q.qq.com/wiki/develop/api-v2/dev-prepare/interface-framework/sign.html
const (
	goldenSecret = "naOC0ocQE3shWLAfffVLB1rhYPG7"
	goldenSeed   = "naOC0ocQE3shWLAfffVLB1rhYPG7naOC"
)

var goldenPublicKey = [32]byte{
	215, 195, 98, 254, 120, 174, 248, 31, 242, 50, 135, 180, 147, 98, 139, 93,
	176, 42, 60, 79, 227, 11, 33, 94, 77, 25, 96, 155, 93, 118, 103, 58,
}

var goldenPrivateKey = [64]byte{
	110, 97, 79, 67, 48, 111, 99, 81, 69, 51, 115, 104, 87, 76, 65, 102, 102, 102, 86, 76, 66, 49, 114, 104, 89, 80, 71, 55, 110, 97, 79, 67,
	215, 195, 98, 254, 120, 174, 248, 31, 242, 50, 135, 180, 147, 98, 139, 93,
	176, 42, 60, 79, 227, 11, 33, 94, 77, 25, 96, 155, 93, 118, 103, 58,
}

// 派生规则不能靠调用包内私有函数来验 —— 那是拿实现验实现。
// 这里改成用官方 DEMO 公布的公钥来验我们签出来的东西：钥匙不是同一把就验不过。
func TestSignatureVerifiesUnderOfficialDemoPublicKey(t *testing.T) {
	const ts = "1636373772"
	body := []byte(`{"msg":"hello"}`)

	sig, err := Sign(goldenSecret, ts, body)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	raw, err := hex.DecodeString(sig)
	if err != nil {
		t.Fatalf("签名不是 hex: %v", err)
	}
	// 公钥与私钥都逐字抄自官方 DEMO，不来自我们的代码。
	if !ed25519.Verify(goldenPublicKey[:], []byte(ts+string(body)), raw) {
		t.Error("我们用 goldenSecret 派生的钥匙和官方 DEMO 的不是同一把 —— 与平台/botgo 签名不兼容")
	}
	if !ed25519.Verify(ed25519.PrivateKey(goldenPrivateKey[:]).Public().(ed25519.PublicKey), []byte(ts+string(body)), raw) {
		t.Error("官方 DEMO 私钥的公钥验不过我们的签名")
	}
}

// 官方 DEMO 用 ed25519.GenerateKey(strings.NewReader(seed)) 派生，我们走
// NewKeyFromSeed(seed)。两条路必须产出同一把钥匙，否则互认失败。
// 种子的「短就自身翻倍到 32 字节」规则在这里按文档独立写一遍当参照实现，
// 判定仍然通过公开的 Sign 做。
func TestSeedDerivationMatchesReference(t *testing.T) {
	refSeed := func(secret string) []byte {
		seed := secret
		for len(seed) < ed25519.SeedSize {
			seed += seed
		}
		return []byte(seed[:ed25519.SeedSize])
	}
	seed := refSeed(goldenSecret)
	if string(seed) != goldenSeed {
		t.Fatalf("参照实现自己都跑不出官方 DEMO 的 seed: %q", seed)
	}
	pubA, _, err := ed25519.GenerateKey(strings.NewReader(string(seed)))
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	const ts = "1636373772"
	body := []byte(`{"msg":"hello"}`)
	sig, err := Sign(goldenSecret, ts, body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString(sig)
	if !ed25519.Verify(pubA, []byte(ts+string(body)), raw) {
		t.Error("Sign 的签名验不过 GenerateKey(reader) 派生的公钥：两条派生路径不等价")
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	const ts = "1636373772"
	body := []byte(`{"msg":"hello"}`)

	sig, err := Sign(goldenSecret, ts, body)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(sig) != ed25519.SignatureSize*2 {
		t.Fatalf("signature hex length = %d, want %d", len(sig), ed25519.SignatureSize*2)
	}
	ok, err := Verify(goldenSecret, ts, sig, body)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("Verify = false, want true")
	}
}

// 变异负对照：任何一处不匹配都必须拒绝，而不是"大概率能过"。
func TestVerifyRejectsTamperedInput(t *testing.T) {
	const ts = "1636373772"
	body := []byte(`{"msg":"hello"}`)
	sig, err := Sign(goldenSecret, ts, body)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		secret    string
		timestamp string
		signature string
		body      []byte
	}{
		{"篡改 body", goldenSecret, ts, sig, []byte(`{"msg":"hellp"}`)},
		{"篡改 timestamp", goldenSecret, "1636373773", sig, body},
		{"换一把 secret", "anotherbotsecret0123456789abcdef", ts, sig, body},
		{"签名缺一半", goldenSecret, ts, sig[:32], body},
		{"签名非 hex", goldenSecret, ts, strings.Repeat("zz", 64), body},
		{"timestamp 为空", goldenSecret, "", sig, body},
	}
	for _, c := range cases {
		ok, err := Verify(c.secret, c.timestamp, c.signature, c.body)
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
			continue
		}
		if ok {
			t.Errorf("%s: Verify = true, want false", c.name)
		}
	}
}

// 官方算法显式要求丢弃 sig[63]&224 != 0 的签名。
func TestVerifyRejectsMalleableSignature(t *testing.T) {
	const ts = "1636373772"
	body := []byte(`{"msg":"hello"}`)
	sig, err := Sign(goldenSecret, ts, body)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(sig)
	if err != nil {
		t.Fatal(err)
	}
	raw[ed25519.SignatureSize-1] |= 224
	if ok, _ := Verify(goldenSecret, ts, hex.EncodeToString(raw), body); ok {
		t.Error("Verify accepted a signature with high bits set, want rejection")
	}
}

func TestEmptySecretRejected(t *testing.T) {
	if _, err := Sign("", "1636373772", nil); err == nil {
		t.Error("Sign with empty secret: err = nil, want error")
	}
	if _, err := Verify("", "1636373772", "00", nil); err == nil {
		t.Error("Verify with empty secret: err = nil, want error")
	}
}
