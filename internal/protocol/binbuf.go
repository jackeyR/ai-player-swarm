package protocol

import (
	"encoding/binary"
	"errors"
	"math"
)

// ErrBadBody 表示二进制消息体长度不足或字段越界。
var ErrBadBody = errors.New("protocol: malformed binary body")

// BinWriter 是零依赖的大端二进制写入器，专为定长数据面消息设计。
// 预分配容量，避免热路径（10Hz × 玩家数）上的小对象分配（B11）。
type BinWriter struct {
	b []byte
}

// NewBinWriter 创建写入器，capacity 为预估容量。
func NewBinWriter(capacity int) *BinWriter {
	if capacity < 32 {
		capacity = 32
	}
	return &BinWriter{b: make([]byte, 0, capacity)}
}

// U8 追加 1 字节。
func (w *BinWriter) U8(v uint8) *BinWriter { w.b = append(w.b, v); return w }

// U16 追加 2 字节大端。
func (w *BinWriter) U16(v uint16) *BinWriter {
	var t [2]byte
	binary.BigEndian.PutUint16(t[:], v)
	w.b = append(w.b, t[:]...)
	return w
}

// U32 追加 4 字节大端。
func (w *BinWriter) U32(v uint32) *BinWriter {
	var t [4]byte
	binary.BigEndian.PutUint32(t[:], v)
	w.b = append(w.b, t[:]...)
	return w
}

// U64 追加 8 字节大端。
func (w *BinWriter) U64(v uint64) *BinWriter {
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], v)
	w.b = append(w.b, t[:]...)
	return w
}

// I16 追加 2 字节大端有符号整数。
func (w *BinWriter) I16(v int16) *BinWriter { return w.U16(uint16(v)) }

// Str8 追加 u8 长度前缀的 UTF-8 字符串，超长截断到 255 字节。
func (w *BinWriter) Str8(s string) *BinWriter {
	if len(s) > math.MaxUint8 {
		s = s[:math.MaxUint8]
	}
	w.b = append(w.b, uint8(len(s)))
	w.b = append(w.b, s...)
	return w
}

// Bytes 返回底层切片。调用方不得继续写入后再使用旧返回值。
func (w *BinWriter) Bytes() []byte { return w.b }

// Len 返回已写入字节数。
func (w *BinWriter) Len() int { return len(w.b) }

// BinReader 是 BinWriter 的对称读取器。字段越界时置内部错误并返回零值，
// 调用方只需在末尾检查一次 Err()，避免逐字段判错。
type BinReader struct {
	b   []byte
	off int
	err error
}

// NewBinReader 包装一个消息体。
func NewBinReader(b []byte) *BinReader { return &BinReader{b: b} }

func (r *BinReader) need(n int) bool {
	if r.err != nil {
		return false
	}
	if r.off+n > len(r.b) {
		r.err = ErrBadBody
		return false
	}
	return true
}

// U8 读 1 字节。
func (r *BinReader) U8() uint8 {
	if !r.need(1) {
		return 0
	}
	v := r.b[r.off]
	r.off++
	return v
}

// U16 读 2 字节大端。
func (r *BinReader) U16() uint16 {
	if !r.need(2) {
		return 0
	}
	v := binary.BigEndian.Uint16(r.b[r.off:])
	r.off += 2
	return v
}

// U32 读 4 字节大端。
func (r *BinReader) U32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.BigEndian.Uint32(r.b[r.off:])
	r.off += 4
	return v
}

// U64 读 8 字节大端。
func (r *BinReader) U64() uint64 {
	if !r.need(8) {
		return 0
	}
	v := binary.BigEndian.Uint64(r.b[r.off:])
	r.off += 8
	return v
}

// I16 读 2 字节大端有符号整数。
func (r *BinReader) I16() int16 { return int16(r.U16()) }

// Str8 读 u8 长度前缀字符串。
func (r *BinReader) Str8() string {
	if !r.need(1) {
		return ""
	}
	n := int(r.b[r.off])
	r.off++
	if !r.need(n) {
		return ""
	}
	s := string(r.b[r.off : r.off+n])
	r.off += n
	return s
}

// Remaining 返回剩余未读字节数。
func (r *BinReader) Remaining() int {
	if r.err != nil {
		return 0
	}
	return len(r.b) - r.off
}

// Err 返回第一个发生的解析错误。
func (r *BinReader) Err() error { return r.err }
