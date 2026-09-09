package server

import (
	"gbaselite/executor"
	"testing"
)

func openTestEngine(t testing.TB, dir, user, password string) (*executor.Engine, error) {
	return openTestEngineWithOptions(t, dir, user, password, executor.OpenOptions{})
}
func openTestEngineWithOptions(t testing.TB, dir, user, password string, o executor.OpenOptions) (*executor.Engine, error) {
	e, err := executor.OpenWithOptions(dir, user, password, o)
	if err == nil {
		t.Cleanup(func() { _ = e.Close() })
	}
	return e, err
}
