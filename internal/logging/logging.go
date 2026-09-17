// Package logging 提供最小可用的分级日志。
//
// 存在的理由：LOG_LEVEL 这个配置项一直是「能改、能存盘、但没有任何代码读它」，
// 运维把它调成 DEBUG 也拿不到更多信息。与其删掉这个开关，不如让它真的管用——
// 排障时「客户端说报错，但容器日志里什么都没有」是最难查的一类问题。
//
// 刻意不引第三方日志库：本项目只需要四个级别和一个全局阈值，标准库的 log
// 已经负责了时间戳与实际输出。
package logging

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
)

// Level 数值越大越啰嗦，这样「当前级别 >= 输出级别」就是「应当输出」。
type Level int32

const (
	LevelError Level = iota
	LevelWarn
	LevelInfo
	LevelDebug
)

var current atomic.Int32

func init() { current.Store(int32(LevelInfo)) }

// Tag 返回日志行里的级别标记。
func (l Level) Tag() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	}
	return "INFO"
}

// ParseLevel 解析配置里的级别名。大小写不敏感，并接受几个常见别名；
// 无法识别时返回错误——宁可让配置保存失败，也不要静默忽略一个打错的级别。
func ParseLevel(name string) (Level, error) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "DEBUG", "TRACE":
		return LevelDebug, nil
	case "INFO", "":
		return LevelInfo, nil
	case "WARN", "WARNING":
		return LevelWarn, nil
	case "ERROR", "FATAL":
		return LevelError, nil
	}
	return LevelInfo, fmt.Errorf("unknown log level %q (want one of DEBUG, INFO, WARN, ERROR)", strings.TrimSpace(name))
}

// Configure 应用级别。传入无法识别的值时退回 INFO 而不是报错：这个函数在
// 启动与配置热更新两处被调用，都不该因为一个日志级别把服务卡住。
func Configure(name string) Level {
	level, err := ParseLevel(name)
	if err != nil {
		level = LevelInfo
	}
	current.Store(int32(level))
	return level
}

// SetLevel 直接设置级别（测试与已校验过的调用方使用）。
func SetLevel(level Level) { current.Store(int32(level)) }

// Enabled 报告某个级别当前是否会输出。用于跳过开销较大的取值动作
// （例如组装日志参数本身就要查库或做格式化时）。
func Enabled(level Level) bool { return level <= Level(current.Load()) }

func logAt(level Level, format string, args ...any) {
	if !Enabled(level) {
		return
	}
	log.Printf("%s %s", level.Tag(), fmt.Sprintf(format, args...))
}

func Debugf(format string, args ...any) { logAt(LevelDebug, format, args...) }
func Infof(format string, args ...any)  { logAt(LevelInfo, format, args...) }
func Warnf(format string, args ...any)  { logAt(LevelWarn, format, args...) }
func Errorf(format string, args ...any) { logAt(LevelError, format, args...) }
