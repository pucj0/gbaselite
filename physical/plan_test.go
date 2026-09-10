package physical

import (
	"context"
	"gbaselite/storageengine"
	"testing"
)

func TestDescribeDoesNotOpenOrExecute(t *testing.T) {
	scan := Scan[int]{Open: func(context.Context) (storageengine.Iterator, error) { t.Fatal("opened by Describe"); return nil, nil }}
	join := Join3[int, string, bool]{Left: scan, Right: func(int) (Operator[string], error) { t.Fatal("probe by Describe"); return nil, nil }}
	op := Limit[bool]{Input: Distinct[bool]{Input: Sort[bool]{Input: Filter[bool]{Input: join}}}, Count: 1}
	got := Describe(op)
	if got.String() != "Limit(Distinct(Sort(Filter(Join(Scan, DynamicScan)))))" {
		t.Fatal(got)
	}
	if got.EstimatedRows != nil || got.EstimatedCost != nil || got.ActualRows != nil {
		t.Fatal("fabricated estimate")
	}
}
