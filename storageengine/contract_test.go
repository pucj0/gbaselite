package storageengine_test

import (
	"context"
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

type coreFixture struct{ inner storageengine.Engine }

func (e coreFixture) Begin(ctx context.Context) (storageengine.Txn, error) { return e.inner.Begin(ctx) }
func (e coreFixture) Close() error                                         { return e.inner.Close() }
func TestCoreOnlyContract(t *testing.T) {
	testkit.Run(t, func(*testing.T) storageengine.Engine { return coreFixture{testkit.NewMemory()} })
}
