//go:build !windows

package play

import "errors"

// EnableVT 在 Unix 终端上 ANSI 是原生的，无需开关。
func EnableVT() bool { return true }

// NewKeys 在非 Windows 平台暂不提供原始键盘模式。
//
// 不假装支持：真正的原始模式要处理 termios，而本项目的目标是 Windows 上的一句可跑。
// 调用方收到错误后会退化成逐行输入，协议路径不受影响。
func NewKeys() (KeySource, error) {
	return nil, errors.New("当前平台暂不支持原始键盘模式，请用 -plain（逐行输入）")
}
