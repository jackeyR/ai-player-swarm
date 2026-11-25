//go:build windows

package play

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

// Windows 控制台原始模式。
//
// 为什么需要它：默认控制台是「行缓冲 + 回显」，按一个键不会立刻返回，
// 要按回车才送到程序。那样根本没法用 WASD 玩。
// 关掉 ENABLE_LINE_INPUT / ENABLE_ECHO_INPUT 之后，每次按键立刻可读。

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

const (
	// 输入模式位
	enableProcessedInput = 0x0001
	enableLineInput      = 0x0002
	enableEchoInput      = 0x0004
	// 输出模式位（与 enableEchoInput 数值相同但属于不同方向，故分开命名）
	enableVTProcessing = 0x0004
)

func consoleMode(h syscall.Handle) (uint32, error) {
	var mode uint32
	r, _, err := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		return 0, err
	}
	return mode, nil
}

func setConsoleMode(h syscall.Handle, mode uint32) error {
	r, _, err := procSetConsoleMode.Call(uintptr(h), uintptr(mode))
	if r == 0 {
		return err
	}
	return nil
}

// EnableVT 打开控制台的 ANSI 转义支持，返回是否可用。
//
// Windows 10 以前的控制台不认 ANSI 序列；关掉失败时调用方应当退回逐行模式，
// 否则屏幕上会出现一堆 `[1;32m` 之类的乱码。
func EnableVT() bool {
	h := syscall.Handle(os.Stdout.Fd())
	mode, err := consoleMode(h)
	if err != nil {
		return false
	}
	if mode&enableVTProcessing != 0 {
		return true
	}
	return setConsoleMode(h, mode|enableVTProcessing) == nil
}

type winKeys struct {
	h    syscall.Handle
	orig uint32
	ch   chan KeyEvent
	done chan struct{}
	once sync.Once
}

// NewKeys 把 stdin 切到原始模式并开始读键。
func NewKeys() (KeySource, error) {
	h := syscall.Handle(os.Stdin.Fd())
	orig, err := consoleMode(h)
	if err != nil {
		return nil, err
	}
	raw := orig
	raw &^= enableLineInput | enableEchoInput | enableProcessedInput
	if err := setConsoleMode(h, raw); err != nil {
		return nil, err
	}
	k := &winKeys{h: h, orig: orig, ch: make(chan KeyEvent, 64), done: make(chan struct{})}
	go k.readLoop()
	return k, nil
}

// Events 返回按键通道。
func (k *winKeys) Events() <-chan KeyEvent { return k.ch }

// Close 恢复控制台原模式。**必须恢复**，否则退出后终端不再回显，
// 用户会以为自己的终端坏了。
func (k *winKeys) Close() error {
	var err error
	k.once.Do(func() {
		close(k.done)
		err = setConsoleMode(k.h, k.orig)
	})
	return err
}

func (k *winKeys) readLoop() {
	defer close(k.ch)
	buf := make([]byte, 64)
	for {
		select {
		case <-k.done:
			return
		default:
		}
		n, err := os.Stdin.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return
			}
			// 非致命：可能是暂时性错误，继续读
			continue
		}
		if n == 0 {
			continue
		}
		k.decode(buf[:n])
	}
}

// decode 把一段原始字节翻译成按键事件。
//
// 方向键/功能键会以 ESC 开头，这里整段跳过：本客户端用 WASD 而不是方向键，
// 与其半吊子地解析 CSI 序列，不如明确忽略（否则 ESC 那一位会被当成 'q' 附近的东西）。
func (k *winKeys) decode(b []byte) {
	for i := 0; i < len(b); {
		if b[i] == 0x1b {
			// 跳过 CSI 序列：直到遇到字母或 '~'
			j := i + 1
			for j < len(b) {
				c := b[j]
				if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '~' {
					j++
					break
				}
				j++
			}
			i = j
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		i += size
		if ev, ok := normalizeKey(r); ok {
			select {
			case k.ch <- ev:
			default:
				// 按键积压：丢弃最旧的无所谓，但绝不能阻塞读取
			}
		}
	}
}
