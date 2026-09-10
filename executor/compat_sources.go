package executor

import (
	"context"
	"gbaselite/physical"
	"gbaselite/storage"
)

// sourceOperator is restricted to historical SQL helpers and materialized API
// boundaries. Core binding passes Operators directly.
func sourceOperator(source func(func(storage.Row) error) error) physical.Operator[storage.Row] {
	return physical.Source[storage.Row](func(_ context.Context, y physical.Yield[storage.Row]) error { return source(y) })
}
