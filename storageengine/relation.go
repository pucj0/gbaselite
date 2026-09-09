package storageengine

import "context"

// Table and Index are transactional logical handles, not physical bbolt buckets.
// Values are SQL-owned encodings; this shared layout keeps backend replacement
// independent of SQL operators and preserves the existing on-disk namespaces.
type Keyspace interface {
	Get([]byte) ([]byte, bool, error)
	Put([]byte, []byte) error
	Delete([]byte) error
	Guard([]byte) error
	GuardRange() error
	Scan(context.Context, ScanRequest) (Iterator, error)
}
type Table interface {
	Keyspace
	Index(name string, kind IndexKind) Index
}
type Index interface{ Keyspace }
type IndexKind uint8

const (
	UniqueIndex IndexKind = iota
	SecondaryIndex
)

type spaceHandle struct {
	tx    Txn
	space string
}
type tableHandle struct {
	spaceHandle
	id string
}

func BindTable(tx Txn, id string) Table { return &tableHandle{spaceHandle{tx, "row/" + id}, id} }
func (t *tableHandle) Index(name string, kind IndexKind) Index {
	prefix := "index/"
	if kind == SecondaryIndex {
		prefix = "secondary/"
	}
	return &spaceHandle{t.tx, prefix + t.id + "/" + name}
}
func (s *spaceHandle) Get(k []byte) ([]byte, bool, error) { return s.tx.Get(s.space, k) }
func (s *spaceHandle) Put(k, v []byte) error              { return s.tx.Put(s.space, k, v) }
func (s *spaceHandle) Delete(k []byte) error              { return s.tx.Delete(s.space, k) }
func (s *spaceHandle) Guard(k []byte) error               { return s.tx.Guard(s.space, k) }
func (s *spaceHandle) GuardRange() error                  { return s.tx.GuardRange(s.space) }
func (s *spaceHandle) Scan(ctx context.Context, r ScanRequest) (Iterator, error) {
	r.Space = s.space
	return s.tx.NewIterator(ctx, r)
}

// Consume always closes the iterator, even if yield stops a scan with an error.
func Consume(iterator Iterator, yield func([]byte, []byte) error) (err error) {
	defer func() {
		if closeErr := iterator.Close(); err == nil {
			err = closeErr
		}
	}()
	for iterator.Next() {
		if err = yield(iterator.Key(), iterator.Value()); err != nil {
			return err
		}
	}
	return iterator.Err()
}
