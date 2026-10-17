package app

import (
	"errors"
	"strings"

	"github.com/3ekbbbs/schedule-server/internal/config"
)

// version 是服务端版本号。发版时同步 README 的版本表。
const version = "1.2.1"

// Version 供入口与面板读取。
func Version() string { return version }

// Config 是 config.Config 的别名，让原有代码里的 Config 引用保持不变。
type Config = config.Config

// maskInvite 只留邀请码首尾各 2 位，用于日志。
//
// 日志文件和 exe 放在同一个目录，完整邀请码进日志等于谁翻到日志谁就能注册。
// 太短的（≤6）全打码：露头露尾等于全露。
func maskInvite(code string) string {
	if len(code) <= 6 {
		return strings.Repeat("•", len(code))
	}
	return code[:2] + strings.Repeat("•", len(code)-4) + code[len(code)-2:]
}

var errForbidden = errors.New("forbidden")
