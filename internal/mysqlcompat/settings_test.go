package mysqlcompat

import "testing"

func TestParseTimeZoneUsesMySQLOffsetRange(t *testing.T) {
	for _, value := range []string{"SYSTEM", "UTC", "+00:00", "+14:00", "-13:59", "Asia/Shanghai"} {
		if _, _, err := ParseTimeZone(value); err != nil {
			t.Fatalf("%s: %v", value, err)
		}
	}
	for _, value := range []string{"", "+14:01", "-14:00", "+25:00", "+08:60"} {
		if _, _, err := ParseTimeZone(value); err == nil {
			t.Fatalf("invalid time zone %q was accepted", value)
		}
	}
}
