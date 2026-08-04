package storage

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// Database owns a case-insensitive collection of tables.
type Database struct {
	mu           sync.RWMutex
	constraintMu sync.Mutex
	name         string
	tables       map[string]*Table
	views        map[string]View
}

type View struct {
	Name       string
	Definition string
	Columns    []string
}

type ViewSnapshot struct {
	Name       string
	Definition string
	Columns    []string
}

type DatabaseSnapshot struct {
	Name   string
	Tables []TableSnapshot
	Views  []ViewSnapshot
}

type StoreSnapshot struct {
	// FormatVersion is zero for snapshots written before the version marker was
	// introduced. Persistence.Load upgrades that legacy shape in memory before
	// validating it and writes the current version only on a later save.
	FormatVersion uint16
	Databases     []DatabaseSnapshot
}

// RowMutation describes explicit row changes using stable indexes from a
// table snapshot. Referential actions are derived and are not counted in the
// affected-row result.
type RowMutation struct {
	Table        string
	Delete       []int
	Replacements map[int]Row
	Inserts      []Row
}

func NewDatabase(name string) (*Database, error) {
	if err := validateIdentifier(name); err != nil {
		return nil, fmt.Errorf("database name %q: %w", name, err)
	}
	return &Database{name: name, tables: make(map[string]*Table), views: make(map[string]View)}, nil
}

func (d *Database) Name() string {
	return d.name
}

func (d *Database) CreateTable(name string, columns []Column) (*Table, error) {
	return d.CreateTableWithIndexes(name, columns, nil, nil)
}

func (d *Database) CreateTableWithPrimary(name string, columns []Column, primaryKey []string) (*Table, error) {
	return d.CreateTableWithIndexes(name, columns, primaryKey, nil)
}

func (d *Database) CreateTableWithIndexes(name string, columns []Column, primaryKey []string, indexes []Index) (*Table, error) {
	table, err := NewTable(name, columns)
	if err != nil {
		return nil, err
	}
	if len(primaryKey) > 0 {
		if err := table.AddPrimaryKey(primaryKey); err != nil {
			return nil, err
		}
	}
	for _, definition := range indexes {
		if err := table.AddIndex(definition.Name, definition.Columns, definition.Unique); err != nil {
			return nil, err
		}
	}
	key := normalizeName(name)

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.tables[key]; exists {
		return nil, fmt.Errorf("%w: %q", ErrTableExists, name)
	}
	if _, exists := d.views[key]; exists {
		return nil, fmt.Errorf("%w: %q is a view", ErrTableExists, name)
	}
	d.tables[key] = table
	return table, nil
}

func (d *Database) DropTable(name string) error {
	key := normalizeName(name)
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.tables[key]; !exists {
		return fmt.Errorf("%w: %q", ErrTableNotFound, name)
	}
	delete(d.tables, key)
	return nil
}

func (d *Database) RenameTables(pairs map[string]string) error {
	if len(pairs) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	sources := make(map[string]*Table, len(pairs))
	targets := make(map[string]bool, len(pairs))
	for from, to := range pairs {
		if err := validateIdentifier(to); err != nil {
			return fmt.Errorf("table name %q: %w", to, err)
		}
		fromKey, toKey := normalizeName(from), normalizeName(to)
		table, exists := d.tables[fromKey]
		if !exists {
			return fmt.Errorf("%w: %q", ErrTableNotFound, from)
		}
		if targets[toKey] {
			return fmt.Errorf("duplicate rename target %q", to)
		}
		targets[toKey] = true
		sources[fromKey] = table
		if _, exists := d.views[toKey]; exists {
			return fmt.Errorf("%w: %q is a view", ErrTableExists, to)
		}
		if _, exists := d.tables[toKey]; exists {
			if _, beingRenamed := pairs[d.tables[toKey].Name()]; !beingRenamed {
				found := false
				for candidate := range pairs {
					if strings.EqualFold(candidate, d.tables[toKey].Name()) {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("%w: %q", ErrTableExists, to)
				}
			}
		}
	}
	for from := range pairs {
		delete(d.tables, normalizeName(from))
	}
	for from, to := range pairs {
		table := sources[normalizeName(from)]
		table.rename(to)
		d.tables[normalizeName(to)] = table
	}
	for _, table := range d.tables {
		table.renameForeignKeyReferences(pairs)
	}
	return nil
}

func (d *Database) Table(name string) (*Table, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	table, exists := d.tables[normalizeName(name)]
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrTableNotFound, name)
	}
	return table, nil
}

func (d *Database) ListTables() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	names := make([]string, 0, len(d.tables))
	for _, table := range d.tables {
		names = append(names, table.Name())
	}
	sort.Strings(names)
	return names
}

func (d *Database) CreateView(name, definition string, columns []string, replace bool) error {
	if err := validateIdentifier(name); err != nil {
		return fmt.Errorf("view name %q: %w", name, err)
	}
	if strings.TrimSpace(definition) == "" {
		return fmt.Errorf("view %q has an empty definition", name)
	}
	for _, column := range columns {
		if err := validateIdentifier(column); err != nil {
			return fmt.Errorf("view column %q: %w", column, err)
		}
	}
	key := normalizeName(name)
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.tables[key]; exists {
		return fmt.Errorf("%w: %q is a table", ErrViewExists, name)
	}
	if _, exists := d.views[key]; exists && !replace {
		return fmt.Errorf("%w: %q", ErrViewExists, name)
	}
	d.views[key] = View{Name: name, Definition: definition, Columns: append([]string(nil), columns...)}
	return nil
}

func (d *Database) DropView(name string) error {
	key := normalizeName(name)
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.views[key]; !exists {
		return fmt.Errorf("%w: %q", ErrViewNotFound, name)
	}
	delete(d.views, key)
	return nil
}

func (d *Database) View(name string) (View, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	view, exists := d.views[normalizeName(name)]
	if !exists {
		return View{}, fmt.Errorf("%w: %q", ErrViewNotFound, name)
	}
	view.Columns = append([]string(nil), view.Columns...)
	return view, nil
}

func (d *Database) ListViews() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	names := make([]string, 0, len(d.views))
	for _, view := range d.views {
		names = append(names, view.Name)
	}
	sort.Strings(names)
	return names
}

func (d *Database) ListRelations() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	names := make([]string, 0, len(d.tables)+len(d.views))
	for _, table := range d.tables {
		names = append(names, table.Name())
	}
	for _, view := range d.views {
		names = append(names, view.Name)
	}
	sort.Strings(names)
	return names
}

func (d *Database) Snapshot() DatabaseSnapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	tables := make([]TableSnapshot, 0, len(d.tables))
	for _, table := range d.tables {
		tables = append(tables, table.Snapshot())
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })
	views := d.viewSnapshots()
	return DatabaseSnapshot{Name: d.name, Tables: tables, Views: views}
}

func (d *Database) persistenceSnapshot() DatabaseSnapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	tables := make([]TableSnapshot, 0, len(d.tables))
	for _, table := range d.tables {
		tables = append(tables, table.persistenceSnapshot())
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })
	views := d.viewSnapshots()
	return DatabaseSnapshot{Name: d.name, Tables: tables, Views: views}
}

func (d *Database) viewSnapshots() []ViewSnapshot {
	views := make([]ViewSnapshot, 0, len(d.views))
	for _, view := range d.views {
		views = append(views, ViewSnapshot{Name: view.Name, Definition: view.Definition, Columns: append([]string(nil), view.Columns...)})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views
}

func (d *Database) Insert(tableName string, row Row) error {
	d.constraintMu.Lock()
	defer d.constraintMu.Unlock()
	table, err := d.Table(tableName)
	if err != nil {
		return err
	}
	if err := d.validateForeignKeys(tableName, row); err != nil {
		return err
	}
	return table.Insert(row)
}

func (d *Database) ValidateForeignKeys(tableName string, row Row) error {
	d.constraintMu.Lock()
	defer d.constraintMu.Unlock()
	return d.validateForeignKeys(tableName, row)
}

func (d *Database) validateForeignKeys(tableName string, row Row) error {
	table, err := d.Table(tableName)
	if err != nil {
		return err
	}
	for _, foreignKey := range table.ForeignKeys() {
		if len(foreignKey.Columns) != len(foreignKey.RefColumns) {
			return fmt.Errorf("%w: invalid definition", ErrForeignKey)
		}
		referencedTable := foreignKey.RefTable
		if dot := strings.LastIndex(referencedTable, "."); dot >= 0 {
			referencedTable = referencedTable[dot+1:]
		}
		parent, parentErr := d.Table(referencedTable)
		if parentErr != nil {
			return fmt.Errorf("%w: referenced table %s", ErrForeignKey, foreignKey.RefTable)
		}
		childPositions := make([]int, len(foreignKey.Columns))
		parentPositions := make([]int, len(foreignKey.Columns))
		nullReference := false
		for index, column := range foreignKey.Columns {
			childPos, ok := table.ColumnIndex(column)
			if !ok {
				return fmt.Errorf("%w: column %s", ErrForeignKey, column)
			}
			childPositions[index] = childPos
			if row[childPos].Null {
				nullReference = true
				break
			}
			parentPos, ok := parent.ColumnIndex(foreignKey.RefColumns[index])
			if !ok {
				return fmt.Errorf("%w: referenced column %s", ErrForeignKey, foreignKey.RefColumns[index])
			}
			parentPositions[index] = parentPos
		}
		if nullReference {
			continue
		}
		valid := false
		for _, candidate := range parent.Select(nil) {
			matched := true
			for index := range childPositions {
				if compareValue(candidate[parentPositions[index]], row[childPositions[index]]) != 0 {
					matched = false
					break
				}
			}
			if matched {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("%w: %s", ErrForeignKey, tableName)
		}
	}
	return nil
}

func (d *Database) AddForeignKey(tableName string, foreignKey ForeignKey) error {
	d.constraintMu.Lock()
	defer d.constraintMu.Unlock()
	if len(foreignKey.Columns) == 0 || len(foreignKey.Columns) != len(foreignKey.RefColumns) {
		return fmt.Errorf("%w: foreign key column counts do not match", ErrForeignKey)
	}
	for _, action := range []string{foreignKey.OnDelete, foreignKey.OnUpdate} {
		action = normalizeReferentialAction(action)
		if action != "" && action != "RESTRICT" && action != "NO ACTION" && action != "CASCADE" && action != "SET NULL" {
			return fmt.Errorf("%w: referential action %s is not supported", ErrForeignKey, action)
		}
	}
	child, err := d.Table(tableName)
	if err != nil {
		return err
	}
	referencedDatabase, referencedTable := "", foreignKey.RefTable
	if dot := strings.LastIndex(referencedTable, "."); dot >= 0 {
		referencedDatabase, referencedTable = referencedTable[:dot], referencedTable[dot+1:]
	}
	if referencedDatabase != "" && !strings.EqualFold(referencedDatabase, d.name) {
		return fmt.Errorf("%w: cross-database reference %s", ErrForeignKey, foreignKey.RefTable)
	}
	parent, err := d.Table(referencedTable)
	if err != nil {
		return fmt.Errorf("%w: referenced table %s", ErrForeignKey, foreignKey.RefTable)
	}
	childColumns, parentColumns := child.ColumnsView(), parent.ColumnsView()
	for index, name := range foreignKey.Columns {
		childPosition, ok := child.ColumnIndex(name)
		if !ok {
			return fmt.Errorf("%w: column %s", ErrForeignKey, name)
		}
		parentPosition, ok := parent.ColumnIndex(foreignKey.RefColumns[index])
		if !ok {
			return fmt.Errorf("%w: referenced column %s", ErrForeignKey, foreignKey.RefColumns[index])
		}
		if childColumns[childPosition].Type != parentColumns[parentPosition].Type {
			return fmt.Errorf("%w: incompatible columns %s and %s", ErrForeignKey, name, foreignKey.RefColumns[index])
		}
	}
	if normalizeReferentialAction(foreignKey.OnDelete) == "SET NULL" || normalizeReferentialAction(foreignKey.OnUpdate) == "SET NULL" {
		for _, name := range foreignKey.Columns {
			position, _ := child.ColumnIndex(name)
			if !ColumnNullable(childColumns[position]) {
				return fmt.Errorf("%w: SET NULL column %s.%s is not nullable", ErrForeignKey, tableName, name)
			}
		}
	}
	foreignKey.OnDelete = normalizeReferentialAction(foreignKey.OnDelete)
	foreignKey.OnUpdate = normalizeReferentialAction(foreignKey.OnUpdate)
	if !parent.HasUniqueIndex(foreignKey.RefColumns) {
		return fmt.Errorf("%w: referenced columns must be PRIMARY or UNIQUE", ErrForeignKey)
	}
	foreignKey.RefTable = referencedTable
	for rowIndex, row := range child.Select(nil) {
		if err := d.validateForeignKey(child, row, foreignKey); err != nil {
			return fmt.Errorf("%w in existing row %d", err, rowIndex+1)
		}
	}
	return child.AddForeignKey(foreignKey)
}

func (d *Database) DropColumn(tableName, columnName string) error {
	d.constraintMu.Lock()
	defer d.constraintMu.Unlock()
	table, err := d.Table(tableName)
	if err != nil {
		return err
	}
	for _, childName := range d.ListTables() {
		child, _ := d.Table(childName)
		for _, foreignKey := range child.ForeignKeys() {
			_, referencedTable := splitQualifiedName(foreignKey.RefTable)
			if !strings.EqualFold(referencedTable, table.Name()) {
				continue
			}
			for _, referencedColumn := range foreignKey.RefColumns {
				if strings.EqualFold(referencedColumn, columnName) {
					return fmt.Errorf("%w: column %s.%s is referenced by %s.%s", ErrForeignKeyReferenced, table.Name(), columnName, childName, foreignKey.Name)
				}
			}
		}
	}
	return table.DropColumn(columnName)
}

func (d *Database) AlterColumn(tableName, oldName string, replacement Column) error {
	d.constraintMu.Lock()
	defer d.constraintMu.Unlock()
	table, err := d.Table(tableName)
	if err != nil {
		return err
	}
	for _, childName := range d.ListTables() {
		child, _ := d.Table(childName)
		for _, foreignKey := range child.ForeignKeys() {
			_, referencedTable := splitQualifiedName(foreignKey.RefTable)
			if strings.EqualFold(childName, table.Name()) {
				for index, localColumn := range foreignKey.Columns {
					if !strings.EqualFold(localColumn, oldName) {
						continue
					}
					parent, parentErr := d.Table(referencedTable)
					if parentErr != nil {
						return parentErr
					}
					parentPosition, _ := parent.ColumnIndex(foreignKey.RefColumns[index])
					if parent.ColumnsView()[parentPosition].Type != replacement.Type {
						return fmt.Errorf("%w: altered foreign key column %s.%s has an incompatible type", ErrForeignKey, table.Name(), oldName)
					}
				}
			}
			if !strings.EqualFold(referencedTable, table.Name()) {
				continue
			}
			for index, referencedColumn := range foreignKey.RefColumns {
				if !strings.EqualFold(referencedColumn, oldName) {
					continue
				}
				childPosition, _ := child.ColumnIndex(foreignKey.Columns[index])
				if child.ColumnsView()[childPosition].Type != replacement.Type {
					return fmt.Errorf("%w: altered referenced column %s.%s has an incompatible type", ErrForeignKey, table.Name(), oldName)
				}
			}
		}
	}
	if err := table.AlterColumn(oldName, replacement); err != nil {
		return err
	}
	if !strings.EqualFold(oldName, replacement.Name) {
		for _, childName := range d.ListTables() {
			child, _ := d.Table(childName)
			if child != table {
				child.renameReferencedColumn(table.Name(), oldName, replacement.Name)
			}
		}
	}
	return nil
}

func (d *Database) validateForeignKey(child *Table, row Row, foreignKey ForeignKey) error {
	parent, err := d.Table(foreignKey.RefTable)
	if err != nil {
		return fmt.Errorf("%w: referenced table %s", ErrForeignKey, foreignKey.RefTable)
	}
	childPositions := make([]int, len(foreignKey.Columns))
	parentPositions := make([]int, len(foreignKey.Columns))
	for index, column := range foreignKey.Columns {
		childPositions[index], _ = child.ColumnIndex(column)
		if row[childPositions[index]].Null {
			return nil
		}
		parentPositions[index], _ = parent.ColumnIndex(foreignKey.RefColumns[index])
	}
	for _, candidate := range parent.Select(nil) {
		matched := true
		for index := range childPositions {
			if compareValue(candidate[parentPositions[index]], row[childPositions[index]]) != 0 {
				matched = false
				break
			}
		}
		if matched {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrForeignKey, child.Name())
}

func compareValue(left, right Value) int {
	if left.Null && right.Null {
		return 0
	}
	if left.Null {
		return -1
	}
	if right.Null {
		return 1
	}
	if left.Type != right.Type {
		return -1
	}
	switch left.Type {
	case TypeInt, TypeBigInt:
		if left.Int64 < right.Int64 {
			return -1
		}
		if left.Int64 > right.Int64 {
			return 1
		}
		return 0
	case TypeFloat, TypeDouble:
		if left.Float < right.Float {
			return -1
		}
		if left.Float > right.Float {
			return 1
		}
		return 0
	case TypeVarchar, TypeText:
		return strings.Compare(left.Text, right.Text)
	case TypeBoolean:
		if left.Bool == right.Bool {
			return 0
		}
		if !left.Bool {
			return -1
		}
		return 1
	case TypeDate, TypeDateTime:
		if left.Date.Before(right.Date) {
			return -1
		}
		if left.Date.After(right.Date) {
			return 1
		}
		return 0
	}
	return -1
}

func (d *Database) Select(tableName string, predicate Predicate) ([]Row, error) {
	table, err := d.Table(tableName)
	if err != nil {
		return nil, err
	}
	return table.Select(predicate), nil
}

func (d *Database) Update(tableName string, predicate Predicate, changes map[string]Value) (int, error) {
	return d.UpdateLimit(tableName, predicate, changes, -1)
}

func (d *Database) UpdateLimit(tableName string, predicate Predicate, changes map[string]Value, limit int) (int, error) {
	table, err := d.Table(tableName)
	if err != nil {
		return 0, err
	}
	snapshot := table.Snapshot()
	replacements := make(map[int]Row)
	for rowIndex, row := range snapshot.Rows {
		candidate := cloneRow(row)
		if predicate != nil && !predicate(candidate) {
			continue
		}
		if limit >= 0 && len(replacements) >= limit {
			break
		}
		for name, value := range changes {
			position, ok := table.ColumnIndex(name)
			if !ok {
				return 0, fmt.Errorf("%w: %q", ErrColumnNotFound, name)
			}
			candidate[position] = value
		}
		replacements[rowIndex] = candidate
	}
	return d.ApplyRowMutations([]RowMutation{{Table: tableName, Replacements: replacements}})
}

func (d *Database) ReplaceRowsLimit(tableName string, predicate Predicate, replacements []Row, limit int) (int, error) {
	table, err := d.Table(tableName)
	if err != nil {
		return 0, err
	}
	snapshot := table.Snapshot()
	indexed := make(map[int]Row, len(replacements))
	replacementIndex := 0
	for rowIndex, row := range snapshot.Rows {
		if predicate != nil && !predicate(cloneRow(row)) {
			continue
		}
		if limit >= 0 && replacementIndex >= limit {
			break
		}
		if replacementIndex >= len(replacements) {
			return 0, fmt.Errorf("replacement row count does not match UPDATE predicate")
		}
		indexed[rowIndex] = replacements[replacementIndex]
		replacementIndex++
	}
	if replacementIndex != len(replacements) {
		return 0, fmt.Errorf("replacement row count does not match UPDATE predicate")
	}
	return d.ApplyRowMutations([]RowMutation{{Table: tableName, Replacements: indexed}})
}

func (d *Database) validateParentUpdate(tableName string, parent *Table, current, candidate Row) error {
	for _, childName := range d.ListTables() {
		child, _ := d.Table(childName)
		for _, foreignKey := range child.ForeignKeys() {
			reference := foreignKey.RefTable
			if dot := strings.LastIndex(reference, "."); dot >= 0 {
				reference = reference[dot+1:]
			}
			if !strings.EqualFold(reference, tableName) {
				continue
			}
			changed := false
			for _, parentColumn := range foreignKey.RefColumns {
				position, _ := parent.ColumnIndex(parentColumn)
				if compareValue(current[position], candidate[position]) != 0 {
					changed = true
					break
				}
			}
			if !changed {
				continue
			}
			for _, childRow := range child.Select(nil) {
				matched := true
				for index, childColumn := range foreignKey.Columns {
					childPosition, _ := child.ColumnIndex(childColumn)
					parentPosition, _ := parent.ColumnIndex(foreignKey.RefColumns[index])
					if childRow[childPosition].Null || compareValue(childRow[childPosition], current[parentPosition]) != 0 {
						matched = false
						break
					}
				}
				if matched {
					return fmt.Errorf("%w: table %s is referenced by %s.%s", ErrForeignKeyReferenced, tableName, childName, foreignKey.Name)
				}
			}
		}
	}
	return nil
}

func (d *Database) Delete(tableName string, predicate Predicate) (int, error) {
	return d.DeleteLimit(tableName, predicate, -1)
}

func (d *Database) DeleteLimit(tableName string, predicate Predicate, limit int) (int, error) {
	table, err := d.Table(tableName)
	if err != nil {
		return 0, err
	}
	snapshot := table.Snapshot()
	indexes := make([]int, 0)
	for rowIndex, row := range snapshot.Rows {
		if predicate != nil && !predicate(cloneRow(row)) {
			continue
		}
		if limit >= 0 && len(indexes) >= limit {
			break
		}
		indexes = append(indexes, rowIndex)
	}
	return d.ApplyRowMutations([]RowMutation{{Table: tableName, Delete: indexes}})
}

// ReplaceRow applies MySQL REPLACE semantics: every PRIMARY/UNIQUE conflict is
// deleted, referential actions run for those deletions, and the candidate is
// inserted as one database mutation.
func (d *Database) ReplaceRow(tableName string, candidate Row) (int, error) {
	d.constraintMu.Lock()
	defer d.constraintMu.Unlock()
	table, err := d.Table(tableName)
	if err != nil {
		return 0, err
	}
	conflicts, err := table.ConflictingRowIndexes(candidate)
	if err != nil {
		return 0, err
	}
	return d.applyRowMutationsLocked([]RowMutation{{Table: tableName, Delete: conflicts, Inserts: []Row{candidate}}})
}

func (d *Database) ApplyRowMutations(mutations []RowMutation) (int, error) {
	d.constraintMu.Lock()
	defer d.constraintMu.Unlock()
	return d.applyRowMutationsLocked(mutations)
}

type stagedTableMutation struct {
	table  *Table
	rows   []Row
	active []bool
}

type referentialEvent struct {
	table   string
	oldRow  Row
	newRow  Row
	deleted bool
}

func (d *Database) applyRowMutationsLocked(mutations []RowMutation) (int, error) {
	staged := make(map[string]*stagedTableMutation)
	for _, tableName := range d.ListTables() {
		table, _ := d.Table(tableName)
		snapshot := table.Snapshot()
		active := make([]bool, len(snapshot.Rows))
		for index := range active {
			active[index] = true
		}
		staged[normalizeName(tableName)] = &stagedTableMutation{table: table, rows: snapshot.Rows, active: active}
	}

	changed := make(map[string]bool)
	explicit := make(map[string]map[int]string)
	events := make([]referentialEvent, 0)
	pendingInserts := make(map[string][]Row)
	affected := 0
	for _, mutation := range mutations {
		_, tableName := splitQualifiedName(mutation.Table)
		key := normalizeName(tableName)
		state, exists := staged[key]
		if !exists {
			return 0, fmt.Errorf("%w: %q", ErrTableNotFound, mutation.Table)
		}
		if explicit[key] == nil {
			explicit[key] = make(map[int]string)
		}
		for rowIndex, replacement := range mutation.Replacements {
			if rowIndex < 0 || rowIndex >= len(state.rows) {
				return 0, fmt.Errorf("row index %d is out of range for table %s", rowIndex, tableName)
			}
			if previous := explicit[key][rowIndex]; previous != "" {
				return 0, fmt.Errorf("row %d of table %s has duplicate %s and UPDATE mutations", rowIndex, tableName, previous)
			}
			explicit[key][rowIndex] = "UPDATE"
			oldRow := cloneRow(state.rows[rowIndex])
			state.rows[rowIndex] = cloneRow(replacement)
			events = append(events, referentialEvent{table: key, oldRow: oldRow, newRow: cloneRow(replacement)})
			changed[key] = true
			affected++
		}
		for _, rowIndex := range mutation.Delete {
			if rowIndex < 0 || rowIndex >= len(state.rows) {
				return 0, fmt.Errorf("row index %d is out of range for table %s", rowIndex, tableName)
			}
			if previous := explicit[key][rowIndex]; previous != "" {
				return 0, fmt.Errorf("row %d of table %s has duplicate %s and DELETE mutations", rowIndex, tableName, previous)
			}
			explicit[key][rowIndex] = "DELETE"
			if state.active[rowIndex] {
				state.active[rowIndex] = false
				events = append(events, referentialEvent{table: key, oldRow: cloneRow(state.rows[rowIndex]), deleted: true})
				changed[key] = true
				affected++
			}
		}
		for _, row := range mutation.Inserts {
			pendingInserts[key] = append(pendingInserts[key], cloneRow(row))
			changed[key] = true
			affected++
		}
	}

	maxEvents := 1000
	for _, state := range staged {
		maxEvents += len(state.rows) * (len(staged) + 1) * 4
	}
	for eventIndex := 0; eventIndex < len(events); eventIndex++ {
		if eventIndex >= maxEvents {
			return 0, fmt.Errorf("%w: cascading foreign key actions did not converge", ErrForeignKey)
		}
		event := events[eventIndex]
		parent := staged[event.table]
		for childKey, child := range staged {
			for _, foreignKey := range child.table.ForeignKeys() {
				_, reference := splitQualifiedName(foreignKey.RefTable)
				if normalizeName(reference) != event.table {
					continue
				}
				if !event.deleted && !referencedKeyChanged(parent.table, event.oldRow, event.newRow, foreignKey) {
					continue
				}
				action := foreignKey.OnUpdate
				if event.deleted {
					action = foreignKey.OnDelete
				}
				action = normalizeReferentialAction(action)
				if action == "" || action == "NO ACTION" {
					action = "RESTRICT"
				}
				for rowIndex := range child.rows {
					if !child.active[rowIndex] || !rowReferencesParent(child.table, child.rows[rowIndex], parent.table, event.oldRow, foreignKey) {
						continue
					}
					switch action {
					case "RESTRICT":
						return 0, fmt.Errorf("%w: table %s is referenced by %s.%s", ErrForeignKeyReferenced, parent.table.Name(), child.table.Name(), foreignKey.Name)
					case "CASCADE":
						if event.deleted {
							child.active[rowIndex] = false
							events = append(events, referentialEvent{table: childKey, oldRow: cloneRow(child.rows[rowIndex]), deleted: true})
							changed[childKey] = true
							continue
						}
						oldRow := cloneRow(child.rows[rowIndex])
						for columnIndex, childColumn := range foreignKey.Columns {
							childPosition, _ := child.table.ColumnIndex(childColumn)
							parentPosition, _ := parent.table.ColumnIndex(foreignKey.RefColumns[columnIndex])
							child.rows[rowIndex][childPosition] = event.newRow[parentPosition]
						}
						if !rowsEqual(oldRow, child.rows[rowIndex]) {
							events = append(events, referentialEvent{table: childKey, oldRow: oldRow, newRow: cloneRow(child.rows[rowIndex])})
							changed[childKey] = true
						}
					case "SET NULL":
						oldRow := cloneRow(child.rows[rowIndex])
						for _, childColumn := range foreignKey.Columns {
							position, _ := child.table.ColumnIndex(childColumn)
							column := child.table.ColumnsView()[position]
							if !ColumnNullable(column) {
								return 0, fmt.Errorf("%w: SET NULL column %s.%s is not nullable", ErrForeignKey, child.table.Name(), childColumn)
							}
							child.rows[rowIndex][position] = NullValue(column.Type)
						}
						if !rowsEqual(oldRow, child.rows[rowIndex]) {
							events = append(events, referentialEvent{table: childKey, oldRow: oldRow, newRow: cloneRow(child.rows[rowIndex])})
							changed[childKey] = true
						}
					default:
						return 0, fmt.Errorf("%w: referential action %s is not supported", ErrForeignKey, action)
					}
				}
			}
		}
	}

	finalRows := make(map[string][]Row, len(staged))
	for key, state := range staged {
		rows := make([]Row, 0, len(state.rows)+len(pendingInserts[key]))
		for rowIndex, row := range state.rows {
			if state.active[rowIndex] {
				rows = append(rows, cloneRow(row))
			}
		}
		rows = append(rows, pendingInserts[key]...)
		finalRows[key] = rows
		if err := state.table.validateRows(rows); err != nil {
			return 0, err
		}
	}
	if err := validateStagedForeignKeys(staged, finalRows); err != nil {
		return 0, err
	}
	keys := make([]string, 0, len(changed))
	for key := range changed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := staged[key].table.replaceAllRows(finalRows[key]); err != nil {
			return 0, err
		}
	}
	return affected, nil
}

func normalizeReferentialAction(action string) string {
	return strings.ToUpper(strings.Join(strings.Fields(action), " "))
}

func referencedKeyChanged(parent *Table, oldRow, newRow Row, foreignKey ForeignKey) bool {
	for _, column := range foreignKey.RefColumns {
		position, _ := parent.ColumnIndex(column)
		if compareValue(oldRow[position], newRow[position]) != 0 {
			return true
		}
	}
	return false
}

func rowReferencesParent(child *Table, childRow Row, parent *Table, parentRow Row, foreignKey ForeignKey) bool {
	for index, childColumn := range foreignKey.Columns {
		childPosition, _ := child.ColumnIndex(childColumn)
		parentPosition, _ := parent.ColumnIndex(foreignKey.RefColumns[index])
		if childRow[childPosition].Null || compareValue(childRow[childPosition], parentRow[parentPosition]) != 0 {
			return false
		}
	}
	return true
}

func rowsEqual(left, right Row) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if compareValue(left[index], right[index]) != 0 {
			return false
		}
	}
	return true
}

func validateStagedForeignKeys(staged map[string]*stagedTableMutation, rows map[string][]Row) error {
	for childKey, child := range staged {
		for _, foreignKey := range child.table.ForeignKeys() {
			_, reference := splitQualifiedName(foreignKey.RefTable)
			parent := staged[normalizeName(reference)]
			if parent == nil {
				return fmt.Errorf("%w: referenced table %s", ErrForeignKey, foreignKey.RefTable)
			}
			for _, childRow := range rows[childKey] {
				nullReference := false
				for _, column := range foreignKey.Columns {
					position, _ := child.table.ColumnIndex(column)
					if childRow[position].Null {
						nullReference = true
						break
					}
				}
				if nullReference {
					continue
				}
				found := false
				for _, parentRow := range rows[normalizeName(reference)] {
					if rowReferencesParent(child.table, childRow, parent.table, parentRow, foreignKey) {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("%w: %s", ErrForeignKey, child.table.Name())
				}
			}
		}
	}
	return nil
}

func (d *Database) Truncate(tableName string) (int, error) {
	d.constraintMu.Lock()
	defer d.constraintMu.Unlock()
	table, err := d.Table(tableName)
	if err != nil {
		return 0, err
	}
	if table.RowCount() > 0 {
		for _, childName := range d.ListTables() {
			child, _ := d.Table(childName)
			for _, foreignKey := range child.ForeignKeys() {
				reference := foreignKey.RefTable
				if dot := strings.LastIndex(reference, "."); dot >= 0 {
					reference = reference[dot+1:]
				}
				if strings.EqualFold(reference, tableName) && child.RowCount() > 0 {
					return 0, fmt.Errorf("%w: table %s is referenced by %s.%s", ErrForeignKeyReferenced, tableName, childName, foreignKey.Name)
				}
			}
		}
	}
	return table.Truncate(), nil
}

// Store owns all in-memory databases.
type Store struct {
	mu        sync.RWMutex
	databases map[string]*Database
}

func NewStore() *Store {
	return &Store{databases: make(map[string]*Database)}
}

func (s *Store) CreateDatabase(name string) (*Database, error) {
	database, err := NewDatabase(name)
	if err != nil {
		return nil, err
	}
	key := normalizeName(name)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.databases[key]; exists {
		return nil, fmt.Errorf("%w: %q", ErrDatabaseExists, name)
	}
	s.databases[key] = database
	return database, nil
}

func (s *Store) DropDatabase(name string) error {
	key := normalizeName(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.databases[key]; !exists {
		return fmt.Errorf("%w: %q", ErrDatabaseNotFound, name)
	}
	delete(s.databases, key)
	return nil
}

func (s *Store) Database(name string) (*Database, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	database, exists := s.databases[normalizeName(name)]
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrDatabaseNotFound, name)
	}
	return database, nil
}

func (s *Store) ListDatabases() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.databases))
	for _, database := range s.databases {
		names = append(names, database.Name())
	}
	sort.Strings(names)
	return names
}

func (s *Store) Snapshot() StoreSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	databases := make([]DatabaseSnapshot, 0, len(s.databases))
	for _, database := range s.databases {
		databases = append(databases, database.Snapshot())
	}
	sort.Slice(databases, func(i, j int) bool { return databases[i].Name < databases[j].Name })
	return StoreSnapshot{FormatVersion: CurrentSnapshotFormatVersion, Databases: databases}
}

func (s *Store) persistenceSnapshot() StoreSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	databases := make([]DatabaseSnapshot, 0, len(s.databases))
	for _, database := range s.databases {
		databases = append(databases, database.persistenceSnapshot())
	}
	sort.Slice(databases, func(i, j int) bool { return databases[i].Name < databases[j].Name })
	return StoreSnapshot{FormatVersion: CurrentSnapshotFormatVersion, Databases: databases}
}

// SharedSnapshot copies mutable collection headers while sharing immutable
// row payloads. It is intended for transaction handoff and persistence.
func (s *Store) SharedSnapshot() StoreSnapshot { return s.persistenceSnapshot() }

func NewStoreFromSnapshot(snapshot StoreSnapshot) (*Store, error) {
	return newStoreFromSnapshot(snapshot, false)
}

func newStoreFromSnapshot(snapshot StoreSnapshot, shareRows bool) (*Store, error) {
	store := NewStore()
	for _, databaseSnapshot := range snapshot.Databases {
		database, err := store.CreateDatabase(databaseSnapshot.Name)
		if err != nil {
			return nil, err
		}
		for _, tableSnapshot := range databaseSnapshot.Tables {
			table, err := database.CreateTable(tableSnapshot.Name, tableSnapshot.Columns)
			if err != nil {
				return nil, err
			}
			if shareRows {
				if err := table.restoreRows(tableSnapshot.Rows); err != nil {
					return nil, err
				}
			} else {
				for _, row := range tableSnapshot.Rows {
					if err := table.Insert(row); err != nil {
						return nil, err
					}
				}
			}
			if err := table.restoreAutoIncrementNext(tableSnapshot.AutoIncrementNext); err != nil {
				return nil, err
			}
			for _, indexSnapshot := range tableSnapshot.Indexes {
				var indexErr error
				if indexSnapshot.Primary || strings.EqualFold(indexSnapshot.Name, "PRIMARY") {
					indexErr = table.AddPrimaryKey(indexSnapshot.Columns)
				} else {
					indexErr = table.AddIndex(indexSnapshot.Name, indexSnapshot.Columns, indexSnapshot.Unique)
				}
				if indexErr != nil {
					return nil, indexErr
				}
			}
			checks := append([]CheckConstraint(nil), tableSnapshot.CheckConstraints...)
			for _, expression := range tableSnapshot.Checks {
				checks = append(checks, CheckConstraint{Expression: expression})
			}
			table.SetNamedConstraints(tableSnapshot.ForeignKeys, checks)
			table.restoreMetadata(tableSnapshot.Comment, tableSnapshot.CreatedAt, tableSnapshot.UpdatedAt)
		}
		for _, viewSnapshot := range databaseSnapshot.Views {
			if err := database.CreateView(viewSnapshot.Name, viewSnapshot.Definition, viewSnapshot.Columns, false); err != nil {
				return nil, err
			}
		}
	}
	return store, nil
}

func (s *Store) Replace(snapshot StoreSnapshot) error {
	replacement, err := NewStoreFromSnapshot(snapshot)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.databases = replacement.databases
	s.mu.Unlock()
	return nil
}

func (s *Store) ReplaceShared(snapshot StoreSnapshot) error {
	replacement, err := newStoreFromSnapshot(snapshot, true)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.databases = replacement.databases
	s.mu.Unlock()
	return nil
}

func (s *Store) Clone() (*Store, error) {
	return newStoreFromSnapshot(s.persistenceSnapshot(), true)
}

func validateIdentifier(name string) error {
	runes := []rune(name)
	if len(runes) == 0 || strings.TrimSpace(name) != name {
		return ErrInvalidIdentifier
	}
	if !(runes[0] == '_' || runes[0] == '-' || runes[0] == '$' || unicode.IsLetter(runes[0]) || unicode.IsDigit(runes[0])) {
		return ErrInvalidIdentifier
	}
	for _, r := range runes[1:] {
		if r != '_' && r != '-' && r != '$' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return ErrInvalidIdentifier
		}
	}
	return nil
}
