package mysqlcompat

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type CharacterSet struct {
	Name             string
	Description      string
	DefaultCollation string
	MaxLength        int64
}

type Collation struct {
	Name      string
	Charset   string
	ID        int64
	IsDefault bool
}

var characterSets = []CharacterSet{
	{Name: "ascii", Description: "US ASCII", DefaultCollation: "ascii_general_ci", MaxLength: 1},
	{Name: "binary", Description: "Binary pseudo charset", DefaultCollation: "binary", MaxLength: 1},
	{Name: "latin1", Description: "cp1252 West European", DefaultCollation: "latin1_swedish_ci", MaxLength: 1},
	{Name: "utf8", Description: "UTF-8 Unicode", DefaultCollation: "utf8_general_ci", MaxLength: 3},
	{Name: "utf8mb4", Description: "UTF-8 Unicode", DefaultCollation: "utf8mb4_general_ci", MaxLength: 4},
}

var collations = []Collation{
	{Name: "latin1_swedish_ci", Charset: "latin1", ID: 8, IsDefault: true},
	{Name: "ascii_general_ci", Charset: "ascii", ID: 11, IsDefault: true},
	{Name: "utf8_general_ci", Charset: "utf8", ID: 33, IsDefault: true},
	{Name: "utf8mb4_general_ci", Charset: "utf8mb4", ID: 45, IsDefault: true},
	{Name: "utf8mb4_bin", Charset: "utf8mb4", ID: 46},
	{Name: "latin1_bin", Charset: "latin1", ID: 47},
	{Name: "latin1_general_ci", Charset: "latin1", ID: 48},
	{Name: "binary", Charset: "binary", ID: 63, IsDefault: true},
	{Name: "ascii_bin", Charset: "ascii", ID: 65},
	{Name: "utf8_bin", Charset: "utf8", ID: 83},
	{Name: "utf8_unicode_ci", Charset: "utf8", ID: 192},
	{Name: "utf8mb4_unicode_ci", Charset: "utf8mb4", ID: 224},
	{Name: "utf8mb4_0900_ai_ci", Charset: "utf8mb4", ID: 255},
}

func CharacterSets() []CharacterSet {
	return append([]CharacterSet(nil), characterSets...)
}

func Collations() []Collation {
	return append([]Collation(nil), collations...)
}

func ResolveCharset(name string) (CharacterSet, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "utf8mb3" {
		name = "utf8"
	}
	for _, charset := range characterSets {
		if charset.Name == name {
			return charset, nil
		}
	}
	return CharacterSet{}, fmt.Errorf("unknown character set %q", name)
}

func ResolveCollation(name string) (Collation, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(name, "utf8mb3_") {
		name = "utf8_" + strings.TrimPrefix(name, "utf8mb3_")
	}
	for _, collation := range collations {
		if collation.Name == name {
			return collation, nil
		}
	}
	return Collation{}, fmt.Errorf("unknown collation %q", name)
}

func CollationByID(id byte) (Collation, bool) {
	for _, collation := range collations {
		if collation.ID == int64(id) {
			return collation, true
		}
	}
	return Collation{}, false
}

func CompareStrings(left, right, collation string) int {
	if !strings.HasSuffix(strings.ToLower(collation), "_bin") && !strings.EqualFold(collation, "binary") {
		left = strings.ToLower(left)
		right = strings.ToLower(right)
	}
	return strings.Compare(left, right)
}

func ParseTimeZone(value string) (string, *time.Location, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil, fmt.Errorf("time zone cannot be empty")
	}
	if strings.EqualFold(value, "SYSTEM") {
		return "SYSTEM", time.Local, nil
	}
	if strings.EqualFold(value, "UTC") || value == "Z" {
		return "+00:00", time.UTC, nil
	}
	if len(value) == 6 && (value[0] == '+' || value[0] == '-') && value[3] == ':' {
		hours, hourErr := strconv.Atoi(value[1:3])
		minutes, minuteErr := strconv.Atoi(value[4:6])
		if hourErr != nil || minuteErr != nil || minutes > 59 || hours > 14 || hours == 14 && (minutes != 0 || value[0] == '-') {
			return "", nil, fmt.Errorf("invalid time zone offset %q", value)
		}
		offset := (hours*60 + minutes) * 60
		if value[0] == '-' {
			offset = -offset
		}
		return value, time.FixedZone(value, offset), nil
	}
	location, err := time.LoadLocation(value)
	if err != nil {
		return "", nil, fmt.Errorf("unknown or unavailable time zone %q", value)
	}
	return value, location, nil
}
