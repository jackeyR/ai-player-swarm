package play

// 按键事件与按键源。具体实现按平台分文件（keys_windows.go / keys_other.go）。

// KeyEvent 是一次按键。Key 已归一化为小写；空格为 ' '。
type KeyEvent struct {
	Key rune
}

// KeySource 是可插拔的按键来源。
//
// 抽象成接口是为了让「没有键可用」这件事不是致命错误：
// 输出被重定向、或者在非 Windows 平台上，客户端会退化成逐行输入模式，
// 但协议路径与 TUI 模式完全一致。
type KeySource interface {
	Events() <-chan KeyEvent
	Close() error
}

// normalizeKey 把控制台读到的字符变成协议键位。返回 false 表示忽略。
func normalizeKey(r rune) (KeyEvent, bool) {
	if r >= 'A' && r <= 'Z' {
		r = r - 'A' + 'a'
	}
	switch r {
	case '\r', '\n', 0:
		return KeyEvent{}, false
	case ' ':
		return KeyEvent{Key: ' '}, true
	}
	if r < 0x20 || r == 0x7f {
		return KeyEvent{}, false
	}
	return KeyEvent{Key: r}, true
}
