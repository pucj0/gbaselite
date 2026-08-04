package catalog

import (
	"fmt"
	"sort"
	"strings"
)

var supportedPrivileges = map[string]bool{
	"ALL": true, "ALTER": true, "ALTER ROUTINE": true, "CREATE": true,
	"CREATE ROUTINE": true, "CREATE TEMPORARY TABLES": true, "CREATE USER": true,
	"CREATE VIEW": true, "DELETE": true, "DROP": true, "EVENT": true,
	"EXECUTE": true, "FILE": true, "INDEX": true, "INSERT": true,
	"LOCK TABLES": true, "PROCESS": true, "REFERENCES": true, "RELOAD": true,
	"REPLICATION CLIENT": true, "REPLICATION SLAVE": true, "SELECT": true,
	"SHOW DATABASES": true, "SHOW VIEW": true, "SHUTDOWN": true,
	"TRIGGER": true, "UPDATE": true, "USAGE": true,
}

func NormalizePrivilege(privilege string) (string, error) {
	privilege = strings.ToUpper(strings.Join(strings.Fields(privilege), " "))
	if privilege == "ALL PRIVILEGES" {
		privilege = "ALL"
	}
	if !supportedPrivileges[privilege] {
		return "", fmt.Errorf("unsupported privilege %q", privilege)
	}
	return privilege, nil
}

func (u *Users) DropAccount(username, host string, ifExists bool) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	key := accountKey(username, host)
	if _, exists := u.users[key]; !exists {
		if ifExists {
			return false, nil
		}
		return false, fmt.Errorf("user %q@%q not found", username, normalizeHost(host))
	}
	delete(u.users, key)
	return true, u.saveLocked()
}

func (u *Users) RenameAccount(oldUsername, oldHost, newUsername, newHost string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	oldKey := accountKey(oldUsername, oldHost)
	user, exists := u.users[oldKey]
	if !exists {
		return fmt.Errorf("user %q@%q not found", oldUsername, normalizeHost(oldHost))
	}
	newKey := accountKey(newUsername, newHost)
	if _, exists := u.users[newKey]; exists {
		return fmt.Errorf("user %q@%q already exists", newUsername, normalizeHost(newHost))
	}
	delete(u.users, oldKey)
	user.Username = newUsername
	user.Host = normalizeHost(newHost)
	u.users[newKey] = user
	return u.saveLocked()
}

func (u *Users) Account(username, host string) (User, bool) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	user, ok := u.users[accountKey(username, host)]
	if !ok {
		return User{}, false
	}
	return cloneUser(user), true
}

func (u *Users) Accounts() []Account {
	u.mu.RLock()
	defer u.mu.RUnlock()
	accounts := make([]Account, 0, len(u.users))
	for _, user := range u.users {
		accounts = append(accounts, Account{Username: user.Username, Host: user.Host})
	}
	sort.Slice(accounts, func(i, j int) bool {
		if strings.EqualFold(accounts[i].Username, accounts[j].Username) {
			return strings.ToLower(accounts[i].Host) < strings.ToLower(accounts[j].Host)
		}
		return strings.ToLower(accounts[i].Username) < strings.ToLower(accounts[j].Username)
	})
	return accounts
}

func (u *Users) GrantsFor(username, host string) ([]Grant, bool) {
	user, ok := u.Account(username, host)
	if !ok {
		return nil, false
	}
	return user.Grants, true
}

func (u *Users) GrantPrivileges(username, host string, privileges []string, database, table string, grantOption bool) error {
	normalized, err := normalizePrivileges(privileges)
	if err != nil {
		return err
	}
	database, table = normalizeScope(database, table)
	u.mu.Lock()
	defer u.mu.Unlock()
	key := accountKey(username, host)
	user, exists := u.users[key]
	if !exists {
		return fmt.Errorf("user %q@%q not found", username, normalizeHost(host))
	}
	position := grantPosition(user.Grants, database, table)
	if position < 0 {
		user.Grants = append(user.Grants, Grant{Database: database, Table: table})
		position = len(user.Grants) - 1
	}
	grant := user.Grants[position]
	set := make(map[string]bool, len(grant.Privileges)+len(normalized))
	for _, privilege := range grant.Privileges {
		set[privilege] = true
	}
	for _, privilege := range normalized {
		set[privilege] = true
	}
	grant.Privileges = sortedPrivilegeSet(set)
	grant.GrantOption = grant.GrantOption || grantOption
	user.Grants[position] = grant
	u.users[key] = user
	return u.saveLocked()
}

func (u *Users) RevokePrivileges(username, host string, privileges []string, database, table string, grantOptionOnly bool) error {
	normalized, err := normalizePrivileges(privileges)
	if err != nil {
		return err
	}
	database, table = normalizeScope(database, table)
	u.mu.Lock()
	defer u.mu.Unlock()
	key := accountKey(username, host)
	user, exists := u.users[key]
	if !exists {
		return fmt.Errorf("user %q@%q not found", username, normalizeHost(host))
	}
	position := grantPosition(user.Grants, database, table)
	if position < 0 {
		return fmt.Errorf("there is no such grant defined for user %q@%q", username, normalizeHost(host))
	}
	grant := user.Grants[position]
	if grantOptionOnly {
		grant.GrantOption = false
	} else {
		remove := make(map[string]bool, len(normalized))
		for _, privilege := range normalized {
			remove[privilege] = true
		}
		if remove["ALL"] {
			grant.Privileges = nil
		} else {
			kept := grant.Privileges[:0]
			for _, privilege := range grant.Privileges {
				if !remove[privilege] {
					kept = append(kept, privilege)
				}
			}
			grant.Privileges = kept
		}
	}
	if len(grant.Privileges) == 0 && !grant.GrantOption {
		user.Grants = append(user.Grants[:position], user.Grants[position+1:]...)
	} else {
		user.Grants[position] = grant
	}
	u.users[key] = user
	return u.saveLocked()
}

func (u *Users) Allowed(username, host, privilege, database, table string) bool {
	if strings.TrimSpace(username) == "" {
		return true
	}
	privilege, err := NormalizePrivilege(privilege)
	if err != nil {
		return false
	}
	database, table = normalizeScope(database, table)
	u.mu.RLock()
	defer u.mu.RUnlock()
	user, exists := u.users[accountKey(username, host)]
	if !exists {
		return false
	}
	return grantsAllow(user.Grants, privilege, database, table, false)
}

func (u *Users) CanGrant(username, host, privilege, database, table string) bool {
	if strings.TrimSpace(username) == "" {
		return true
	}
	privilege, err := NormalizePrivilege(privilege)
	if err != nil {
		return false
	}
	database, table = normalizeScope(database, table)
	u.mu.RLock()
	defer u.mu.RUnlock()
	user, exists := u.users[accountKey(username, host)]
	if !exists {
		return false
	}
	return grantsAllow(user.Grants, privilege, database, table, true)
}

func (u *Users) HasDatabaseAccess(username, host, database string) bool {
	if strings.TrimSpace(username) == "" {
		return true
	}
	u.mu.RLock()
	defer u.mu.RUnlock()
	user, exists := u.users[accountKey(username, host)]
	if !exists {
		return false
	}
	for _, grant := range user.Grants {
		if scopeMatches(grant.Database, database) && hasEffectivePrivilege(grant.Privileges) {
			return true
		}
	}
	return false
}

func (u *Users) HasObjectAccess(username, host, database, table string) bool {
	if strings.TrimSpace(username) == "" {
		return true
	}
	u.mu.RLock()
	defer u.mu.RUnlock()
	user, exists := u.users[accountKey(username, host)]
	if !exists {
		return false
	}
	for _, grant := range user.Grants {
		if scopeMatches(grant.Database, database) && scopeMatches(grant.Table, table) && hasEffectivePrivilege(grant.Privileges) {
			return true
		}
	}
	return false
}

func hasEffectivePrivilege(privileges []string) bool {
	for _, privilege := range privileges {
		if privilege != "USAGE" {
			return true
		}
	}
	return false
}

func (u *Users) ShowGrants(username, host string) ([]string, error) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	user, exists := u.users[accountKey(username, host)]
	if !exists {
		return nil, fmt.Errorf("user %q@%q not found", username, normalizeHost(host))
	}
	account := quoteAccount(user.Username, user.Host)
	rows := []string{"GRANT USAGE ON *.* TO " + account}
	grants := append([]Grant(nil), user.Grants...)
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].Database == grants[j].Database {
			return grants[i].Table < grants[j].Table
		}
		return grants[i].Database < grants[j].Database
	})
	for _, grant := range grants {
		if len(grant.Privileges) == 0 {
			continue
		}
		line := "GRANT " + strings.Join(grant.Privileges, ", ") + " ON " + quoteScope(grant.Database, grant.Table) + " TO " + account
		if grant.GrantOption {
			line += " WITH GRANT OPTION"
		}
		rows = append(rows, line)
	}
	return rows, nil
}

func (u *Users) CreateUserSQL(username, host string) (string, error) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	user, exists := u.users[accountKey(username, host)]
	if !exists {
		return "", fmt.Errorf("user %q@%q not found", username, normalizeHost(host))
	}
	return "CREATE USER " + quoteAccount(user.Username, user.Host) + " IDENTIFIED WITH 'mysql_native_password'", nil
}

func (u *Users) resolveLocked(username, remoteHost string) (User, bool) {
	remoteHost = normalizeHost(remoteHost)
	bestScore := -1
	var best User
	for _, user := range u.users {
		if !strings.EqualFold(user.Username, username) || !hostMatches(user.Host, remoteHost) {
			continue
		}
		score := hostSpecificity(user.Host, remoteHost)
		if score > bestScore {
			best, bestScore = user, score
		}
	}
	return best, bestScore >= 0
}

func grantsAllow(grants []Grant, privilege, database, table string, requireGrantOption bool) bool {
	for _, grant := range grants {
		if requireGrantOption && !grant.GrantOption {
			continue
		}
		if !scopeMatches(grant.Database, database) || !scopeMatches(grant.Table, table) {
			continue
		}
		for _, granted := range grant.Privileges {
			if granted == "ALL" || granted == privilege {
				return true
			}
		}
	}
	return false
}

func normalizePrivileges(privileges []string) ([]string, error) {
	set := make(map[string]bool, len(privileges))
	for _, privilege := range privileges {
		normalized, err := NormalizePrivilege(privilege)
		if err != nil {
			return nil, err
		}
		set[normalized] = true
	}
	return sortedPrivilegeSet(set), nil
}

func sortedPrivilegeSet(set map[string]bool) []string {
	privileges := make([]string, 0, len(set))
	for privilege := range set {
		privileges = append(privileges, privilege)
	}
	sort.Strings(privileges)
	return privileges
}

func normalizeScope(database, table string) (string, string) {
	database = strings.Trim(strings.TrimSpace(database), "`")
	table = strings.Trim(strings.TrimSpace(table), "`")
	if database == "" {
		database = "*"
	}
	if table == "" {
		table = "*"
	}
	return database, table
}

func grantPosition(grants []Grant, database, table string) int {
	for index, grant := range grants {
		if strings.EqualFold(grant.Database, database) && strings.EqualFold(grant.Table, table) {
			return index
		}
	}
	return -1
}

func hasGlobalAll(grants []Grant) bool {
	return grantsAllow(grants, "ALL", "*", "*", true)
}

func scopeMatches(granted, requested string) bool {
	return granted == "*" || strings.EqualFold(granted, requested)
}

func hostMatches(pattern, host string) bool {
	pattern, host = strings.ToLower(pattern), strings.ToLower(host)
	if pattern == "localhost" && (host == "127.0.0.1" || host == "::1") {
		return true
	}
	input, wildcard := []rune(host), []rune(pattern)
	i, p, star, retry := 0, 0, -1, 0
	for i < len(input) {
		switch {
		case p < len(wildcard) && (wildcard[p] == '_' || wildcard[p] == input[i]):
			i++
			p++
		case p < len(wildcard) && wildcard[p] == '%':
			star, p, retry = p, p+1, i
		case star >= 0:
			p = star + 1
			retry++
			i = retry
		default:
			return false
		}
	}
	for p < len(wildcard) && wildcard[p] == '%' {
		p++
	}
	return p == len(wildcard)
}

func hostSpecificity(pattern, host string) int {
	if strings.EqualFold(pattern, host) {
		return 10000
	}
	if strings.EqualFold(pattern, "localhost") && (host == "127.0.0.1" || host == "::1") {
		return 9000
	}
	return len(strings.ReplaceAll(strings.ReplaceAll(pattern, "%", ""), "_", ""))
}

func quoteAccount(username, host string) string {
	return "'" + strings.ReplaceAll(username, "'", "''") + "'@'" + strings.ReplaceAll(host, "'", "''") + "'"
}

func quoteScope(database, table string) string {
	quote := func(value string) string {
		if value == "*" {
			return "*"
		}
		return "`" + strings.ReplaceAll(value, "`", "``") + "`"
	}
	return quote(database) + "." + quote(table)
}

func cloneUser(user User) User {
	user.PasswordHash = append([]byte(nil), user.PasswordHash...)
	user.Grants = append([]Grant(nil), user.Grants...)
	for index := range user.Grants {
		user.Grants[index].Privileges = append([]string(nil), user.Grants[index].Privileges...)
	}
	return user
}
