package model

import (
	"testing"
	"time"
)

func TestDumpSchedule(t *testing.T) {
	schedule := DumpSchedule{Enabled: true, Days: []int{0, 4}, Time: "04:00", Timezone: "Europe/Berlin"}
	for _, tc := range []struct{ after, want string }{
		{"2026-09-06T01:59:00Z", "2026-09-06T02:00:00Z"},
		{"2026-09-06T02:00:00Z", "2026-09-10T02:00:00Z"},
		{"2026-10-24T12:00:00Z", "2026-10-25T03:00:00Z"},
	} {
		after, _ := time.Parse(time.RFC3339, tc.after)
		if got := schedule.Next(after).UTC().Format(time.RFC3339); got != tc.want {
			t.Errorf("Next(%s) = %s, want %s", tc.after, got, tc.want)
		}
	}
	schedule.Enabled = false
	if !schedule.Next(time.Now()).IsZero() {
		t.Fatal("disabled schedule has a next run")
	}
}

func TestDumpScheduleDST(t *testing.T) {
	schedule := DumpSchedule{Enabled: true, Days: []int{0}, Time: "02:30", Timezone: "Europe/Berlin"}
	after, _ := time.Parse(time.RFC3339, "2026-03-28T12:00:00Z")
	if got := schedule.Next(after).UTC().Format(time.RFC3339); got != "2026-04-05T00:30:00Z" {
		t.Fatalf("spring gap = %s", got)
	}
	after, _ = time.Parse(time.RFC3339, "2026-10-24T12:00:00Z")
	first := schedule.Next(after)
	second := schedule.Next(first)
	if first.Day() != 25 || first.Hour() != 2 || first.Minute() != 30 || second.Day() != 1 || second.Month() != time.November {
		t.Fatalf("fall overlap should run once: %v then %v", first, second)
	}
}

func TestDumpScheduleValidation(t *testing.T) {
	for _, schedule := range []DumpSchedule{
		{Days: []int{0}, Time: "4:00", Timezone: "UTC"},
		{Days: []int{0}, Time: "24:00", Timezone: "UTC"},
		{Days: []int{0}, Time: "04:00", Timezone: "not/a/timezone"},
		{Days: []int{0}, Time: "04:00", Timezone: ""},
		{Days: []int{7}, Time: "04:00", Timezone: "UTC"},
		{Days: []int{0, 0}, Time: "04:00", Timezone: "UTC"},
		{Time: "04:00", Timezone: "UTC"},
	} {
		if err := schedule.Validate(); err == nil {
			t.Errorf("accepted invalid schedule: %+v", schedule)
		}
	}
}
