package logging

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestParseLevelAcceptsCanonicalNamesAndAliases(t *testing.T) {
	cases := map[string]Level{
		"DEBUG":   LevelDebug,
		"debug":   LevelDebug,
		" trace ": LevelDebug,
		"INFO":    LevelInfo,
		"":        LevelInfo,
		"Info":    LevelInfo,
		"WARN":    LevelWarn,
		"warning": LevelWarn,
		"ERROR":   LevelError,
		"fatal":   LevelError,
	}
	for input, want := range cases {
		got, err := ParseLevel(input)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", input, got, err, want)
		}
	}
}

func TestParseLevelRejectsUnknownNames(t *testing.T) {
	for _, bad := range []string{"verbose", "1", "调试", "warningish"} {
		if _, err := ParseLevel(bad); err == nil {
			t.Errorf("ParseLevel(%q) should fail so a typo is caught instead of silently ignored", bad)
		}
	}
}

// capture 临时接管标准日志输出并设定级别，返回捕获到的内容。
func capture(t *testing.T, level Level, fn func()) string {
	t.Helper()
	var buffer bytes.Buffer
	previousWriter := log.Writer()
	previousLevel := Level(current.Load())
	log.SetOutput(&buffer)
	SetLevel(level)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		SetLevel(previousLevel)
	})
	fn()
	return buffer.String()
}

func TestOnlyEnabledLevelsAreWritten(t *testing.T) {
	out := capture(t, LevelWarn, func() {
		Debugf("debug line")
		Infof("info line")
		Warnf("warn line")
		Errorf("error line")
	})
	for _, want := range []string{"WARN warn line", "ERROR error line"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	for _, banned := range []string{"DEBUG debug line", "INFO info line"} {
		if strings.Contains(out, banned) {
			t.Errorf("%q should have been filtered out at WARN, got %q", banned, out)
		}
	}
}

func TestDebugIsOffAtDefaultInfoLevel(t *testing.T) {
	out := capture(t, LevelInfo, func() { Debugf("must not appear") })
	if out != "" {
		t.Fatalf("debug output leaked at INFO: %q", out)
	}
}

func TestConfigureAppliesLevelAndFallsBackToInfo(t *testing.T) {
	previous := Level(current.Load())
	t.Cleanup(func() { SetLevel(previous) })

	if got := Configure("debug"); got != LevelDebug || !Enabled(LevelDebug) {
		t.Fatalf("Configure(debug) = %v, Enabled(debug) = %v", got, Enabled(LevelDebug))
	}
	// 无法识别的级别退回 INFO 而不是报错：这个函数在启动与配置热更新两处调用，
	// 不该因为一个日志级别把服务卡住（配置层已经在 Parse 时就拒绝过打错的值）。
	if got := Configure("nonsense"); got != LevelInfo {
		t.Fatalf("Configure(nonsense) = %v, want INFO", got)
	}
	if !Enabled(LevelInfo) || Enabled(LevelDebug) {
		t.Fatalf("after falling back to INFO: Enabled(INFO)=%v Enabled(DEBUG)=%v", Enabled(LevelInfo), Enabled(LevelDebug))
	}
}

func TestEnabledOrdering(t *testing.T) {
	previous := Level(current.Load())
	t.Cleanup(func() { SetLevel(previous) })
	SetLevel(LevelError)
	if Enabled(LevelWarn) || Enabled(LevelInfo) || Enabled(LevelDebug) {
		t.Fatal("ERROR must suppress everything below it")
	}
	if !Enabled(LevelError) {
		t.Fatal("ERROR must always be enabled")
	}
}
