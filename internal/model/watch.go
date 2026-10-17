package model

import (
	"fmt"
	"slices"
	"time"
)

// DumpSchedule describes a weekly wall-clock schedule, independent of the
// timezone of the browser or the server process. Sunday is day zero.
type DumpSchedule struct {
	Enabled  bool   `json:"enabled"`
	Days     []int  `json:"days"`
	Time     string `json:"time"`
	Timezone string `json:"timezone"`
}

func DefaultDumpSchedule() DumpSchedule {
	return DumpSchedule{Days: []int{0, 4}, Time: "04:00", Timezone: "Local"}
}

func (s DumpSchedule) Validate() error {
	if _, err := time.Parse("15:04", s.Time); err != nil || len(s.Time) != 5 {
		return fmt.Errorf("time must use HH:MM (24-hour clock)")
	}
	if s.Timezone == "" {
		return fmt.Errorf("choose a timezone")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("unknown timezone %q", s.Timezone)
	}
	if len(s.Days) == 0 {
		return fmt.Errorf("choose at least one weekday")
	}
	seen := map[int]bool{}
	for _, d := range s.Days {
		if d < 0 || d > 6 || seen[d] {
			return fmt.Errorf("weekdays must be distinct numbers from 0 (Sunday) to 6 (Saturday)")
		}
		seen[d] = true
	}
	return nil
}

// Next returns the next scheduled instant strictly after after. A nonexistent
// local time during the spring DST change is skipped; the fall overlap runs once.
func (s DumpSchedule) Next(after time.Time) time.Time {
	if !s.Enabled || s.Validate() != nil {
		return time.Time{}
	}
	loc, _ := time.LoadLocation(s.Timezone)
	clock, _ := time.Parse("15:04", s.Time)
	local := after.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, loc)
	for i := 0; i < 15; i++ {
		d := day.AddDate(0, 0, i)
		if !slices.Contains(s.Days, int(d.Weekday())) {
			continue
		}
		next := time.Date(d.Year(), d.Month(), d.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
		if next.Hour() == clock.Hour() && next.Minute() == clock.Minute() && next.After(after) {
			return next
		}
	}
	return time.Time{}
}

type ReleaseNotification struct {
	ID          int64  `json:"id"`
	ArtistID    string `json:"artistId"`
	ArtistName  string `json:"artistName"`
	ReleaseID   string `json:"releaseId"`
	Title       string `json:"title"`
	ReleaseDate string `json:"releaseDate"`
	Kind        string `json:"kind"`
	PrimaryType string `json:"primaryType"` // Album, EP or Single
	Unread      bool   `json:"unread"`
}
