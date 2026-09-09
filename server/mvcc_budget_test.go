package server

import (
	"fmt"
	"gbaselite/mvcc"
	"testing"
)

func TestMVCCWriteBudgetErrorCode(t *testing.T) {
	if code := mysqlExecutionErrorCode(fmt.Errorf("limit: %w", mvcc.ErrWriteSetLimit)); code != 1041 {
		t.Fatal(code)
	}
}
