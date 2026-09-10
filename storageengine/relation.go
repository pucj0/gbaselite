package storageengine

import (
	"context"
)

// Table and Index are transactional logical handles, not physical bbolt buckets.
// Values are SQL-owned encodings; this shared layout keeps backend replacement
// independent of SQL operators and preserves the existing on-disk namespaces.
type Keyspace interface {
	Get([]byte) ([]byte, bool, error)
	Put([]byte, []byte) error
	Delete([]byte) error
	Guard([]byte) error
	GuardRange(KeyRange) error
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
