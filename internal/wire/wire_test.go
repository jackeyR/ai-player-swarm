package wire

import (
	"bytes"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, 0)
	in := []Frame{
		{MsgID: 0x0101, Seq: 1, Body: []byte(`{"a":1}`)},
		{MsgID: 0x0203, Seq: 2, Body: nil},
		{MsgID: 0xFFFF, Seq: 0xFFFFFFFF, Body: bytes.Repeat([]byte{7}, 1000)},
	}
	for _, f := range in {
		if err := w.WriteFrame(f); err != nil {
			t.Fatalf("写帧 %s: %v", f, err)
		}
	}

	r := NewReader(bytes.NewReader(buf.Bytes()), 0)
	for i, want := range in {
		got, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("第 %d 帧读取失败: %v", i, err)
		}
		if got.MsgID != want.MsgID || got.Seq != want.Seq || !bytes.Equal(got.Body, want.Body) {
			t.Fatalf("第 %d 帧不一致: got=%+v want=%+v", i, got, want)
		}
	}
	if _, err := r.ReadFrame(); err != io.EOF && err != ErrShortFrame {
		t.Fatalf("读完后期望 EOF/短帧，实际 %v", err)
	}
}

// TestTruncatedFrame 验证半包：连接在帧中间断掉必须报错，而不是静默返回半条消息。
func TestTruncatedFrame(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, 0)
	_ = w.WriteFrame(Frame{MsgID: 1, Seq: 1, Body: bytes.Repeat([]byte{1}, 100)})
	full := buf.Bytes()

	for _, cut := range []int{2, 6, 9, 20, len(full) - 1} {
		r := NewReader(bytes.NewReader(full[:cut]), 0)
		if _, err := r.ReadFrame(); err == nil {
			t.Fatalf("截断到 %d 字节时应当报错", cut)
		}
	}
}

// TestFrameTooLarge 验证长度上限：对端声明超大帧时必须拒绝，不能先分配内存再炸。
func TestFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, 16)
	if err := w.WriteFrame(Frame{MsgID: 1, Seq: 1, Body: bytes.Repeat([]byte{1}, 17)}); err != ErrFrameTooLarge {
		t.Fatalf("写入超限帧期望 ErrFrameTooLarge，实际 %v", err)
	}

	// 手工构造一条声明长度为 99999 的帧头。
	var head [10]byte
	head[0], head[1], head[2], head[3] = 0, 1, 0x86, 0x9f // 99999
	r := NewReader(bytes.NewReader(head[:]), 16)
	if _, err := r.ReadFrame(); err != ErrFrameTooLarge {
		t.Fatalf("读取超限帧期望 ErrFrameTooLarge，实际 %v", err)
	}
}

// TestFrameLengthMaxUint32 是 32 位平台上的分配炸弹回归测试。
//
// 旧实现用 `int(total) > maxBody+6` 判断上限：在 32 位平台上
// int(0xFFFFFFFF) 会变成 -1，判断为「没有超限」，紧接着
// make([]byte, -1-6) 直接 panic。对端只发 4 个字节就能打死进程。
func TestFrameLengthMaxUint32(t *testing.T) {
	var head [10]byte
	head[0], head[1], head[2], head[3] = 0xFF, 0xFF, 0xFF, 0xFF
	r := NewReader(bytes.NewReader(head[:]), 1024)
	f, err := r.ReadFrame()
	if err != ErrFrameTooLarge {
		t.Fatalf("0xFFFFFFFF 长度前缀必须被判为超限，实际 err=%v frame=%+v", err, f)
	}

	// 边界：恰好等于上限应当能读（长度对但没数据 → 短帧错误，而不是超限）。
	var exact [10]byte
	n := uint32(1024 + EnvelopeSize)
	exact[0], exact[1], exact[2], exact[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	r2 := NewReader(bytes.NewReader(exact[:]), 1024)
	if _, err := r2.ReadFrame(); err != ErrShortFrame {
		t.Fatalf("恰好等于上限时应当报短帧，实际 %v", err)
	}
}

func TestDeclaredLengthTooSmall(t *testing.T) {
	var head [10]byte
	head[3] = 3 // 宣称总长 3 字节，比信封 6 字节还小
	r := NewReader(bytes.NewReader(head[:]), 0)
	if _, err := r.ReadFrame(); err == nil {
		t.Fatal("非法长度应当报错")
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	b := EncodeEnvelope(0x0109, 42, []byte("hello"))
	if len(b) != EnvelopeSize+5 {
		t.Fatalf("信封长度应为 %d，实际 %d", EnvelopeSize+5, len(b))
	}
	id, seq, body, err := DecodeEnvelope(b)
	if err != nil {
		t.Fatalf("解信封: %v", err)
	}
	if id != 0x0109 || seq != 42 || string(body) != "hello" {
		t.Fatalf("信封内容不一致: id=%#x seq=%d body=%q", id, seq, body)
	}
	if _, _, _, err := DecodeEnvelope([]byte{1, 2}); err == nil {
		t.Fatal("过短的信封应当报错")
	}
}

// chunkedWriter 模拟只写一部分的 Writer（bufio/TLS 包装层可能出现），验证 writeFull。
type chunkedWriter struct {
	buf  bytes.Buffer
	step int
}

func (c *chunkedWriter) Write(p []byte) (int, error) {
	n := c.step
	if n > len(p) {
		n = len(p)
	}
	c.buf.Write(p[:n])
	return n, nil
}

func TestShortWrites(t *testing.T) {
	cw := &chunkedWriter{step: 3}
	w := NewWriter(cw, 0)
	want := Frame{MsgID: 7, Seq: 9, Body: bytes.Repeat([]byte{0xAB}, 50)}
	if err := w.WriteFrame(want); err != nil {
		t.Fatalf("短写场景下写帧失败: %v", err)
	}
	r := NewReader(bytes.NewReader(cw.buf.Bytes()), 0)
	got, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("短写场景下读帧失败: %v", err)
	}
	if got.MsgID != want.MsgID || got.Seq != want.Seq || !bytes.Equal(got.Body, want.Body) {
		t.Fatalf("短写后帧内容不一致")
	}
}
