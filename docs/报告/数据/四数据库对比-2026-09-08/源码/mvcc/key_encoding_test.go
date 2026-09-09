package mvcc

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestKeyEncodingBoundsAndOwnership(t *testing.T) {
	for _, pair := range []struct {
		space string
		key   []byte
	}{{"rows", []byte{0, 1, 255}}, {"表", nil}, {strings.Repeat("s", 1024), bytes.Repeat([]byte{255}, 8192)}} {
		got, err := key(pair.space, pair.key)
		if err != nil {
			t.Fatal(err)
		}
		want := append(append([]byte(pair.space), 0), pair.key...)
		if !bytes.Equal(got, want) {
			t.Fatal("encoding changed")
		}
		if len(pair.key) > 0 {
			old := got[len(got)-1]
			pair.key[len(pair.key)-1] ^= 255
			if got[len(got)-1] != old {
				t.Fatal("borrowed key")
			}
		}
	}
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.Begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, pair := range []struct {
		space string
		key   []byte
	}{{"", nil}, {"a\x00b", nil}, {strings.Repeat("s", 1025), nil}, {"r", make([]byte, 8193)}} {
		if _, err = key(pair.space, pair.key); err == nil {
			t.Fatal("invalid key accepted")
		}
		if err = validateCommand(Command{Ops: []Op{{Space: pair.space, Key: pair.key}}}); err == nil {
			t.Fatal("invalid command accepted")
		}
		if _, _, err = tx.Get(pair.space, pair.key); err == nil {
			t.Fatal("empty overlay bypassed validation")
		}
	}
	if err = tx.Put("r", []byte("k"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := tx.Get("r", []byte("k")); err != nil || !ok || string(v) != "value" {
		t.Fatal("buffered read", err)
	}
	if err = tx.flushWrites(); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := tx.Get("r", []byte("k")); err != nil || !ok || string(v) != "value" {
		t.Fatal("staged read", err)
	}
}
