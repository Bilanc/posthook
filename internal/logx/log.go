package logx

import (
	"fmt"
	"os"
)

func debugEnabled() bool { return os.Getenv("POSTHOOK_DEBUG") == "1" }

func Info(msg string)                    { fmt.Fprintln(os.Stderr, msg) }
func Infof(format string, args ...any)   { fmt.Fprintln(os.Stderr, fmt.Sprintf(format, args...)) }
func Warn(msg string)                    { fmt.Fprintln(os.Stderr, "[posthook] warning: "+msg) }
func Warnf(format string, args ...any)   { fmt.Fprintln(os.Stderr, "[posthook] warning: "+fmt.Sprintf(format, args...)) }

func Debug(msg string) {
	if debugEnabled() {
		fmt.Fprintln(os.Stderr, "[posthook] "+msg)
	}
}

func Debugf(format string, args ...any) {
	if debugEnabled() {
		fmt.Fprintln(os.Stderr, "[posthook] "+fmt.Sprintf(format, args...))
	}
}
