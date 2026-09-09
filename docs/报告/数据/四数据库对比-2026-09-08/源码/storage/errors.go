package storage

import "errors"

var (
	ErrInvalidIdentifier    = errors.New("invalid identifier")
	ErrInvalidDataType      = errors.New("invalid data type")
	ErrDatabaseExists       = errors.New("database already exists")
	ErrDatabaseNotFound     = errors.New("database not found")
	ErrTableExists          = errors.New("table already exists")
	ErrTableNotFound        = errors.New("table not found")
	ErrViewExists           = errors.New("view already exists")
	ErrViewNotFound         = errors.New("view not found")
	ErrColumnNotFound       = errors.New("column not found")
	ErrDuplicateColumn      = errors.New("duplicate column")
	ErrIndexExists          = errors.New("index already exists")
	ErrIndexNotFound        = errors.New("index not found")
	ErrDuplicateKey         = errors.New("duplicate key")
	ErrColumnCount          = errors.New("column count mismatch")
	ErrTypeMismatch         = errors.New("value type mismatch")
	ErrForeignKey           = errors.New("foreign key constraint fails")
	ErrForeignKeyReferenced = errors.New("foreign key constraint prevents parent change")
	ErrCheckConstraint      = errors.New("check constraint fails")
	ErrConstraintExists     = errors.New("constraint already exists")
	ErrConstraintNotFound   = errors.New("constraint not found")
	ErrInvalidJSON          = errors.New("invalid JSON value")
	ErrInvalidEnum          = errors.New("invalid ENUM value")
)
