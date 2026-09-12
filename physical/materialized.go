package physical

import "context"

// Materialized replays an owned, immutable row slice. It is the relation form of
// a derived table or CTE: the rows were produced once by the pipeline under the
// statement result budget and now act as a read-only input to the enclosing
// plan, so the operator owns no resources and can be run more than once.
type Materialized[T any] struct {
	Rows []T
}

func (m Materialized[T]) Run(ctx context.Context, yield Yield[T]) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, row := range m.Rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := yield(row); err != nil {
			return err
		}
	}
	return nil
}
