package executor

import "gbaselite/storage"

func startQuery(session *Session, options QueryOptions) func() {
	previous := session.query
	if previous == nil {
		session.query = newQueryControl(session.Context, options)
	}
	return func() { session.query = previous }
}

func checkQuery(session *Session) error {
	if session == nil {
		return nil
	}
	return session.query.check()
}

// bindQueryResult retains the execution deadline during deferred protocol
// streaming, after executeStatement restores the session's outer query state.
func bindQueryResult(result *Result, q *queryControl) (*Result, error) {
	if result == nil {
		return nil, nil
	}
	if err := q.check(); err != nil {
		return nil, err
	}
	if q == nil {
		return result, nil
	}
	if q.options.ResultMemoryBytes > 0 {
		used := int64(0)
		for _, row := range result.Rows {
			var err error
			used, err = checkResultMemory(q.options.ResultMemoryBytes, used, row)
			if err != nil {
				return nil, err
			}
		}
	}
	if !q.hasDeadline() {
		return result, nil
	}
	if stream := result.StreamRows; stream != nil {
		result.StreamRows = func(yield func([]any) error) error {
			if err := q.check(); err != nil {
				return err
			}
			err := stream(func(row []any) error {
				if err := q.check(); err != nil {
					return err
				}
				return yield(row)
			})
			if err != nil {
				return err
			}
			return q.check()
		}
	}
	if stream := result.StreamValues; stream != nil {
		result.StreamValues = func(yield func(storage.Row) error) error {
			if err := q.check(); err != nil {
				return err
			}
			err := stream(func(row storage.Row) error {
				if err := q.check(); err != nil {
					return err
				}
				return yield(row)
			})
			if err != nil {
				return err
			}
			return q.check()
		}
	}
	return result, nil
}
