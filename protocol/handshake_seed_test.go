package protocol

import (
	"testing"
)

func TestNativePasswordSeedIsASCII(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		seed, err := NewSeed()
		if err != nil || len(seed) != 20 {
			t.Fatal(err)
		}
		for _, b := range seed {
			if b < 33 || b > 126 {
				t.Fatal("challenge is not printable ASCII")
			}
		}
		if seen[string(seed)] {
			t.Fatal("repeated challenge")
		}
		seen[string(seed)] = true
	}
}
