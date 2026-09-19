package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/Pilan-AI/mnemo/internal/db"
)

func TestShorten(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{name: "short string", input: "hello", maxLen: 10, want: "hello"},
		{name: "exact length", input: "hello", maxLen: 5, want: "hello"},
		{name: "truncated", input: "hello world foo", maxLen: 10, want: "hello w..."},
		{name: "empty", input: "", maxLen: 10, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shorten(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("shorten(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestFormatRelativeShort(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name  string
		input time.Time
		want  string
	}{
		{name: "zero time", input: time.Time{}, want: "?"},
		{name: "minutes", input: now.Add(-20 * time.Minute), want: "20m"},
		{name: "hours", input: now.Add(-5 * time.Hour), want: "5h"},
		{name: "days", input: now.Add(-3 * 24 * time.Hour), want: "3d"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatRelativeShort(tt.input)
			if got != tt.want {
				t.Errorf("formatRelativeShort() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatRelativeShortFarPast(t *testing.T) {
	old := time.Now().Add(-60 * 24 * time.Hour)
	got := formatRelativeShort(old)
	if got == "?" || got == "" {
		t.Error("formatRelativeShort for 60 days ago should not be empty/unknown")
	}
}

func TestFormatSessionLineNamesTheHost(t *testing.T) {
	r := db.SessionMatch{
		Project:    "lora-bootloader",
		FirstQuery: "why does the second stage hang",
		Tool:       "claude",
		Host:       "tmmnote15",
		StartTime:  time.Now().Add(-3 * 24 * time.Hour),
		MatchCount: 4,
	}

	got := formatSessionLine(1, r)
	want := "1. [tmmnote15:lora-bootloader/3d/4hits] \"why does the second stage hang\" — claude\n"
	if got != want {
		t.Errorf("formatSessionLine() = %q, want %q", got, want)
	}
}

// A row indexed before the host column existed records no machine. Saying so
// by leaving the field out beats naming the machine that ran the query, which
// is wrong whenever the answer matters.
func TestFormatSessionLineOmitsAnUnknownHost(t *testing.T) {
	r := db.SessionMatch{
		Project:    "lora-bootloader",
		FirstQuery: "why does the second stage hang",
		Tool:       "claude",
		StartTime:  time.Now().Add(-3 * 24 * time.Hour),
		MatchCount: 4,
	}

	got := formatSessionLine(1, r)
	if strings.Contains(got, ":") {
		t.Errorf("formatSessionLine() = %q, should carry no host separator", got)
	}
	want := "1. [lora-bootloader/3d/4hits] \"why does the second stage hang\" — claude\n"
	if got != want {
		t.Errorf("formatSessionLine() = %q, want %q", got, want)
	}
}

func TestFormatSessionLineFallsBackToTheProject(t *testing.T) {
	r := db.SessionMatch{
		Project:    "lora-bootloader",
		Tool:       "claude",
		Host:       "mercury2-emb-ah3",
		StartTime:  time.Now().Add(-2 * time.Hour),
		MatchCount: 1,
	}

	got := formatSessionLine(2, r)
	want := "2. [mercury2-emb-ah3:lora-bootloader/2h/1hits] \"lora-bootloader\" — claude\n"
	if got != want {
		t.Errorf("formatSessionLine() = %q, want %q", got, want)
	}
}
