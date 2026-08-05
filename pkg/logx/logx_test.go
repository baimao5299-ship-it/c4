package logx_test

import (
	"testing"

	"go-proxy-mini/pkg/logx"
)

func TestLevelFiltering(t *testing.T) {
	// warn 级别下 Debug 不输出、Warn 输出
	logger, err := logx.New("warn", "stdout")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	logger.Debug("hidden", logx.String("k", "v"))
	logger.Warn("visible", logx.Int("n", 1))
	_ = logger.Sync()
}

func TestWithFields(t *testing.T) {
	logger, _ := logx.New("error", "stdout")
	child := logger.With(logx.String("trace", "abc"))
	child.Error("boom", logx.Error(nil))
	_ = logger.Sync()
}
