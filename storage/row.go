package storage

// Row stores values in the same order as the table's columns.
type Row []Value

func NewRow(values ...Value) Row {
	return cloneRow(values)
}

func cloneRow(row Row) Row {
	return append(Row(nil), row...)
}
