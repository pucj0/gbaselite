package server

import (
	"gbaselite/executor"
	"gbaselite/internal/mysqlcompat"
)

func initializeHandshakeCharacterSet(session *executor.Session, collationID byte) {
	session.InitializeSettings()
	if collation, ok := mysqlcompat.CollationByID(collationID); ok {
		_ = session.SetNames(collation.Charset, collation.Name)
	}
}
