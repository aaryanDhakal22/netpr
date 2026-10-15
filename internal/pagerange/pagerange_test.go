package pagerange

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		spec  string
		total int
		canon string
		count int
	}{
		{"", 10, "", 10},
		{"  ", 10, "", 10},
		{"1-3,5", 10, "1-3,5", 4},
		{"5, 1-3", 10, "1-3,5", 4},
		{"1-3,2-4", 10, "1-4", 4},
		{"3", 10, "3", 1},
		{"8-", 10, "8-10", 3},
		{"1-10", 10, "", 10}, // whole document: no split
		{"1,2,3,4,5,6,7,8,9,10", 10, "", 10},
		{"2,4,6", 6, "2,4,6", 3},
		{"1-2,,3", 5, "1-3", 3},
	}
	for _, tt := range tests {
		canon, count, err := Parse(tt.spec, tt.total)
		if err != nil {
			t.Errorf("Parse(%q, %d): %v", tt.spec, tt.total, err)
			continue
		}
		if canon != tt.canon || count != tt.count {
			t.Errorf("Parse(%q, %d) = %q, %d; want %q, %d", tt.spec, tt.total, canon, count, tt.canon, tt.count)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, spec := range []string{"0", "11", "5-3", "a", "1-x", "-3", ",", "1;2"} {
		if _, _, err := Parse(spec, 10); err == nil {
			t.Errorf("Parse(%q) should fail", spec)
		}
	}
}

func TestSheets(t *testing.T) {
	tests := []struct {
		pages, copies int
		duplex        bool
		want          int
	}{
		{10, 1, false, 10},
		{10, 1, true, 5},
		{11, 1, true, 6},
		{11, 3, true, 18},
		{1, 2, true, 2},
	}
	for _, tt := range tests {
		if got := Sheets(tt.pages, tt.copies, tt.duplex); got != tt.want {
			t.Errorf("Sheets(%d, %d, %v) = %d, want %d", tt.pages, tt.copies, tt.duplex, got, tt.want)
		}
	}
}
