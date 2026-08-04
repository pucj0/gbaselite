package catalog

import (
	"crypto/sha1"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gbaselite/internal/atomicfile"
)

type User struct {
	Username     string
	Host         string
	PasswordHash []byte
	Grants       []Grant
}

type Account struct {
	Username string
	Host     string
}

type Grant struct {
	Database    string
	Table       string
	Privileges  []string
	GrantOption bool
}

type Users struct {
	mu          sync.RWMutex
	path        string
	users       map[string]User
	replaceFile func(string, string) error
}

func OpenUsers(dataDir, defaultUsername, defaultPassword string) (*Users, error) {
	manager := &Users{
		path:        filepath.Join(dataDir, "users", "users.gob"),
		users:       map[string]User{},
		replaceFile: atomicfile.Replace,
	}
	_, statErr := os.Stat(manager.path)
	catalogExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if !catalogExists {
		if _, temporaryErr := os.Stat(manager.path + ".tmp"); temporaryErr == nil {
			return nil, fmt.Errorf("user catalog %s is missing but recovery candidate %s exists; do not initialize a new administrator or overwrite either file, copy the data directory and validate the candidate", manager.path, manager.path+".tmp")
		} else if !errors.Is(temporaryErr, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect user catalog recovery candidate %s: %w", manager.path+".tmp", temporaryErr)
		}
	}
	if err := manager.load(); err != nil {
		return nil, err
	}
	migrated := manager.migrateUsers()
	key := accountKey(defaultUsername, "%")
	user, ok := manager.users[key]
	if !catalogExists {
		if strings.TrimSpace(defaultUsername) == "" {
			return nil, errors.New("initial administrator username is required")
		}
		if defaultPassword == "" {
			return nil, errors.New("initial administrator password is required when users/users.gob does not exist")
		}
		user = User{Username: defaultUsername, Host: "%", PasswordHash: passwordHash(defaultPassword)}
		ok = true
	}
	addedAdminGrant := ok && !hasGlobalAll(user.Grants)
	if addedAdminGrant {
		user.Grants = append(user.Grants, Grant{Database: "*", Table: "*", Privileges: []string{"ALL"}, GrantOption: true})
	}
	if ok {
		manager.users[key] = user
	}
	if migrated || !catalogExists || addedAdminGrant {
		if err := manager.saveLocked(); err != nil {
			return nil, err
		}
	}
	return manager, nil
}

func (u *Users) Create(username, password string) error {
	_, err := u.CreateAccount(username, "%", password, false)
	return err
}

func (u *Users) CreateAccount(username, host, password string, ifNotExists bool) (bool, error) {
	if strings.TrimSpace(username) == "" {
		return false, errors.New("username cannot be empty")
	}
	host = normalizeHost(host)
	u.mu.Lock()
	defer u.mu.Unlock()
	key := accountKey(username, host)
	if _, exists := u.users[key]; exists {
		if ifNotExists {
			return false, nil
		}
		return false, fmt.Errorf("user %q@%q already exists", username, host)
	}
	u.users[key] = User{Username: username, Host: host, PasswordHash: passwordHash(password)}
	return true, u.saveLocked()
}

func (u *Users) AlterPassword(username, password string) error {
	return u.AlterAccountPassword(username, "%", password, false)
}

func (u *Users) AlterAccountPassword(username, host, password string, ifExists bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	key := accountKey(username, host)
	user, exists := u.users[key]
	if !exists {
		if ifExists {
			return nil
		}
		return fmt.Errorf("user %q@%q not found", username, normalizeHost(host))
	}
	user.PasswordHash = passwordHash(password)
	u.users[key] = user
	return u.saveLocked()
}

func (u *Users) VerifyNativePassword(username string, seed, response []byte) bool {
	_, ok := u.AuthenticateNativePassword(username, "%", seed, response)
	return ok
}

func (u *Users) AuthenticateNativePassword(username, remoteHost string, seed, response []byte) (Account, bool) {
	u.mu.RLock()
	user, ok := u.resolveLocked(username, remoteHost)
	u.mu.RUnlock()
	if !ok {
		return Account{}, false
	}
	if len(response) == 0 {
		return Account{Username: user.Username, Host: user.Host}, subtle.ConstantTimeCompare(user.PasswordHash, passwordHash("")) == 1
	}
	stage2 := sha1.Sum(user.PasswordHash)
	hash := sha1.New()
	hash.Write(seed)
	hash.Write(stage2[:])
	scramble := hash.Sum(nil)
	if len(scramble) != len(response) || len(user.PasswordHash) != len(response) {
		return Account{}, false
	}
	candidate := make([]byte, len(response))
	for i := range response {
		candidate[i] = response[i] ^ scramble[i]
	}
	return Account{Username: user.Username, Host: user.Host}, subtle.ConstantTimeCompare(candidate, user.PasswordHash) == 1
}

func (u *Users) VerifyPassword(username, password string) bool {
	u.mu.RLock()
	user, ok := u.resolveLocked(username, "%")
	u.mu.RUnlock()
	return ok && subtle.ConstantTimeCompare(user.PasswordHash, passwordHash(password)) == 1
}

func (u *Users) List() []string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	names := make([]string, 0, len(u.users))
	for _, user := range u.users {
		names = append(names, user.Username+"@"+user.Host)
	}
	sort.Strings(names)
	return names
}

func (u *Users) load() error {
	file, err := os.Open(u.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("read user catalog %s: %w", u.path, err)
	}
	if users, _, err := decodeUserCatalog(data); err != nil {
		recoveryHint := ""
		if _, temporaryErr := os.Stat(u.path + ".tmp"); temporaryErr == nil {
			recoveryHint = fmt.Sprintf("; recovery candidate %s also exists", u.path+".tmp")
		}
		return fmt.Errorf("decode user catalog %s: %w%s; do not replace the file in place, copy the data directory and restore a known-good backup or validate the recovery candidate", u.path, err, recoveryHint)
	} else {
		u.users = users
	}
	return nil
}

func (u *Users) migrateUsers() bool {
	migrated := false
	rebuilt := make(map[string]User, len(u.users))
	for _, user := range u.users {
		if user.Host == "" {
			user.Host = "%"
			migrated = true
		}
		key := accountKey(user.Username, user.Host)
		rebuilt[key] = user
	}
	if len(rebuilt) != len(u.users) {
		migrated = true
	}
	for key := range u.users {
		if _, ok := rebuilt[key]; !ok {
			migrated = true
			break
		}
	}
	u.users = rebuilt
	return migrated
}
func (u *Users) saveLocked() (saveErr error) {
	previousUsers, previousExists, err := readUserCatalog(u.path)
	if err != nil {
		return fmt.Errorf("read previous user catalog before save: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if previousExists {
			u.users = previousUsers
		} else {
			u.users = map[string]User{}
		}
	}()
	if err := os.MkdirAll(filepath.Dir(u.path), 0o755); err != nil {
		return err
	}
	temporary := u.path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	encodeErr := encodeUserCatalog(file, u.users)
	syncErr := file.Sync()
	closeErr := file.Close()
	if encodeErr != nil {
		return fmt.Errorf("encode user catalog: %w", encodeErr)
	}
	if syncErr != nil {
		return fmt.Errorf("sync user catalog: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close user catalog: %w", closeErr)
	}
	if err := u.replaceFile(temporary, u.path); err != nil {
		return fmt.Errorf("replace user catalog without deleting the previous catalog: %w", err)
	}
	committed = true
	return nil
}

func readUserCatalog(path string) (map[string]User, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, true, err
	}
	users, _, err := decodeUserCatalog(data)
	if err != nil {
		return nil, true, err
	}
	return users, true, nil
}
func passwordHash(password string) []byte {
	sum := sha1.Sum([]byte(password))
	return append([]byte(nil), sum[:]...)
}
func normalize(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "%"
	}
	return host
}
func accountKey(username, host string) string { return normalize(username) + "@" + normalizeHost(host) }
