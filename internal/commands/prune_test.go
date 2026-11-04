package commands

import (
	"testing"
	"time"
)

func TestParseRetention(t *testing.T) {
	day := 24 * time.Hour
	ok := []struct {
		in   string
		want time.Duration
	}{
		{"1d", day},
		{"90d", 90 * day},
		{"2w", 14 * day},
		{"6m", 180 * day},
		{"1y", 365 * day},
		{"2Y", 730 * day},
		{" 30d ", 30 * day},
		{"12W", 84 * day},
	}
	for _, tc := range ok {
		got, err := parseRetention(tc.in)
		if err != nil {
			t.Errorf("parseRetention(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseRetention(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	bad := []string{"", "d", "90", "0d", "-5d", "+5d", "1.5y", "5h", "5 d", "abc", "d90", "1000000d"}
	for _, in := range bad {
		if got, err := parseRetention(in); err == nil {
			t.Errorf("parseRetention(%q) = %v, want error", in, got)
		}
	}
}

func TestFmtBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0.0 MB"},
		{512 * 1024, "0.5 MB"},
		{443_025_408, "422.5 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{3 * 1024 * 1024 * 1024 / 2, "1.5 GB"},
	}
	for _, tc := range cases {
		if got := fmtBytes(tc.in); got != tc.want {
			t.Errorf("fmtBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
