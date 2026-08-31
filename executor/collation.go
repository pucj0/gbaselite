package executor

import (
	"strings"

	"gbaselite/storage"
)

func (s *Session) IsBinaryCollation() bool {
	if s == nil {
		return false
	}
	s.InitializeSettings()
	return strings.HasSuffix(strings.ToLower(s.CollationConnection), "_bin") || strings.EqualFold(s.CollationConnection, "binary")
}

func isTextColumn(column storage.DataType) bool {
	return column == storage.TypeVarchar || column == storage.TypeText
}
