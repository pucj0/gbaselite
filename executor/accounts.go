package executor

import (
	"fmt"
	"gbaselite/parser"
	"gbaselite/storage"
)

// Account metadata has its own durable catalog and is not part of SQL transactions.
func (e *Engine) executeAccountStatement(session *Session, statement parser.Statement) (*Result, error) {
	var result *Result
	var err error
	switch value := statement.(type) {
	case parser.CreateUser:
		var created uint64
		for _, user := range value.Users {
			var changed bool
			changed, err = e.Users.CreateAccount(user.Account.Username, user.Account.Host, user.Password, value.IfNotExists)
			if err != nil {
				break
			}
			if changed {
				created++
			}
		}
		result = &Result{AffectedRows: created, Message: "users created"}
	case parser.AlterUser:
		for _, user := range value.Users {
			err = e.Users.AlterAccountPassword(user.Account.Username, user.Account.Host, user.Password, value.IfExists)
			if err != nil {
				break
			}
		}
		result = &Result{AffectedRows: uint64(len(value.Users)), Message: "users altered"}
	case parser.DropUser:
		var dropped uint64
		for _, account := range value.Accounts {
			var changed bool
			changed, err = e.Users.DropAccount(account.Username, account.Host, value.IfExists)
			if err != nil {
				break
			}
			if changed {
				dropped++
			}
		}
		result = &Result{AffectedRows: dropped, Message: "users dropped"}
	case parser.RenameUser:
		for _, pair := range value.Pairs {
			err = e.Users.RenameAccount(pair.From.Username, pair.From.Host, pair.To.Username, pair.To.Host)
			if err != nil {
				break
			}
		}
		result = &Result{AffectedRows: uint64(len(value.Pairs)), Message: "users renamed"}
	case parser.SetPassword:
		account := value.Account
		if account.Username == "" {
			account = parser.Account{Username: session.Username, Host: session.Host}
		}
		err = e.Users.AlterAccountPassword(account.Username, account.Host, value.Password, false)
		result = &Result{AffectedRows: 1, Message: "password changed"}
	case parser.Grant:
		for _, account := range value.Accounts {
			err = e.Users.GrantPrivileges(account.Username, account.Host, value.Privileges, value.Database, value.Table, value.GrantOption)
			if err != nil {
				break
			}
		}
		result = &Result{Message: "privileges granted"}
	case parser.Revoke:
		for _, account := range value.Accounts {
			err = e.Users.RevokePrivileges(account.Username, account.Host, value.Privileges, value.Database, value.Table, value.GrantOptionOnly)
			if err != nil {
				break
			}
		}
		result = &Result{Message: "privileges revoked"}
	case parser.ShowGrants:
		account := value.Account
		if !value.ForAccount {
			account = parser.Account{Username: session.Username, Host: session.Host}
		}
		var grants []string
		grants, err = e.Users.ShowGrants(account.Username, account.Host)
		result = &Result{Columns: []Column{{Name: "Grants for " + account.Username + "@" + account.Host, Type: storage.TypeText}}}
		for _, grant := range grants {
			result.Rows = append(result.Rows, []any{grant})
		}
	case parser.ShowCreateUser:
		var definition string
		definition, err = e.Users.CreateUserSQL(value.Account.Username, value.Account.Host)
		result = &Result{Columns: []Column{{Name: "User", Type: storage.TypeVarchar}, {Name: "Create User", Type: storage.TypeText}}, Rows: [][]any{{value.Account.Username + "@" + value.Account.Host, definition}}}
	default:
		return nil, fmt.Errorf("unsupported account statement")
	}
	return result, err
}
