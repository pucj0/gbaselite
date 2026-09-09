package server

import (
	"fmt"
	"gbaselite/storageengine"
	"testing"
)

func TestMVCCWriteBudgetErrorCode(t *testing.T) {
	if code := mysqlExecutionErrorCode(fmt.Errorf("limit: %w", storageengine.ErrWriteSetLimit)); code != 1041 {
		t.Fatal(code)
	}
}
