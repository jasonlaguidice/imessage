// mautrix-imessage - A Matrix-iMessage puppeting bridge.

package connector

import (
	"testing"

	"github.com/rs/zerolog"
)

func TestRustLogLevel(t *testing.T) {
	tests := []struct {
		level string
		want  zerolog.Level
	}{
		{"ERROR", zerolog.ErrorLevel},
		{"WARN", zerolog.WarnLevel},
		{"INFO", zerolog.InfoLevel},
		{"DEBUG", zerolog.DebugLevel},
		{"TRACE", zerolog.TraceLevel},
		{"bogus", zerolog.InfoLevel},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			if got := rustLogLevel(tt.level); got != tt.want {
				t.Errorf("rustLogLevel(%q) = %d, want %d", tt.level, got, tt.want)
			}
		})
	}
}
