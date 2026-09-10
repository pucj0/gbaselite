package storageengine_test

import (
	"gbaselite/storageengine"
	"gbaselite/storageengine/mvccadapter"
	"gbaselite/storageengine/testkit"
	"testing"
)

func TestBackendContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		testkit.Run(t, func(*testing.T) storageengine.Engine { return testkit.NewMemory() })
	})
	for _, wal := range []bool{false, true} {
		name := "mvcc"
		if wal {
			name = "mvcc-wal"
		}
		t.Run(name, func(t *testing.T) {
			testkit.Run(t, func(t *testing.T) storageengine.Engine {
				e, err := mvccadapter.Open(t.TempDir(), storageengine.Options{LocalWAL: wal})
				if err != nil {
					t.Fatal(err)
				}
				return e
			})
		})
	}
}
