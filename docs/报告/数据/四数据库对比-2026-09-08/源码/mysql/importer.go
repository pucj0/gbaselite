package mysql

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"gbaselite/storage"

	_ "github.com/go-sql-driver/mysql"
)

type ImportOptions struct {
	Host                     string
	Port                     int
	User, Password, Database string
}

func Import(store *storage.Store, options ImportOptions) (int, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=false", options.User, options.Password, options.Host, options.Port, options.Database)
	source, err := sql.Open("mysql", dsn)
	if err != nil {
		return 0, err
	}
	defer source.Close()
	if err = source.Ping(); err != nil {
		return 0, err
	}
	database, err := store.Database(options.Database)
	if err != nil {
		database, err = store.CreateDatabase(options.Database)
		if err != nil {
			return 0, err
		}
	}
	tableRows, err := source.Query("SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME", options.Database)
	if err != nil {
		return 0, err
	}
	var tableNames []string
	for tableRows.Next() {
		var name string
		if err := tableRows.Scan(&name); err != nil {
			tableRows.Close()
			return 0, err
		}
		tableNames = append(tableNames, name)
	}
	tableRows.Close()
	imported := 0
	for _, tableName := range tableNames {
		columns, err := readColumns(source, options.Database, tableName)
		if err != nil {
			return imported, err
		}
		if existing, lookupErr := database.Table(tableName); lookupErr == nil {
			if existing.RowCount() > 0 {
				return imported, fmt.Errorf("target table %s already contains data", tableName)
			}
			_ = database.DropTable(tableName)
		}
		table, err := database.CreateTable(tableName, columns)
		if err != nil {
			return imported, err
		}
		query := "SELECT * FROM `" + strings.ReplaceAll(tableName, "`", "``") + "`"
		rows, err := source.Query(query)
		if err != nil {
			return imported, err
		}
		for rows.Next() {
			raw := make([]sql.RawBytes, len(columns))
			dest := make([]any, len(columns))
			for i := range raw {
				dest[i] = &raw[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return imported, err
			}
			row := make(storage.Row, len(columns))
			for i, column := range columns {
				value, err := rawToValue(column, raw[i])
				if err != nil {
					rows.Close()
					return imported, fmt.Errorf("%s.%s row conversion: %w", tableName, column.Name, err)
				}
				row[i] = value
			}
			if err := table.Insert(row); err != nil {
				rows.Close()
				return imported, err
			}
			imported++
		}
		if err := rows.Close(); err != nil {
			return imported, err
		}
	}
	return imported, nil
}

func readColumns(database *sql.DB, schema, table string) ([]storage.Column, error) {
	rows, err := database.Query("SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, COALESCE(CHARACTER_MAXIMUM_LENGTH,0) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION", schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []storage.Column
	for rows.Next() {
		var name, dataType, columnType string
		var length int64
		if err := rows.Scan(&name, &dataType, &columnType, &length); err != nil {
			return nil, err
		}
		mapped, err := storage.ParseDataType(dataType)
		if err != nil {
			return nil, fmt.Errorf("column %s has unsupported MySQL type %s: %w", name, columnType, err)
		}
		if mapped == storage.TypeVarchar && length <= 0 {
			length = 255
		}
		result = append(result, storage.Column{Name: name, Type: mapped, SQLType: strings.ToLower(columnType), Length: int(length)})
	}
	return result, rows.Err()
}
func rawToValue(column storage.Column, raw sql.RawBytes) (storage.Value, error) {
	if raw == nil {
		return storage.NullValue(column.Type), nil
	}
	text := string(raw)
	switch column.Type {
	case storage.TypeInt, storage.TypeBigInt:
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return storage.Value{}, err
		}
		return storage.NewValue(column.Type, value)
	case storage.TypeFloat, storage.TypeDouble:
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return storage.Value{}, err
		}
		return storage.NewValue(column.Type, value)
	case storage.TypeBoolean:
		return storage.NewValue(column.Type, text == "1" || strings.EqualFold(text, "true"))
	case storage.TypeDate:
		if len(text) >= 10 {
			text = text[:10]
		}
		return storage.NewValue(column.Type, text)
	case storage.TypeDateTime:
		return storage.NewValue(column.Type, text)
	default:
		return storage.NewValue(column.Type, text)
	}
}
