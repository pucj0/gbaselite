// Package sqllayout owns SQL logical keyspace encoding. The byte strings are
// persistent format contracts: do not normalize or alter them during refactors.
package sqllayout

import "encoding/binary"

const Catalog = "catalog"
const DatabasePrefix = "db/"
const TablePrefix = "table/"

func Rows(id string) string                  { return "row/" + id }
func UniqueIndex(id, name string) string     { return "index/" + id + "/" + name }
func SecondaryIndex(id, name string) string  { return "secondary/" + id + "/" + name }
func DatabaseKey(name string) []byte         { return []byte(DatabasePrefix + name) }
func TableKey(database, table string) []byte { return []byte(TablePrefix + database + "/" + table) }
func TablesPrefix(database string) string    { return TablePrefix + database + "/" }
func Counter(id, column string) string       { return id + "/" + column }
func SignedInteger(n int64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, uint64(n)^(uint64(1)<<63))
	return key
}
