package catalog

import (
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// UserCatalogInspection contains aggregate metadata only. It never exposes
// account names, hosts, password hashes, or grant scopes.
type UserCatalogInspection struct {
	Path                string
	Size                int64
	ModifiedAt          time.Time
	SHA256              string
	SourceFormatVersion uint16
	FormatVersion       uint16
	Accounts            int
	Grants              int
	Privileges          int
}

// InspectUserCatalog decodes and validates a user catalog without modifying it.
func InspectUserCatalog(path string) (UserCatalogInspection, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return UserCatalogInspection{}, fmt.Errorf("resolve user catalog path: %w", err)
	}
	file, err := os.Open(absPath)
	if err != nil {
		return UserCatalogInspection{}, fmt.Errorf("open user catalog %s: %w", absPath, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return UserCatalogInspection{}, fmt.Errorf("stat user catalog %s: %w", absPath, err)
	}
	if !info.Mode().IsRegular() {
		return UserCatalogInspection{}, fmt.Errorf("user catalog %s is not a regular file", absPath)
	}

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return UserCatalogInspection{}, fmt.Errorf("hash user catalog %s: %w", absPath, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return UserCatalogInspection{}, fmt.Errorf("rewind user catalog %s: %w", absPath, err)
	}

	data, err := io.ReadAll(file)
	if err != nil {
		return UserCatalogInspection{}, fmt.Errorf("read user catalog %s: %w", absPath, err)
	}
	users, sourceFormatVersion, err := decodeUserCatalog(data)
	if err != nil {
		return UserCatalogInspection{}, fmt.Errorf("decode user catalog %s: %w", absPath, err)
	}
	inspection := UserCatalogInspection{
		Path:                absPath,
		Size:                info.Size(),
		ModifiedAt:          info.ModTime().UTC(),
		SHA256:              fmt.Sprintf("%x", hash.Sum(nil)),
		SourceFormatVersion: sourceFormatVersion,
		FormatVersion:       CurrentUserCatalogFormatVersion,
		Accounts:            len(users),
	}
	for _, user := range users {
		if len(user.PasswordHash) != sha1.Size {
			return UserCatalogInspection{}, fmt.Errorf("validate user catalog %s: account has invalid password hash length", absPath)
		}
		inspection.Grants += len(user.Grants)
		for _, grant := range user.Grants {
			for _, privilege := range grant.Privileges {
				if _, err := NormalizePrivilege(privilege); err != nil {
					return UserCatalogInspection{}, fmt.Errorf("validate user catalog %s: account has an unsupported privilege", absPath)
				}
				inspection.Privileges++
			}
		}
	}
	return inspection, nil
}
