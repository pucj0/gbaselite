package executor

import (
	"fmt"
	"gbaselite/parser"
	"testing"
)

func TestParseCacheEvictsOneEntry(t *testing.T) {
	e := &Engine{}
	for i := 0; i < maxParsedStatements; i++ {
		e.cacheStatement(fmt.Sprint(i), parser.Empty{})
	}
	e.cacheStatement("new", parser.Empty{})
	if _, ok := e.parseCache.Load("0"); ok {
		t.Fatal("oldest retained")
	}
	for i := 1; i < maxParsedStatements; i++ {
		if _, ok := e.parseCache.Load(fmt.Sprint(i)); !ok {
			t.Fatal("unrelated entry evicted", i)
		}
	}
	if e.parseCount != maxParsedStatements {
		t.Fatal(e.parseCount)
	}
}
