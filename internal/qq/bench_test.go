package qq_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	. "xingta/internal/qq"

	"xingta/internal/devtools/qqsim"
)

// 这几个基准是用来做成本归因的：端到端吞吐里「解码 vs 去重 vs 派发」各占多少。
// 没有这组数，只能看到一个总吞吐，不知道往哪儿优化。

var benchPayload = func() json.RawMessage {
	_, raw, _ := qqsim.DefaultGenerator().NextGroupAt()
	return raw
}()

func BenchmarkDecodeMessage(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchPayload)))
	for i := 0; i < b.N; i++ {
		if _, err := DecodeMessage(EventGroupAtMessage, benchPayload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDedupSeeAdmit(b *testing.B) {
	d := NewDeduper(DedupConfig{MaxKeys: b.N + 1})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !d.See(EventGroupAtMessage, fmt.Sprintf("m%d", i)) {
			b.Fatal("不该判成重复")
		}
	}
}

// 热点 id 反复命中：这是补发风暴时的路径。
func BenchmarkDedupSeeHit(b *testing.B) {
	d := NewDeduper(DedupConfig{})
	d.See(EventGroupAtMessage, "hot")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if d.See(EventGroupAtMessage, "hot") {
			b.Fatal("窗口内不该放行")
		}
	}
}

// idSlot 是模板报文里那段定宽数字 id 的字节区间。
type idSlot struct {
	buf   []byte
	start int
	end   int
}

// newIDTemplate 复制一份真形状，把 id 换成 "0000...0" 定宽数字槽，
// 这样每轮原地写数字就行 —— 用 Sprintf 重造整条报文会把分配开销混进被测成本。
func newIDTemplate(t *testing.B) *idSlot {
	t.Helper()
	const marker = `ROBOT1.0_`
	raw := append([]byte(nil), benchPayload...)
	i := bytes.Index(raw, []byte(marker))
	if i < 0 {
		t.Fatal("fixture 里没有 ROBOT1.0_ 前缀，改法失效")
	}
	start := i + len(marker)
	end := start
	for end < len(raw) && raw[end] != '"' {
		end++
	}
	width := end - start
	if width < 8 {
		t.Fatalf("id 段太短（%d），换不出唯一值", width)
	}
	for k := start; k < end; k++ {
		raw[k] = '0'
	}
	return &idSlot{buf: raw, start: start, end: end}
}

// write 把 n 右对齐写进槽里（前面补 0）。
func (s *idSlot) write(n int) {
	for k := s.end - 1; k >= s.start; k-- {
		s.buf[k] = byte('0' + n%10)
		n /= 10
	}
}

// 完整入口：信封已在手，走解码 + 判重 + 派发。
func BenchmarkHubHandle(b *testing.B) {
	h := NewHub(nil, func(string, ...any) {})
	h.OnMessage(EventGroupAtMessage, func(context.Context, *Message) error { return nil })
	slot := newIDTemplate(b)
	// 每轮 id 都不同，走的是「插入 + 淘汰最老」的稳态路径 —— 和生产一致，
	// 淘汰后的键同样按新键计入，所以不需要把去重表开到 b.N 那么大。
	ev := Event{Type: EventGroupAtMessage, Data: slot.buf}
	ctx := context.Background()

	b.ReportAllocs()
	b.SetBytes(int64(len(slot.buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev.Seq = int64(i)
		slot.write(i)
		if err := h.Handle(ctx, ev); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHubHandleReplayed(b *testing.B) {
	h := NewHub(nil, func(string, ...any) {})
	h.OnMessage(EventGroupAtMessage, func(context.Context, *Message) error { return nil })
	ctx := context.Background()
	_ = h.Handle(ctx, Event{Type: EventGroupAtMessage, Data: benchPayload})

	b.ReportAllocs()
	b.SetBytes(int64(len(benchPayload)))
	for i := 0; i < b.N; i++ {
		if err := h.Handle(ctx, Event{Type: EventGroupAtMessage, Data: benchPayload}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExtValue(b *testing.B) {
	s := MessageScene{Ext: []string{"msg_idx=" + repeat('a', 84), "auth_token=" + repeat('b', 124)}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if s.ExtValue("auth_token") == "" {
			b.Fatal("取空了")
		}
	}
}

func repeat(c byte, n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = c
	}
	return string(buf)
}
