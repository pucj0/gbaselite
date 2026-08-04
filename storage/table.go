package storage

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Column struct {
	Name              string
	Type              DataType
	SQLType           string
	Length            int
	MetadataVersion   uint8
	Nullable          bool
	HasDefault        bool
	Default           Value
	DefaultExpression string
	AutoIncrement     bool
	Comment           string
	OnUpdate          string
}

type Predicate func(Row) bool

type Index struct {
	Name    string
	Columns []string
	Unique  bool
	Primary bool
}
type ForeignKey struct {
	Name       string
	Columns    []string
	RefTable   string
	RefColumns []string
	OnDelete   string
	OnUpdate   string
}
type CheckConstraint struct{ Name, Expression string }

// Table owns its rows and protects them for concurrent access.
type Table struct {
	mu          sync.RWMutex
	schema      atomic.Pointer[tableSchema]
	name        string
	comment     string
	createdAt   time.Time
	updatedAt   time.Time
	columns     []Column
	columnIndex map[string]int
	indexes     map[string]Index
	foreignKeys []ForeignKey
	checks      []CheckConstraint
	uniqueRows  map[string]map[string]int
	indexRows   map[string][]int
	autoNext    map[string]int64
	dataLength  int64
	rows        []Row
}

type tableSchema struct {
	columns     []Column
	columnIndex map[string]int
}

type TableSnapshot struct {
	Name              string
	Comment           string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Columns           []Column
	Indexes           []Index
	Rows              []Row
	ForeignKeys       []ForeignKey
	Checks            []string
	CheckConstraints  []CheckConstraint
	AutoIncrementNext map[string]int64
}

func (t *Table) SetConstraints(foreignKeys []ForeignKey, checks []string) {
	named := make([]CheckConstraint, len(checks))
	for index, expression := range checks {
		named[index] = CheckConstraint{Expression: expression}
	}
	t.SetNamedConstraints(foreignKeys, named)
}

func (t *Table) SetNamedConstraints(foreignKeys []ForeignKey, checks []CheckConstraint) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.foreignKeys = cloneForeignKeys(foreignKeys)
	t.checks = append([]CheckConstraint(nil), checks...)
	t.assignConstraintNamesLocked()
}
func (t *Table) ForeignKeys() []ForeignKey {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return cloneForeignKeys(t.foreignKeys)
}
func (t *Table) Checks() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	checks := make([]string, len(t.checks))
	for index, check := range t.checks {
		checks[index] = check.Expression
	}
	return checks
}
func (t *Table) CheckConstraints() []CheckConstraint {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]CheckConstraint(nil), t.checks...)
}

func NewTable(name string, columns []Column) (*Table, error) {
	return newTable(name, columns, true)
}

// NewTransientTable builds a query-local table whose qualified column names
// may contain dots. It is never attached to a Database or persisted.
func NewTransientTable(name string, columns []Column) (*Table, error) {
	return newTable(name, columns, false)
}

func newTable(name string, columns []Column, validateColumnNames bool) (*Table, error) {
	if err := validateIdentifier(name); err != nil {
		return nil, fmt.Errorf("table name %q: %w", name, err)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("table %q must contain at least one column", name)
	}

	columnCopy := append([]Column(nil), columns...)
	index := make(map[string]int, len(columnCopy))
	for i := range columnCopy {
		column := &columnCopy[i]
		if column.Name == "" {
			return nil, fmt.Errorf("column name cannot be empty")
		}
		if validateColumnNames {
			if err := validateIdentifier(column.Name); err != nil {
				return nil, fmt.Errorf("column name %q: %w", column.Name, err)
			}
		}
		parsedType, err := ParseDataType(string(column.Type))
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", column.Name, err)
		}
		column.Type = parsedType
		column.SQLType = normalizeColumnSQLType(*column)
		if column.Type == TypeVarchar && column.Length <= 0 {
			return nil, fmt.Errorf("column %q: VARCHAR length must be positive", column.Name)
		}

		key := normalizeName(column.Name)
		if _, exists := index[key]; exists {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateColumn, column.Name)
		}
		index[key] = i
	}

	now := time.Now().UTC()
	table := &Table{
		name:        name,
		createdAt:   now,
		updatedAt:   now,
		columns:     columnCopy,
		columnIndex: index,
		indexes:     make(map[string]Index),
		uniqueRows:  make(map[string]map[string]int),
		indexRows:   make(map[string][]int),
		autoNext:    make(map[string]int64),
		rows:        make([]Row, 0),
	}
	for _, column := range columnCopy {
		if column.AutoIncrement {
			table.autoNext[normalizeName(column.Name)] = 1
		}
	}
	table.publishSchemaLocked()
	return table, nil
}

func (t *Table) Name() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.name
}

func (t *Table) Columns() []Column {
	return append([]Column(nil), t.schema.Load().columns...)
}

// ColumnsView returns an immutable column snapshot without copying it. Table
// schema changes replace the backing slice, so callers may retain the result.
func (t *Table) ColumnsView() []Column {
	return t.schema.Load().columns
}

func (t *Table) ColumnIndex(name string) (int, bool) {
	index := t.schema.Load().columnIndex
	position, ok := index[normalizeName(name)]
	if ok || strings.Contains(name, ".") {
		return position, ok
	}
	suffix := "." + normalizeName(name)
	matched := -1
	for columnName, columnIndex := range index {
		if strings.HasSuffix(columnName, suffix) {
			if matched >= 0 {
				return 0, false
			}
			matched = columnIndex
		}
	}
	return matched, matched >= 0
}

func (t *Table) publishSchemaLocked() {
	t.schema.Store(&tableSchema{columns: t.columns, columnIndex: t.columnIndex})
}

func (t *Table) RowCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.rows)
}

func (t *Table) Metadata() (string, time.Time, time.Time) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.comment, t.createdAt, t.updatedAt
}

func (t *Table) SetComment(comment string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.comment == comment {
		return
	}
	t.comment = comment
	t.updatedAt = time.Now().UTC()
}

func (t *Table) restoreMetadata(comment string, createdAt, updatedAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.comment = comment
	if !createdAt.IsZero() {
		t.createdAt = createdAt.UTC()
	}
	if !updatedAt.IsZero() {
		t.updatedAt = updatedAt.UTC()
	}
}

func (t *Table) DataLength() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.dataLength
}

func (t *Table) touchLocked() {
	t.updatedAt = time.Now().UTC()
}

// NextAutoIncrement atomically reserves the next value. Failed inserts may
// leave gaps, matching MySQL AUTO_INCREMENT behavior.
func (t *Table) NextAutoIncrement(name string) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	position, ok := t.ColumnIndex(name)
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrColumnNotFound, name)
	}
	key := normalizeName(t.columns[position].Name)
	next := t.autoNext[key]
	if next <= 0 {
		next = 1
	}
	t.autoNext[key] = next + 1
	return next, nil
}

// AdvanceAutoIncrement preserves reservations made outside this table, such as
// IDs consumed by a transaction that may later roll back.
func (t *Table) AdvanceAutoIncrement(name string, next int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	position, ok := t.ColumnIndex(name)
	if !ok {
		return fmt.Errorf("%w: %q", ErrColumnNotFound, name)
	}
	column := t.columns[position]
	if !column.AutoIncrement {
		return fmt.Errorf("column %q is not AUTO_INCREMENT", column.Name)
	}
	key := normalizeName(column.Name)
	if next > t.autoNext[key] {
		t.autoNext[key] = next
	}
	return nil
}

func (t *Table) Truncate() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := len(t.rows)
	t.rows = nil
	t.dataLength = 0
	_ = t.rebuildIndexesLocked()
	t.rebuildAutoNextLocked()
	if count > 0 {
		t.touchLocked()
	}
	return count
}

func (t *Table) AddIndex(name string, columns []string, unique bool) error {
	return t.addIndex(name, columns, unique, false)
}

// AddPrimaryKey creates the table's single primary key and makes all of its
// columns non-nullable. Validation completes before table metadata is changed.
func (t *Table) AddPrimaryKey(columns []string) error {
	return t.addIndex("PRIMARY", columns, true, true)
}

func (t *Table) addIndex(name string, columns []string, unique, primary bool) error {
	if err := validateIdentifier(name); err != nil {
		return fmt.Errorf("index name %q: %w", name, err)
	}
	if len(columns) == 0 {
		return fmt.Errorf("index %q must contain at least one column", name)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	resolved := make([]string, len(columns))
	seen := make(map[int]bool, len(columns))
	for position, column := range columns {
		columnPosition, ok := t.ColumnIndex(column)
		if !ok {
			return fmt.Errorf("%w: %q", ErrColumnNotFound, column)
		}
		if seen[columnPosition] {
			return fmt.Errorf("index %q contains duplicate column %q", name, column)
		}
		seen[columnPosition] = true
		resolved[position] = t.columns[columnPosition].Name
	}
	if primary {
		name = "PRIMARY"
	}
	definition := Index{Name: name, Columns: resolved, Unique: unique || primary, Primary: primary}
	if primary {
		for _, existing := range t.indexes {
			if isPrimaryIndex(existing) {
				return fmt.Errorf("%w: %q", ErrIndexExists, "PRIMARY")
			}
		}
		for rowIndex, row := range t.rows {
			for _, column := range resolved {
				if row[t.columnIndex[normalizeName(column)]].Null {
					return fmt.Errorf("primary key column %q contains NULL in row %d", column, rowIndex+1)
				}
			}
		}
	}
	key := normalizeName(name)
	if _, exists := t.indexes[key]; exists {
		return fmt.Errorf("%w: %q", ErrIndexExists, name)
	}
	if unique {
		entries, err := buildUniqueIndexRows(definition, t.columnIndex, t.rows)
		if err != nil {
			return err
		}
		t.uniqueRows[key] = entries
	}
	if primary {
		nextColumns := append([]Column(nil), t.columns...)
		for _, column := range resolved {
			position := t.columnIndex[normalizeName(column)]
			nextColumns[position].MetadataVersion = 1
			nextColumns[position].Nullable = false
		}
		t.columns = nextColumns
	}
	t.indexes[key] = definition
	if err := t.rebuildIndexesLocked(); err != nil {
		delete(t.indexes, key)
		delete(t.uniqueRows, key)
		delete(t.indexRows, key)
		return err
	}
	if primary {
		t.publishSchemaLocked()
	}
	t.touchLocked()
	return nil
}

func (t *Table) DropIndex(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := normalizeName(name)
	if _, exists := t.indexes[key]; !exists {
		return fmt.Errorf("%w: %q", ErrIndexNotFound, name)
	}
	delete(t.indexes, key)
	delete(t.uniqueRows, key)
	delete(t.indexRows, key)
	t.touchLocked()
	return nil
}

// RenameIndex changes only index metadata and keeps the existing ordered and
// unique lookup structures under the new case-insensitive key.
func (t *Table) RenameIndex(oldName, newName string) error {
	if err := validateIdentifier(newName); err != nil {
		return fmt.Errorf("index name %q: %w", newName, err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	oldKey, newKey := normalizeName(oldName), normalizeName(newName)
	definition, exists := t.indexes[oldKey]
	if !exists {
		return fmt.Errorf("%w: %q", ErrIndexNotFound, oldName)
	}
	if definition.Primary || strings.EqualFold(definition.Name, "PRIMARY") {
		return fmt.Errorf("PRIMARY index cannot be renamed")
	}
	if oldKey == newKey {
		definition.Name = newName
		t.indexes[oldKey] = definition
		t.touchLocked()
		return nil
	}
	if _, exists := t.indexes[newKey]; exists {
		return fmt.Errorf("%w: %q", ErrIndexExists, newName)
	}
	delete(t.indexes, oldKey)
	definition.Name = newName
	t.indexes[newKey] = definition
	if rows, ok := t.uniqueRows[oldKey]; ok {
		delete(t.uniqueRows, oldKey)
		t.uniqueRows[newKey] = rows
	}
	if rows, ok := t.indexRows[oldKey]; ok {
		delete(t.indexRows, oldKey)
		t.indexRows[newKey] = rows
	}
	t.touchLocked()
	return nil
}

// AddColumn adds a column and backfills existing rows as one table mutation.
// Position is zero based; a negative position appends the column.
func (t *Table) AddColumn(column Column, fill Value, position int) error {
	if err := validateIdentifier(column.Name); err != nil {
		return fmt.Errorf("column name %q: %w", column.Name, err)
	}
	dataType, err := ParseDataType(string(column.Type))
	if err != nil {
		return fmt.Errorf("column %q: %w", column.Name, err)
	}
	column.Type = dataType
	column.SQLType = normalizeColumnSQLType(column)
	if column.Type == TypeVarchar && column.Length <= 0 {
		return fmt.Errorf("column %q: VARCHAR length must be positive", column.Name)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.columnIndex[normalizeName(column.Name)]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateColumn, column.Name)
	}
	if position < 0 {
		position = len(t.columns)
	}
	if position > len(t.columns) {
		return fmt.Errorf("column position %d is out of range", position)
	}
	if len(t.rows) > 0 {
		if err := validateValue(column, fill); err != nil {
			return fmt.Errorf("backfill column %q: %w", column.Name, err)
		}
	}

	nextColumns := make([]Column, 0, len(t.columns)+1)
	nextColumns = append(nextColumns, t.columns[:position]...)
	nextColumns = append(nextColumns, column)
	nextColumns = append(nextColumns, t.columns[position:]...)
	nextRows := make([]Row, len(t.rows))
	for index, row := range t.rows {
		next := make(Row, 0, len(row)+1)
		next = append(next, row[:position]...)
		next = append(next, fill)
		next = append(next, row[position:]...)
		nextRows[index] = next
	}
	nextIndex := make(map[string]int, len(nextColumns))
	for index, definition := range nextColumns {
		nextIndex[normalizeName(definition.Name)] = index
	}
	t.columns = nextColumns
	t.columnIndex = nextIndex
	t.rows = nextRows
	t.dataLength = rowsDataLength(nextRows)
	if column.AutoIncrement {
		t.autoNext[normalizeName(column.Name)] = 1
	}
	t.rebuildAutoNextLocked()
	if err := t.rebuildIndexesLocked(); err != nil {
		return err
	}
	t.publishSchemaLocked()
	t.touchLocked()
	return nil
}

// DropColumn removes an unreferenced column and rewrites every stored row as
// one table mutation. Callers must remove indexes and constraints first.
func (t *Table) DropColumn(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	position, exists := t.columnIndex[normalizeName(name)]
	if !exists {
		return fmt.Errorf("%w: %q", ErrColumnNotFound, name)
	}
	if len(t.columns) == 1 {
		return fmt.Errorf("%w: a table must retain at least one column", ErrColumnCount)
	}
	for _, definition := range t.indexes {
		for _, column := range definition.Columns {
			if strings.EqualFold(column, name) {
				return fmt.Errorf("%w: column %q is used by index %q", ErrIndexExists, name, definition.Name)
			}
		}
	}
	for _, definition := range t.foreignKeys {
		for _, column := range definition.Columns {
			if strings.EqualFold(column, name) {
				return fmt.Errorf("%w: column %q is used by foreign key %q", ErrForeignKey, name, definition.Name)
			}
		}
		_, referencedTable := splitQualifiedName(definition.RefTable)
		if strings.EqualFold(referencedTable, t.name) {
			for _, column := range definition.RefColumns {
				if strings.EqualFold(column, name) {
					return fmt.Errorf("%w: column %q is referenced by foreign key %q", ErrForeignKeyReferenced, name, definition.Name)
				}
			}
		}
	}
	for _, definition := range t.checks {
		if expressionReferencesIdentifier(definition.Expression, name) {
			return fmt.Errorf("%w: column %q is used by CHECK %q", ErrCheckConstraint, name, definition.Name)
		}
	}

	nextColumns := make([]Column, 0, len(t.columns)-1)
	nextColumns = append(nextColumns, t.columns[:position]...)
	nextColumns = append(nextColumns, t.columns[position+1:]...)
	nextRows := make([]Row, len(t.rows))
	for index, row := range t.rows {
		next := make(Row, 0, len(row)-1)
		next = append(next, row[:position]...)
		next = append(next, row[position+1:]...)
		nextRows[index] = next
	}
	nextIndex := make(map[string]int, len(nextColumns))
	for index, definition := range nextColumns {
		nextIndex[normalizeName(definition.Name)] = index
	}
	delete(t.autoNext, normalizeName(name))
	t.columns = nextColumns
	t.columnIndex = nextIndex
	t.rows = nextRows
	t.dataLength = rowsDataLength(nextRows)
	if err := t.rebuildIndexesLocked(); err != nil {
		return err
	}
	t.rebuildAutoNextLocked()
	t.publishSchemaLocked()
	t.touchLocked()
	return nil
}

func (t *Table) AddForeignKey(foreignKey ForeignKey) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if foreignKey.Name == "" {
		foreignKey.Name = t.nextConstraintNameLocked("ibfk")
	}
	if err := validateIdentifier(foreignKey.Name); err != nil {
		return fmt.Errorf("constraint name %q: %w", foreignKey.Name, err)
	}
	if t.constraintNameExistsLocked(foreignKey.Name) {
		return fmt.Errorf("%w: %q", ErrConstraintExists, foreignKey.Name)
	}
	foreignKey.Columns = append([]string(nil), foreignKey.Columns...)
	foreignKey.RefColumns = append([]string(nil), foreignKey.RefColumns...)
	t.foreignKeys = append(t.foreignKeys, foreignKey)
	t.touchLocked()
	return nil
}

func (t *Table) DropForeignKey(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for index, definition := range t.foreignKeys {
		if strings.EqualFold(definition.Name, name) {
			t.foreignKeys = append(t.foreignKeys[:index], t.foreignKeys[index+1:]...)
			t.touchLocked()
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrConstraintNotFound, name)
}

func (t *Table) AddCheck(check CheckConstraint) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if check.Name == "" {
		check.Name = t.nextConstraintNameLocked("chk")
	}
	if err := validateIdentifier(check.Name); err != nil {
		return fmt.Errorf("constraint name %q: %w", check.Name, err)
	}
	if t.constraintNameExistsLocked(check.Name) {
		return fmt.Errorf("%w: %q", ErrConstraintExists, check.Name)
	}
	t.checks = append(t.checks, check)
	t.touchLocked()
	return nil
}

func (t *Table) DropCheck(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for index, definition := range t.checks {
		if strings.EqualFold(definition.Name, name) {
			t.checks = append(t.checks[:index], t.checks[index+1:]...)
			t.touchLocked()
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrConstraintNotFound, name)
}

func (t *Table) HasUniqueIndex(columns []string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, definition := range t.indexes {
		if !definition.Unique || len(definition.Columns) != len(columns) {
			continue
		}
		matched := true
		for index := range columns {
			if !strings.EqualFold(definition.Columns[index], columns[index]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func (t *Table) rename(name string) {
	t.mu.Lock()
	t.name = name
	t.touchLocked()
	t.mu.Unlock()
}

func (t *Table) renameForeignKeyReferences(renames map[string]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	changed := false
	for index := range t.foreignKeys {
		reference := t.foreignKeys[index].RefTable
		prefix, base := "", reference
		if dot := strings.LastIndex(reference, "."); dot >= 0 {
			prefix, base = reference[:dot+1], reference[dot+1:]
		}
		for oldName, newName := range renames {
			if strings.EqualFold(base, oldName) {
				t.foreignKeys[index].RefTable = prefix + newName
				changed = true
				break
			}
		}
	}
	if changed {
		t.touchLocked()
	}
}

func (t *Table) constraintNameExistsLocked(name string) bool {
	for _, definition := range t.foreignKeys {
		if strings.EqualFold(definition.Name, name) {
			return true
		}
	}
	for _, definition := range t.checks {
		if strings.EqualFold(definition.Name, name) {
			return true
		}
	}
	return false
}

func (t *Table) nextConstraintNameLocked(kind string) string {
	for number := 1; ; number++ {
		candidate := fmt.Sprintf("%s_%s_%d", t.name, kind, number)
		if !t.constraintNameExistsLocked(candidate) {
			return candidate
		}
	}
}

func (t *Table) assignConstraintNamesLocked() {
	for index := range t.foreignKeys {
		if t.foreignKeys[index].Name == "" {
			t.foreignKeys[index].Name = t.nextConstraintNameLocked("ibfk")
		}
	}
	for index := range t.checks {
		if t.checks[index].Name == "" {
			t.checks[index].Name = t.nextConstraintNameLocked("chk")
		}
	}
}

func cloneForeignKeys(source []ForeignKey) []ForeignKey {
	result := make([]ForeignKey, len(source))
	for index, definition := range source {
		result[index] = definition
		result[index].Columns = append([]string(nil), definition.Columns...)
		result[index].RefColumns = append([]string(nil), definition.RefColumns...)
	}
	return result
}

// AlterColumn atomically changes a column definition and converts existing
// values. Index definitions follow the column when it is renamed.
func (t *Table) AlterColumn(oldName string, replacement Column) error {
	if err := validateIdentifier(replacement.Name); err != nil {
		return fmt.Errorf("column name %q: %w", replacement.Name, err)
	}
	dataType, err := ParseDataType(string(replacement.Type))
	if err != nil {
		return fmt.Errorf("column %q: %w", replacement.Name, err)
	}
	replacement.Type = dataType
	replacement.SQLType = normalizeColumnSQLType(replacement)
	if replacement.Type == TypeVarchar && replacement.Length <= 0 {
		return fmt.Errorf("column %q: VARCHAR length must be positive", replacement.Name)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	position, exists := t.columnIndex[normalizeName(oldName)]
	if !exists {
		return fmt.Errorf("%w: %q", ErrColumnNotFound, oldName)
	}
	newKey := normalizeName(replacement.Name)
	if other, duplicate := t.columnIndex[newKey]; duplicate && other != position {
		return fmt.Errorf("%w: %q", ErrDuplicateColumn, replacement.Name)
	}
	if !strings.EqualFold(oldName, replacement.Name) {
		for _, definition := range t.checks {
			if expressionReferencesIdentifier(definition.Expression, oldName) {
				return fmt.Errorf("%w: column %q is used by CHECK %q", ErrCheckConstraint, oldName, definition.Name)
			}
		}
	}
	for _, definition := range t.indexes {
		if !isPrimaryIndex(definition) {
			continue
		}
		for _, column := range definition.Columns {
			if strings.EqualFold(column, oldName) {
				replacement.MetadataVersion = 1
				replacement.Nullable = false
			}
		}
	}

	nextRows := make([]Row, len(t.rows))
	for rowIndex, row := range t.rows {
		nextRows[rowIndex] = cloneRow(row)
		current := row[position]
		var converted Value
		if current.Null {
			converted = NullValue(replacement.Type)
		} else {
			converted, err = NewValue(replacement.Type, current.Interface())
			if err != nil {
				return fmt.Errorf("convert row %d column %q: %w", rowIndex+1, oldName, err)
			}
		}
		if err := validateValue(replacement, converted); err != nil {
			return fmt.Errorf("convert row %d column %q: %w", rowIndex+1, oldName, err)
		}
		nextRows[rowIndex][position] = converted
	}

	nextColumns := append([]Column(nil), t.columns...)
	nextColumns[position] = replacement
	nextColumnIndex := make(map[string]int, len(t.columnIndex))
	for key, index := range t.columnIndex {
		nextColumnIndex[key] = index
	}
	delete(nextColumnIndex, normalizeName(oldName))
	nextColumnIndex[newKey] = position

	nextIndexes := make(map[string]Index, len(t.indexes))
	for key, definition := range t.indexes {
		definition.Columns = append([]string(nil), definition.Columns...)
		for index, column := range definition.Columns {
			if strings.EqualFold(column, oldName) {
				definition.Columns[index] = replacement.Name
			}
		}
		nextIndexes[key] = definition
	}
	nextForeignKeys := cloneForeignKeys(t.foreignKeys)
	for definitionIndex := range nextForeignKeys {
		definition := &nextForeignKeys[definitionIndex]
		for index, column := range definition.Columns {
			if strings.EqualFold(column, oldName) {
				definition.Columns[index] = replacement.Name
			}
		}
		_, referencedTable := splitQualifiedName(definition.RefTable)
		if strings.EqualFold(referencedTable, t.name) {
			for index, column := range definition.RefColumns {
				if strings.EqualFold(column, oldName) {
					definition.RefColumns[index] = replacement.Name
				}
			}
		}
	}
	if err := validateUniqueRows(nextIndexes, nextColumnIndex, nextRows); err != nil {
		return err
	}

	t.columns = nextColumns
	t.columnIndex = nextColumnIndex
	t.indexes = nextIndexes
	t.foreignKeys = nextForeignKeys
	t.rows = nextRows
	t.dataLength = rowsDataLength(nextRows)
	if !strings.EqualFold(oldName, replacement.Name) {
		if next, exists := t.autoNext[normalizeName(oldName)]; exists {
			delete(t.autoNext, normalizeName(oldName))
			t.autoNext[newKey] = next
		}
	}
	if err := t.rebuildUniqueRowsLocked(); err != nil {
		return err
	}
	if err := t.rebuildIndexesLocked(); err != nil {
		return err
	}
	t.rebuildAutoNextLocked()
	t.publishSchemaLocked()
	t.touchLocked()
	return nil
}

func (t *Table) renameReferencedColumn(referencedTable, oldName, newName string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	changed := false
	for definitionIndex := range t.foreignKeys {
		definition := &t.foreignKeys[definitionIndex]
		_, tableName := splitQualifiedName(definition.RefTable)
		if !strings.EqualFold(tableName, referencedTable) {
			continue
		}
		for index, column := range definition.RefColumns {
			if strings.EqualFold(column, oldName) {
				definition.RefColumns[index] = newName
				changed = true
			}
		}
	}
	if changed {
		t.touchLocked()
	}
}

func splitQualifiedName(name string) (string, string) {
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		return name[:dot], name[dot+1:]
	}
	return "", name
}

func expressionReferencesIdentifier(expression, name string) bool {
	for index := 0; index < len(expression); {
		for index < len(expression) && !isIdentifierByte(expression[index]) {
			index++
		}
		start := index
		for index < len(expression) && isIdentifierByte(expression[index]) {
			index++
		}
		if start < index && strings.EqualFold(expression[start:index], name) {
			return true
		}
	}
	return false
}

func isIdentifierByte(value byte) bool {
	return value == '_' || value == '$' || value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= 0x80
}

func (t *Table) Indexes() []Index {
	t.mu.RLock()
	defer t.mu.RUnlock()
	indexes := make([]Index, 0, len(t.indexes))
	for _, definition := range t.indexes {
		definition.Columns = append([]string(nil), definition.Columns...)
		indexes = append(indexes, definition)
	}
	sort.Slice(indexes, func(i, j int) bool { return strings.ToLower(indexes[i].Name) < strings.ToLower(indexes[j].Name) })
	return indexes
}

// ColumnKey returns the MySQL SHOW COLUMNS key marker for a column.
func (t *Table) ColumnKey(name string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	key := ""
	for _, definition := range t.indexes {
		if len(definition.Columns) == 0 || !strings.EqualFold(definition.Columns[0], name) {
			continue
		}
		if isPrimaryIndex(definition) {
			return "PRI"
		}
		if definition.Unique {
			key = "UNI"
		} else if key == "" {
			key = "MUL"
		}
	}
	return key
}

func isPrimaryIndex(definition Index) bool {
	return definition.Primary || strings.EqualFold(definition.Name, "PRIMARY")
}

// ColumnNullable preserves the historical behavior of snapshots created
// before column constraints were persisted: those columns remain nullable.
func ColumnNullable(column Column) bool {
	if column.MetadataVersion == 0 {
		return true
	}
	return column.Nullable
}

func (t *Table) Snapshot() TableSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return TableSnapshot{Name: t.name, Comment: t.comment, CreatedAt: t.createdAt, UpdatedAt: t.updatedAt, Columns: append([]Column(nil), t.columns...), Indexes: t.indexesLocked(), Rows: cloneRows(t.rows), ForeignKeys: cloneForeignKeys(t.foreignKeys), CheckConstraints: append([]CheckConstraint(nil), t.checks...), AutoIncrementNext: t.autoIncrementNextLocked()}
}

// persistenceSnapshot copies the row slice but shares the immutable row data.
// Stored rows are never modified in place: update replaces a complete row and
// delete only changes the table's row slice. This keeps persistence snapshots
// consistent without duplicating every Value in memory.
func (t *Table) persistenceSnapshot() TableSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return TableSnapshot{
		Name:              t.name,
		Comment:           t.comment,
		CreatedAt:         t.createdAt,
		UpdatedAt:         t.updatedAt,
		Columns:           append([]Column(nil), t.columns...),
		Indexes:           t.indexesLocked(),
		Rows:              append([]Row(nil), t.rows...),
		ForeignKeys:       cloneForeignKeys(t.foreignKeys),
		CheckConstraints:  append([]CheckConstraint(nil), t.checks...),
		AutoIncrementNext: t.autoIncrementNextLocked(),
	}
}

func (t *Table) autoIncrementNextLocked() map[string]int64 {
	next := make(map[string]int64, len(t.autoNext))
	for name, value := range t.autoNext {
		next[name] = value
	}
	return next
}

func (t *Table) indexesLocked() []Index {
	indexes := make([]Index, 0, len(t.indexes))
	for _, definition := range t.indexes {
		definition.Columns = append([]string(nil), definition.Columns...)
		indexes = append(indexes, definition)
	}
	sort.Slice(indexes, func(i, j int) bool { return strings.ToLower(indexes[i].Name) < strings.ToLower(indexes[j].Name) })
	return indexes
}

func (t *Table) Insert(row Row) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	rowCopy, err := t.validateAndCloneRow(row)
	if err != nil {
		return err
	}

	if err := t.validateUniqueCandidateLocked(rowCopy, t.rows); err != nil {
		return err
	}
	position := len(t.rows)
	t.rows = append(t.rows, rowCopy)
	t.dataLength += rowDataLength(rowCopy)
	for key, definition := range t.indexes {
		if !definition.Unique {
			continue
		}
		encoded, comparable := indexKey(definition, t.columnIndex, rowCopy)
		if comparable {
			t.uniqueRows[key][encoded] = position
		}
	}
	if err := t.rebuildIndexesLocked(); err != nil {
		return err
	}
	t.updateAutoNextLocked(rowCopy)
	t.touchLocked()
	return nil
}

func (t *Table) Select(predicate Predicate) []Row {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make([]Row, 0, len(t.rows))
	for _, row := range t.rows {
		candidate := cloneRow(row)
		if predicate == nil || predicate(candidate) {
			result = append(result, candidate)
		}
	}
	return result
}

// ConflictingRowIndexes returns every row that conflicts with candidate on at
// least one PRIMARY or UNIQUE index. NULL values do not conflict.
func (t *Table) ConflictingRowIndexes(candidate Row) ([]int, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(candidate) != len(t.columns) {
		return nil, fmt.Errorf("%w: table %q expects %d values, got %d", ErrColumnCount, t.name, len(t.columns), len(candidate))
	}
	conflicts := make([]int, 0)
	for rowIndex, row := range t.rows {
		matched := false
		for _, definition := range t.indexes {
			if !definition.Unique {
				continue
			}
			equal := true
			for _, column := range definition.Columns {
				position := t.columnIndex[normalizeName(column)]
				if candidate[position].Null || row[position].Null || compareValue(candidate[position], row[position]) != 0 {
					equal = false
					break
				}
			}
			if equal {
				matched = true
				break
			}
		}
		if matched {
			conflicts = append(conflicts, rowIndex)
		}
	}
	return conflicts, nil
}

// Count returns the number of matching rows without allocating row copies.
func (t *Table) Count(predicate Predicate) int {
	t.mu.RLock()
	defer t.mu.RUnlock()

	count := 0
	for _, row := range t.rows {
		if predicate == nil || predicate(row) {
			count++
		}
	}
	return count
}

// Visit calls visitor for every matching row while holding a read lock. Rows
// are read-only and must not be retained or modified by the visitor.
func (t *Table) Visit(predicate Predicate, visitor func(Row) error) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, row := range t.rows {
		if predicate != nil && !predicate(row) {
			continue
		}
		if err := visitor(row); err != nil {
			return err
		}
	}
	return nil
}

// Project scans matching rows and converts only the requested columns to
// result values. A negative limit means unlimited. Callers must provide valid
// column indexes.
func (t *Table) Project(predicate Predicate, selected []int, offset, limit int) [][]any {
	if offset < 0 {
		offset = 0
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	capacity := len(t.rows)
	if limit >= 0 && limit < capacity {
		capacity = limit
	}

	result := make([][]any, 0, capacity)
	skipped := 0
	for _, row := range t.rows {
		if predicate != nil && !predicate(row) {
			continue
		}
		if skipped < offset {
			skipped++
			continue
		}
		if limit >= 0 && len(result) >= limit {
			break
		}
		projected := make([]any, len(selected))
		for i, index := range selected {
			projected[i] = row[index].Interface()
		}
		result = append(result, projected)
	}
	return result
}

// StreamProject sends projected rows to yield one at a time. Each projected
// row has independent ownership so callers may retain it after yield returns.
func (t *Table) StreamProject(predicate Predicate, selected []int, offset, limit int, yield func([]any) error) error {
	return streamRowSnapshot(t.rowSnapshot(), predicate, offset, limit, func(row Row) error {
		projected := make([]any, len(selected))
		for i, index := range selected {
			projected[i] = row[index].Interface()
		}
		return yield(projected)
	})
}

// Stream visits complete matching rows without projecting Values through
// interface{}. The row must be treated as read-only and only retained for the
// duration of yield.
func (t *Table) Stream(predicate Predicate, offset, limit int, yield func(Row) error) error {
	return streamRowSnapshot(t.rowSnapshot(), predicate, offset, limit, yield)
}

func (t *Table) rowSnapshot() []Row {
	t.mu.RLock()
	rows := t.rows
	t.mu.RUnlock()
	return rows
}

func (t *Table) restoreRows(rows []Row) error {
	for _, row := range rows {
		if len(row) != len(t.columns) {
			return fmt.Errorf("%w: table %q expects %d values, got %d", ErrColumnCount, t.name, len(t.columns), len(row))
		}
		for index, value := range row {
			if err := validateValue(t.columns[index], value); err != nil {
				return err
			}
		}
	}
	t.mu.Lock()
	t.rows = rows
	t.dataLength = rowsDataLength(rows)
	t.rebuildAutoNextLocked()
	t.mu.Unlock()
	return nil
}

func (t *Table) restoreAutoIncrementNext(next map[string]int64) error {
	if len(next) == 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for name, value := range next {
		position, ok := t.columnIndex[normalizeName(name)]
		if !ok {
			return fmt.Errorf("%w: %q", ErrColumnNotFound, name)
		}
		if !t.columns[position].AutoIncrement {
			return fmt.Errorf("column %q is not AUTO_INCREMENT", t.columns[position].Name)
		}
		key := normalizeName(t.columns[position].Name)
		if value > t.autoNext[key] {
			t.autoNext[key] = value
		}
	}
	return nil
}

func streamRowSnapshot(rows []Row, predicate Predicate, offset, limit int, yield func(Row) error) error {
	if offset < 0 {
		offset = 0
	}
	if limit == 0 {
		return nil
	}
	skipped := 0
	matched := 0
	for _, row := range rows {
		if predicate != nil && !predicate(row) {
			continue
		}
		if skipped < offset {
			skipped++
			continue
		}
		if limit >= 0 && matched >= limit {
			break
		}
		if err := yield(row); err != nil {
			return err
		}
		matched++
	}
	return nil
}

// Update applies column values to every matching row and returns the number of
// affected rows. A nil predicate matches all rows.
func (t *Table) Update(predicate Predicate, changes map[string]Value) (int, error) {
	return t.UpdateLimit(predicate, changes, -1)
}

// UpdateLimit applies changes to at most limit matching rows. A negative
// limit means unlimited, matching UPDATE without a LIMIT clause.
func (t *Table) UpdateLimit(predicate Predicate, changes map[string]Value, limit int) (int, error) {
	if len(changes) == 0 {
		return 0, nil
	}
	if limit == 0 {
		return 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	type change struct {
		index int
		value Value
	}
	prepared := make([]change, 0, len(changes))
	keys := make([]string, 0, len(changes))
	seenColumns := make(map[int]string, len(changes))
	for name := range changes {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		index, ok := t.ColumnIndex(name)
		if !ok {
			return 0, fmt.Errorf("%w: %q", ErrColumnNotFound, name)
		}
		if previous, exists := seenColumns[index]; exists {
			return 0, fmt.Errorf("duplicate update column %q (also specified as %q)", name, previous)
		}
		seenColumns[index] = name
		value := changes[name]
		if err := validateValue(t.columns[index], value); err != nil {
			return 0, err
		}
		prepared = append(prepared, change{index: index, value: value})
	}

	affected := 0
	nextRows := append([]Row(nil), t.rows...)
	nextDataLength := t.dataLength
	for i, row := range t.rows {
		candidate := cloneRow(row)
		if predicate != nil && !predicate(candidate) {
			continue
		}
		for _, item := range prepared {
			candidate[item.index] = item.value
		}
		nextRows[i] = candidate
		nextDataLength += rowDataLength(candidate) - rowDataLength(row)
		affected++
		if limit >= 0 && affected >= limit {
			break
		}
	}
	indexedChange := false
	for _, definition := range t.indexes {
		if !definition.Unique {
			continue
		}
		for _, column := range definition.Columns {
			for changedColumn := range changes {
				if strings.EqualFold(column, changedColumn) {
					indexedChange = true
					break
				}
			}
		}
	}
	if indexedChange {
		if err := t.validateUniqueRowsLocked(nextRows); err != nil {
			return 0, err
		}
	}
	t.rows = nextRows
	t.dataLength = nextDataLength
	if indexedChange {
		if err := t.rebuildIndexesLocked(); err != nil {
			return 0, err
		}
	} else if affected > 0 {
		if err := t.rebuildIndexesLocked(); err != nil {
			return 0, err
		}
	}
	t.rebuildAutoNextLocked()
	if affected > 0 {
		t.touchLocked()
	}
	return affected, nil
}

// ReplaceRowsLimit atomically replaces matching rows in scan order. It is used
// by expression UPDATE, where each row may receive different computed values.
func (t *Table) ReplaceRowsLimit(predicate Predicate, replacements []Row, limit int) (int, error) {
	if len(replacements) == 0 || limit == 0 {
		return 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	nextRows := append([]Row(nil), t.rows...)
	replacementIndex := 0
	for index, row := range t.rows {
		if predicate != nil && !predicate(cloneRow(row)) {
			continue
		}
		if limit >= 0 && replacementIndex >= limit {
			break
		}
		if replacementIndex >= len(replacements) {
			return 0, fmt.Errorf("replacement row count does not match UPDATE predicate")
		}
		candidate := cloneRow(replacements[replacementIndex])
		if len(candidate) != len(t.columns) {
			return 0, fmt.Errorf("%w: got %d values for %d columns", ErrColumnCount, len(candidate), len(t.columns))
		}
		for columnIndex, value := range candidate {
			if err := validateValue(t.columns[columnIndex], value); err != nil {
				return 0, err
			}
		}
		nextRows[index] = candidate
		replacementIndex++
	}
	if replacementIndex != len(replacements) {
		return 0, fmt.Errorf("replacement row count does not match UPDATE predicate")
	}
	if err := t.validateUniqueRowsLocked(nextRows); err != nil {
		return 0, err
	}
	t.rows = nextRows
	t.dataLength = rowsDataLength(nextRows)
	if err := t.rebuildIndexesLocked(); err != nil {
		return 0, err
	}
	t.rebuildAutoNextLocked()
	t.touchLocked()
	return replacementIndex, nil
}

func (t *Table) validateRows(rows []Row) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, row := range rows {
		if len(row) != len(t.columns) {
			return fmt.Errorf("%w: table %q expects %d values, got %d", ErrColumnCount, t.name, len(t.columns), len(row))
		}
		for index, value := range row {
			if err := validateValue(t.columns[index], value); err != nil {
				return err
			}
		}
	}
	return validateUniqueRows(t.indexes, t.columnIndex, rows)
}

func (t *Table) replaceAllRows(rows []Row) error {
	if err := t.validateRows(rows); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rows = cloneRows(rows)
	t.dataLength = rowsDataLength(t.rows)
	if err := t.rebuildIndexesLocked(); err != nil {
		return err
	}
	for _, row := range t.rows {
		t.updateAutoNextLocked(row)
	}
	t.touchLocked()
	return nil
}

func (t *Table) validateUniqueCandidateLocked(candidate Row, existing []Row) error {
	for key, definition := range t.indexes {
		if !definition.Unique {
			continue
		}
		candidateKey, comparable := t.indexKey(definition, candidate)
		if !comparable {
			continue
		}
		if _, exists := t.uniqueRows[key][candidateKey]; exists {
			return fmt.Errorf("%w for index %q", ErrDuplicateKey, definition.Name)
		}
	}
	return nil
}

func (t *Table) validateUniqueRowsLocked(rows []Row) error {
	for _, definition := range t.indexes {
		if definition.Unique {
			if err := t.validateUniqueIndexRowsLocked(definition, rows); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *Table) validateUniqueIndexRowsLocked(definition Index, rows []Row) error {
	_, err := buildUniqueIndexRows(definition, t.columnIndex, rows)
	return err
}

func (t *Table) indexKey(definition Index, row Row) (string, bool) {
	return indexKey(definition, t.columnIndex, row)
}

func validateUniqueRows(indexes map[string]Index, columnIndex map[string]int, rows []Row) error {
	for _, definition := range indexes {
		if !definition.Unique {
			continue
		}
		seen := make(map[string]struct{}, len(rows))
		for _, row := range rows {
			key, comparable := indexKey(definition, columnIndex, row)
			if !comparable {
				continue
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("%w for index %q", ErrDuplicateKey, definition.Name)
			}
			seen[key] = struct{}{}
		}
	}
	return nil
}

func indexKey(definition Index, columnIndex map[string]int, row Row) (string, bool) {
	key := make([]byte, 0, len(definition.Columns)*16)
	for _, column := range definition.Columns {
		position := columnIndex[normalizeName(column)]
		value := row[position]
		if value.Null {
			return "", false
		}
		key = appendIndexValueKey(key, value)
	}
	return string(key), true
}

func appendIndexValueKey(key []byte, value Value) []byte {
	key = append(key, byte(len(value.Type)))
	key = append(key, value.Type...)
	key = append(key, ':')
	switch value.Type {
	case TypeInt, TypeBigInt:
		key = strconv.AppendInt(key, value.Int64, 10)
	case TypeFloat, TypeDouble:
		key = strconv.AppendUint(key, math.Float64bits(value.Float), 16)
	case TypeVarchar, TypeText:
		key = strconv.AppendInt(key, int64(len(value.Text)), 10)
		key = append(key, ':')
		key = append(key, value.Text...)
	case TypeBoolean:
		if value.Bool {
			key = append(key, '1')
		} else {
			key = append(key, '0')
		}
	case TypeDate, TypeDateTime:
		key = strconv.AppendInt(key, value.Date.UnixNano(), 10)
	}
	return append(key, ';')
}

// LookupUnique resolves a single-column primary/unique key without scanning.
func (t *Table) LookupUnique(column string, value Value) (Row, bool, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for key, definition := range t.indexes {
		if !definition.Unique || len(definition.Columns) != 1 || !strings.EqualFold(definition.Columns[0], column) {
			continue
		}
		encoded := string(appendIndexValueKey(make([]byte, 0, 24), value))
		position, found := t.uniqueRows[key][encoded]
		if !found || position < 0 || position >= len(t.rows) {
			return nil, false, true
		}
		return t.rows[position], true, true
	}
	return nil, false, false
}

func buildUniqueIndexRows(definition Index, columnIndex map[string]int, rows []Row) (map[string]int, error) {
	entries := make(map[string]int, len(rows))
	for position, row := range rows {
		key, comparable := indexKey(definition, columnIndex, row)
		if !comparable {
			continue
		}
		if _, exists := entries[key]; exists {
			return nil, fmt.Errorf("%w for index %q", ErrDuplicateKey, definition.Name)
		}
		entries[key] = position
	}
	return entries, nil
}

func (t *Table) rebuildUniqueRowsLocked() error {
	next := make(map[string]map[string]int)
	for key, definition := range t.indexes {
		if !definition.Unique {
			continue
		}
		entries, err := buildUniqueIndexRows(definition, t.columnIndex, t.rows)
		if err != nil {
			return err
		}
		next[key] = entries
	}
	t.uniqueRows = next
	return nil
}

func (t *Table) rebuildIndexesLocked() error {
	if err := t.rebuildUniqueRowsLocked(); err != nil {
		return err
	}
	next := make(map[string][]int, len(t.indexes))
	for key, definition := range t.indexes {
		positions := make([]int, len(t.rows))
		for index := range positions {
			positions[index] = index
		}
		sort.SliceStable(positions, func(left, right int) bool {
			comparison := compareRowsByIndex(t.rows[positions[left]], t.rows[positions[right]], definition, t.columnIndex)
			if comparison == 0 {
				return positions[left] < positions[right]
			}
			return comparison < 0
		})
		next[key] = positions
	}
	t.indexRows = next
	return nil
}

func compareRowsByIndex(left, right Row, definition Index, columnIndex map[string]int) int {
	for _, column := range definition.Columns {
		position := columnIndex[normalizeName(column)]
		if comparison := compareValue(left[position], right[position]); comparison != 0 {
			return comparison
		}
	}
	return 0
}

type IndexBound struct {
	Value     Value
	Inclusive bool
}

type IndexScan struct {
	Name        string
	EqualPrefix []Value
	Lower       *IndexBound
	Upper       *IndexBound
	Descending  bool
}

// ScanIndex returns rows from a bounded B-tree-style index interval. The
// predicate is reapplied to preserve SQL semantics for residual conditions.
func (t *Table) ScanIndex(scan IndexScan, predicate Predicate, offset, limit int) ([]Row, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	definition, exists := t.indexes[normalizeName(scan.Name)]
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrIndexNotFound, scan.Name)
	}
	if len(scan.EqualPrefix) > len(definition.Columns) || len(scan.EqualPrefix) == len(definition.Columns) && (scan.Lower != nil || scan.Upper != nil) {
		return nil, fmt.Errorf("invalid bounds for index %q", scan.Name)
	}
	positions := t.indexRows[normalizeName(scan.Name)]
	comparePrefix := func(row Row) int {
		for index, value := range scan.EqualPrefix {
			position := t.columnIndex[normalizeName(definition.Columns[index])]
			if comparison := compareValue(row[position], value); comparison != 0 {
				return comparison
			}
		}
		return 0
	}
	start := sort.Search(len(positions), func(index int) bool { return comparePrefix(t.rows[positions[index]]) >= 0 })
	end := sort.Search(len(positions), func(index int) bool { return comparePrefix(t.rows[positions[index]]) > 0 })
	if len(scan.EqualPrefix) == 0 {
		start, end = 0, len(positions)
	}
	if len(scan.EqualPrefix) < len(definition.Columns) {
		columnPosition := t.columnIndex[normalizeName(definition.Columns[len(scan.EqualPrefix)])]
		if scan.Lower != nil {
			bound := *scan.Lower
			start += sort.Search(end-start, func(index int) bool {
				comparison := compareValue(t.rows[positions[start+index]][columnPosition], bound.Value)
				return comparison > 0 || bound.Inclusive && comparison == 0
			})
		}
		if scan.Upper != nil {
			bound := *scan.Upper
			end = start + sort.Search(end-start, func(index int) bool {
				comparison := compareValue(t.rows[positions[start+index]][columnPosition], bound.Value)
				return comparison > 0 || !bound.Inclusive && comparison == 0
			})
		}
	}
	if offset < 0 {
		offset = 0
	}
	result := make([]Row, 0)
	visit := func(position int) bool {
		row := t.rows[positions[position]]
		if predicate != nil && !predicate(row) {
			return true
		}
		if offset > 0 {
			offset--
			return true
		}
		if limit >= 0 && len(result) >= limit {
			return false
		}
		result = append(result, cloneRow(row))
		return true
	}
	if scan.Descending {
		for index := end - 1; index >= start; index-- {
			if !visit(index) {
				break
			}
		}
	} else {
		for index := start; index < end; index++ {
			if !visit(index) {
				break
			}
		}
	}
	return result, nil
}

func (t *Table) updateAutoNextLocked(row Row) {
	for position, column := range t.columns {
		if !column.AutoIncrement || row[position].Null {
			continue
		}
		key := normalizeName(column.Name)
		if candidate := row[position].Int64 + 1; candidate > t.autoNext[key] {
			t.autoNext[key] = candidate
		}
	}
}

func (t *Table) rebuildAutoNextLocked() {
	if len(t.autoNext) == 0 {
		return
	}
	for key := range t.autoNext {
		t.autoNext[key] = 1
	}
	for _, row := range t.rows {
		t.updateAutoNextLocked(row)
	}
}

// Delete removes every matching row and returns the number of affected rows.
// A nil predicate matches all rows.
func (t *Table) Delete(predicate Predicate) int {
	return t.DeleteLimit(predicate, -1)
}

// DeleteLimit removes at most limit matching rows. A negative limit means
// unlimited, matching DELETE without a LIMIT clause.
func (t *Table) DeleteLimit(predicate Predicate, limit int) int {
	if limit == 0 {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	kept := make([]Row, 0, len(t.rows))
	var keptDataLength int64
	deleted := 0
	for _, row := range t.rows {
		matches := predicate == nil || predicate(cloneRow(row))
		if matches && (limit < 0 || deleted < limit) {
			deleted++
			continue
		}
		kept = append(kept, row)
		keptDataLength += rowDataLength(row)
	}
	t.rows = kept
	t.dataLength = keptDataLength
	_ = t.rebuildIndexesLocked()
	if deleted > 0 {
		t.touchLocked()
	}
	return deleted
}

func (t *Table) validateAndCloneRow(row Row) (Row, error) {
	if len(row) != len(t.columns) {
		return nil, fmt.Errorf("%w: table %q expects %d values, got %d", ErrColumnCount, t.name, len(t.columns), len(row))
	}
	for i, value := range row {
		if err := validateValue(t.columns[i], value); err != nil {
			return nil, err
		}
	}
	return cloneRow(row), nil
}

func validateValue(column Column, value Value) error {
	if value.Type != column.Type {
		return fmt.Errorf("%w: column %q expects %s, got %s", ErrTypeMismatch, column.Name, column.Type, value.Type)
	}
	if value.Null && !ColumnNullable(column) {
		return fmt.Errorf("column %q cannot be null", column.Name)
	}
	if !value.Null && column.Type == TypeVarchar && len([]rune(value.Text)) > column.Length {
		return fmt.Errorf("column %q exceeds VARCHAR(%d)", column.Name, column.Length)
	}
	if !value.Null && column.Type == TypeText {
		sqlType := strings.ToUpper(strings.TrimSpace(column.SQLType))
		if strings.HasPrefix(sqlType, "JSON") {
			if !json.Valid([]byte(value.Text)) {
				return fmt.Errorf("%w for column %q", ErrInvalidJSON, column.Name)
			}
		}
		if strings.HasPrefix(sqlType, "ENUM(") {
			allowed := strings.TrimSuffix(strings.TrimPrefix(column.SQLType, "enum("), ")")
			valid := false
			for _, item := range strings.Split(allowed, ",") {
				if strings.Trim(item, " '") == value.Text {
					valid = true
					break
				}
			}
			if !valid {
				return fmt.Errorf("%w for column %q", ErrInvalidEnum, column.Name)
			}
		}
	}
	return nil
}

func rowsDataLength(rows []Row) int64 {
	var total int64
	for _, row := range rows {
		total += rowDataLength(row)
	}
	return total
}

func rowDataLength(row Row) int64 {
	var total int64
	for _, value := range row {
		if value.Null {
			continue
		}
		switch value.Type {
		case TypeInt, TypeFloat:
			total += 4
		case TypeBigInt, TypeDouble, TypeDateTime:
			total += 8
		case TypeBoolean:
			total++
		case TypeDate:
			total += 3
		case TypeVarchar, TypeText:
			total += int64(len(value.Text))
		}
	}
	return total
}

func cloneRows(rows []Row) []Row {
	copyRows := make([]Row, len(rows))
	for i, row := range rows {
		copyRows[i] = cloneRow(row)
	}
	return copyRows
}

func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
