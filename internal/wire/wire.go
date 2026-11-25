// Package wire 定义应用层二进制帧格式。
//
// 两种承载形态共用同一套「消息号 + 序列号」语义（对应知识清单 A6 / A7）：
//
//	裸 TCP/UDP 形态 —— 长度前缀 + 消息号 + 序列号 + 消息体（解决粘包 / 半包）
//
//	  +--------+--------+--------+------------------+
//	  | len:u32| msgID:u16| seq:u32| body[len-6]      |
//	  +--------+--------+--------+------------------+
//	  len 表示其后 (msgID + seq + body) 的总字节数，不含自身 4 字节。
//
//	WebSocket 形态 —— 剥掉长度前缀，只保留信封（WS 自身已按消息分帧）
//
//	  +--------+--------+------------------+
//	  | msgID:u16| seq:u32| body             |
//	  +--------+--------+------------------+
//
// 为什么仍然保留序列号：它是幂等去重与「输入乱序 / 重复投递」检测的依据（E8），
// 也是 bot 侧把响应配对回请求的唯一手段（同一连接上并发多个请求）。
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// 常量定义。
const (
	// HeaderSize 是裸 TCP 帧的固定头长度：len(4) + msgID(2) + seq(4)。
	HeaderSize = 10
	// EnvelopeSize 是 WebSocket 形态信封长度：msgID(2) + seq(4)。
	EnvelopeSize = 6
	// DefaultMaxBody 是默认单帧消息体上限（64KiB），防对端发送超大帧打爆内存（A14 背压）。
	DefaultMaxBody = 64 << 10
	// MaxBodyLimit 是允许配置的硬上限。
	MaxBodyLimit = 16 << 20
)

// 错误定义。
var (
	// ErrFrameTooLarge 表示声明的帧长度超过上限。
	ErrFrameTooLarge = errors.New("wire: frame body too large")
	// ErrShortFrame 表示读取过程中对端关闭，帧被截断。
	ErrShortFrame = errors.New("wire: truncated frame")
	// ErrEmptyEnvelope 表示 WS 信封长度不足。
	ErrEmptyEnvelope = errors.New("wire: envelope shorter than header")
)

// Frame 是一条应用层消息。
type Frame struct {
	MsgID uint16
	Seq   uint32
	Body  []byte
}

// String 便于日志输出。
func (f Frame) String() string {
	return fmt.Sprintf("frame{msg=0x%04x seq=%d body=%dB}", f.MsgID, f.Seq, len(f.Body))
}

// Reader 从裸字节流读取帧。非并发安全，一个连接一个 Reader。
type Reader struct {
	r       io.Reader
	maxBody int
	hdr     [HeaderSize]byte
}

// NewReader 创建帧读取器，maxBody <= 0 时取 DefaultMaxBody。
func NewReader(r io.Reader, maxBody int) *Reader {
	if maxBody <= 0 {
		maxBody = DefaultMaxBody
	}
	if maxBody > MaxBodyLimit {
		maxBody = MaxBodyLimit
	}
	return &Reader{r: r, maxBody: maxBody}
}

// ReadFrame 读取一帧。返回的 Body 是新分配的切片，调用方可长期持有。
func (r *Reader) ReadFrame() (Frame, error) {
	if _, err := io.ReadFull(r.r, r.hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return Frame{}, ErrShortFrame
		}
		return Frame{}, err
	}
	total := binary.BigEndian.Uint32(r.hdr[0:4])
	if total < EnvelopeSize {
		return Frame{}, fmt.Errorf("wire: declared frame length %d < envelope %d", total, EnvelopeSize)
	}
	// 用 uint64 比较而不是 int(total)：在 32 位平台上 int(uint32) 会变成负数，
	// 于是「大于上限」的判断失效，紧接着 make([]byte, 负数) 直接 panic。
	// 对端只要发一个 0xFFFFFFFF 的长度前缀就能把 32 位进程打死。
	if uint64(total) > uint64(r.maxBody)+uint64(EnvelopeSize) {
		return Frame{}, ErrFrameTooLarge
	}
	body := make([]byte, int(total)-EnvelopeSize)
	// msgID 与 seq 紧跟长度前缀，已随固定头一并读入。
	f := Frame{
		MsgID: binary.BigEndian.Uint16(r.hdr[4:6]),
		Seq:   binary.BigEndian.Uint32(r.hdr[6:10]),
	}
	if len(body) > 0 {
		if _, err := io.ReadFull(r.r, body); err != nil {
			return Frame{}, ErrShortFrame
		}
	}
	f.Body = body
	return f, nil
}

// Writer 向裸字节流写帧。可被多个 goroutine 并发调用（内部加锁由调用方的 Conn 负责，
// 这里只保证单次写入的完整性）。
type Writer struct {
	w       io.Writer
	maxBody int
	buf     []byte
}

// NewWriter 创建帧写入器。
func NewWriter(w io.Writer, maxBody int) *Writer {
	if maxBody <= 0 {
		maxBody = DefaultMaxBody
	}
	if maxBody > MaxBodyLimit {
		maxBody = MaxBodyLimit
	}
	return &Writer{w: w, maxBody: maxBody, buf: make([]byte, HeaderSize, HeaderSize+512)}
}

// WriteFrame 写出一帧。
func (w *Writer) WriteFrame(f Frame) error {
	if len(f.Body) > w.maxBody {
		return ErrFrameTooLarge
	}
	need := HeaderSize + len(f.Body)
	if cap(w.buf) < need {
		w.buf = make([]byte, need)
	}
	b := w.buf[:need]
	binary.BigEndian.PutUint32(b[0:4], uint32(EnvelopeSize+len(f.Body)))
	binary.BigEndian.PutUint16(b[4:6], f.MsgID)
	binary.BigEndian.PutUint32(b[6:10], f.Seq)
	copy(b[HeaderSize:], f.Body)
	return writeFull(w.w, b)
}

// writeFull 处理短写：net.Conn.Write 通常一次写完，但包装层（bufio、TLS）可能短写。
func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// EncodeEnvelope 生成 WebSocket 形态的信封字节（msgID + seq + body）。
// 返回的切片长度恰为 EnvelopeSize + len(body)。
func EncodeEnvelope(msgID uint16, seq uint32, body []byte) []byte {
	b := make([]byte, EnvelopeSize+len(body))
	binary.BigEndian.PutUint16(b[0:2], msgID)
	binary.BigEndian.PutUint32(b[2:6], seq)
	copy(b[EnvelopeSize:], body)
	return b
}

// DecodeEnvelope 解析 WebSocket 形态信封。返回的 body 是入参切片的子切片（零拷贝）。
func DecodeEnvelope(b []byte) (msgID uint16, seq uint32, body []byte, err error) {
	if len(b) < EnvelopeSize {
		return 0, 0, nil, ErrEmptyEnvelope
	}
	return binary.BigEndian.Uint16(b[0:2]), binary.BigEndian.Uint32(b[2:6]), b[EnvelopeSize:], nil
}
