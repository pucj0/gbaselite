package executor

import (
	"fmt"
	"strings"
	"time"

	"gbaselite/internal/mysqlcompat"
)

const (
	DefaultCharacterSet = "utf8mb4"
	DefaultCollation    = "utf8mb4_general_ci"
	DefaultTimeZone     = "SYSTEM"
)

func (s *Session) InitializeSettings() {
	if s == nil {
		return
	}
	if s.CharacterSetClient == "" {
		s.CharacterSetClient = DefaultCharacterSet
	}
	if s.CharacterSetConnection == "" {
		s.CharacterSetConnection = DefaultCharacterSet
	}
	if s.CharacterSetResults == "" {
		s.CharacterSetResults = DefaultCharacterSet
	}
	if s.CollationConnection == "" {
		s.CollationConnection = DefaultCollation
	}
	if s.TimeZone == "" {
		s.TimeZone = DefaultTimeZone
	}
	if s.ServerTimeZone == "" {
		s.ServerTimeZone = DefaultTimeZone
	}
	if s.UserVariables == nil {
		s.UserVariables = make(map[string]string)
	}
}

func (s *Session) SetNames(charsetName, collationName string) error {
	charset, err := mysqlcompat.ResolveCharset(charsetName)
	if err != nil {
		return err
	}
	if collationName == "" {
		collationName = charset.DefaultCollation
	}
	collation, err := mysqlcompat.ResolveCollation(collationName)
	if err != nil {
		return err
	}
	if collation.Charset != charset.Name {
		return fmt.Errorf("collation %q is not valid for character set %q", collation.Name, charset.Name)
	}
	s.CharacterSetClient = charset.Name
	s.CharacterSetConnection = charset.Name
	s.CharacterSetResults = charset.Name
	s.CollationConnection = collation.Name
	return nil
}

func (s *Session) SetCharacterSet(variable, value string) error {
	s.InitializeSettings()
	variable = strings.ToLower(strings.TrimSpace(variable))
	if variable == "collation_connection" {
		collation, err := mysqlcompat.ResolveCollation(value)
		if err != nil {
			return err
		}
		s.CollationConnection = collation.Name
		s.CharacterSetConnection = collation.Charset
		return nil
	}
	charset, err := mysqlcompat.ResolveCharset(value)
	if err != nil {
		return err
	}
	switch variable {
	case "character_set_client":
		s.CharacterSetClient = charset.Name
	case "character_set_connection":
		s.CharacterSetConnection = charset.Name
		s.CollationConnection = charset.DefaultCollation
	case "character_set_results":
		s.CharacterSetResults = charset.Name
	default:
		return fmt.Errorf("unsupported character set variable %q", variable)
	}
	return nil
}

func (s *Session) SetTimeZone(value string) error {
	name, location, err := mysqlcompat.ParseTimeZone(value)
	if err != nil {
		return err
	}
	s.TimeZone = name
	s.timeLocation = location
	return nil
}

func (s *Session) Location() *time.Location {
	if s == nil {
		return time.Local
	}
	s.InitializeSettings()
	if s.timeLocation == nil {
		_, location, err := mysqlcompat.ParseTimeZone(s.TimeZone)
		if err == nil {
			s.timeLocation = location
		}
	}
	if s.timeLocation == nil {
		return time.Local
	}
	return s.timeLocation
}

func (s *Session) Compare(left, right any) int {
	if leftText, ok := left.(string); ok {
		if rightText, rightOK := right.(string); rightOK {
			s.InitializeSettings()
			return mysqlcompat.CompareStrings(leftText, rightText, s.CollationConnection)
		}
	}
	return compareAny(left, right)
}

func (s *Session) Now() time.Time {
	return time.Now().In(s.Location())
}
