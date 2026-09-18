package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{AutoImportMidweek: true}, nil
	}
	if err != nil {
		return Config{}, err
	}
	// An empty file is a config that was being written when the box lost power
	// or the process was killed.
	if len(bytes.TrimSpace(data)) == 0 {
		log.Printf("config: %s is empty", path)
		return loadConfigBackup(path), nil
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		// A clock that will not boot is worse than a clock that forgot its
		// schedule: an unreadable config used to crash-loop the appliance until
		// somebody could reach it with ssh, which for most halls is nobody.
		// Keep a copy of the bad file for forensics and come up on the last good
		// copy. Copied, not moved: the startup save that replaces it can fail on
		// the same card that corrupted it, and a config.json moved aside reads as
		// a deliberate reset on the next boot — defaults, and pairing open.
		corrupt := path + ".corrupt"
		if copyErr := os.WriteFile(corrupt, data, 0o600); copyErr != nil {
			log.Printf("config: could not keep a copy of unreadable %s: %v", path, copyErr)
		}
		log.Printf("config: %s is unreadable (%v); bad copy kept at %s", path, err, corrupt)
		return loadConfigBackup(path), nil
	}
	return config, nil
}

// configBackupPath holds the last config written successfully. It is read only
// when config.json itself is unreadable: a missing config.json is a deliberate
// reset, and the backup must not undo it.
func configBackupPath(path string) string {
	return path + ".bak"
}

// loadConfigBackup falls back to the last good config, and to defaults when
// there is none. Defaults are a last resort, not a neutral choice: they drop the
// PIN and mint a new token, which strands every paired phone and opens the
// first-boot pairing window to anyone on the network — in a hall that had
// locked its clock.
func loadConfigBackup(path string) Config {
	backup := configBackupPath(path)
	if data, err := os.ReadFile(backup); err == nil {
		var config Config
		if err := json.Unmarshal(data, &config); err == nil {
			log.Printf("config: restored the last good copy from %s", backup)
			return config
		}
	}
	log.Printf("config: no usable copy at %s; starting from defaults", backup)
	return Config{AutoImportMidweek: true}
}

// saveConfig writes the config atomically: a temporary file in the same
// directory, flushed to disk, then renamed over the target. os.WriteFile would
// truncate the real file first, so a process killed mid-write (systemd stopping
// the unit for an update, or the Pi losing power) would leave an empty config
// behind and the app would never boot again.
func saveConfig(path string, config Config) error {
	data, err := marshalConfig(config)
	if err != nil {
		return err
	}
	return writeConfigFile(path, data)
}

func marshalConfig(config Config) ([]byte, error) {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func writeConfigFile(path string, data []byte) error {
	if err := writeFileAtomic(path, data); err != nil {
		return err
	}
	// Best-effort: the config itself is already safely on disk.
	if err := writeFileAtomic(configBackupPath(path), data); err != nil {
		log.Printf("config: could not write backup: %v", err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Rename is atomic, but on an SD card the rename can land before the data
	// does. Flush first, or a power cut leaves a valid name pointing at zeroes.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// The rename lives in the directory, which is flushed separately: without
	// this, a power cut seconds after a save could bring back the old file — a
	// PIN just set, gone at the next boot. Best-effort, since not every platform
	// can sync a directory.
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		dirFile.Close()
	}
	return nil
}

// persistConfig snapshots the current config under s.mu and writes it to disk.
// Writers are serialized and sequenced so a slower, older snapshot can never
// rename over a newer one — the marshal happens under s.mu, so the snapshot
// can't observe a half-applied mutation either.
func (s *server) persistConfig() error {
	s.mu.Lock()
	s.saveSeq++
	seq := s.saveSeq
	data, err := marshalConfig(s.config)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if seq <= s.lastSavedSeq {
		// A newer snapshot already reached disk while we waited.
		return nil
	}
	if err := writeConfigFile(s.configPath, data); err != nil {
		return err
	}
	s.lastSavedSeq = seq
	return nil
}

func defaultConfigPath() string {
	if path := os.Getenv("WALL_CLOCK_CONFIG"); path != "" {
		return path
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "hall-clock", "config.json")
	}
	return "hall-clock.json"
}

func normalizeMeetingType(meetingType string) string {
	switch strings.ToLower(strings.TrimSpace(meetingType)) {
	case "weekend":
		return "weekend"
	default:
		return "midweek"
	}
}

func normalizeStartTime(startTime string) string {
	startTime = strings.TrimSpace(startTime)
	if startTime == "" {
		return "19:00"
	}
	parsed, err := parseClockTime(startTime)
	if err != nil {
		return "19:00"
	}
	return fmt.Sprintf("%02d:%02d", parsed.hour, parsed.minute)
}

func parseClockTime(value string) (clockTime, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return clockTime{}, errors.New("invalid time")
	}

	hour, err := parsePositiveInt(parts[0])
	if err != nil {
		return clockTime{}, err
	}
	minute, err := parsePositiveInt(parts[1])
	if err != nil {
		return clockTime{}, err
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return clockTime{}, errors.New("invalid time")
	}
	return clockTime{hour: hour, minute: minute}, nil
}

func normalizeMeetingStarts(starts []MeetingStart, fallbackStartTime string) []MeetingStart {
	if len(starts) == 0 {
		return defaultMeetingStarts(fallbackStartTime)
	}

	normalized := make([]MeetingStart, 0, len(starts))
	for _, start := range starts {
		if start.Day < int(time.Sunday) || start.Day > int(time.Saturday) {
			continue
		}
		start.Time = normalizeStartTime(start.Time)
		start.Congregation = truncateRunes(strings.TrimSpace(start.Congregation), maxNameRunes)
		start.MidweekURL = strings.TrimSpace(start.MidweekURL)
		// Only ever an ISO week this app wrote, like "2026-W38". The setup page
		// posts it back, and anything else would be kept and rebroadcast to
		// every screen four times a second.
		if !isoWeekPattern.MatchString(start.MidweekImportedWeek) {
			start.MidweekImportedWeek = ""
		}
		if language := normalizeMidweekLanguage(start.Language); language != "" {
			start.Language = language
		} else {
			start.Language = normalizeMidweekLanguage(wolLanguage(start.MidweekURL))
		}
		normalized = append(normalized, start)
	}
	if len(normalized) == 0 {
		return defaultMeetingStarts(fallbackStartTime)
	}
	sort.SliceStable(normalized, func(i, j int) bool {
		if normalized[i].Day != normalized[j].Day {
			return normalized[i].Day < normalized[j].Day
		}
		return normalized[i].Time < normalized[j].Time
	})
	for i := range normalized {
		normalized[i].ID = i + 1
	}
	return normalized
}

func defaultMeetingStarts(startTime string) []MeetingStart {
	startTime = normalizeStartTime(startTime)
	starts := make([]MeetingStart, 0, 6)
	starts = append(starts, MeetingStart{ID: 1, Day: int(time.Sunday), Time: "10:00"})
	for day := int(time.Monday); day <= int(time.Friday); day++ {
		starts = append(starts, MeetingStart{
			ID:           len(starts) + 1,
			Day:          day,
			Time:         startTime,
			Congregation: "",
		})
	}
	return starts
}

func hasWeekendStart(starts []MeetingStart) bool {
	for _, start := range starts {
		if start.Day == int(time.Sunday) || start.Day == int(time.Saturday) {
			return true
		}
	}
	return false
}

const (
	// maxSchedulePartID caps the ids a saved programme may use, and ad-hoc parts
	// number from temporaryPartIDBase up, so the two ranges never meet. Ad-hoc
	// parts used to take the highest id plus one, which the next part a save
	// added took as well — and Next then bounced between the two all meeting.
	maxSchedulePartID   = 9999
	temporaryPartIDBase = maxSchedulePartID + 1

	// These bound what a paired phone can make the clock hold and rebroadcast
	// to every screen four times a second.
	maxScheduleParts = 100
	maxTitleRunes    = 200
	maxMeetingStarts = 50
	maxNameRunes     = 100
	maxURLLength     = 2048
)

// normalizeSchedule tidies a programme in place. It keeps the ids a schedule
// arrives with: the running part is found by id across every save, and
// renumbering by position made deleting an earlier part relabel the part being
// spoken — or, when the last part was on, stop the clock outright. Only a
// missing, duplicate or out-of-range id is given a fresh one.
func normalizeSchedule(schedule []Talk) {
	normalizeScheduleAbove(schedule, 0)
}

// normalizeScheduleAbove is normalizeSchedule with fresh ids numbered above
// floor. A save passes the highest id the clock already holds: numbering from
// the schedule alone handed a new part the id of one deleted in the same save,
// and if that was the part on the clock, the new part took over its timer.
func normalizeScheduleAbove(schedule []Talk, floor int) {
	highest := max(0, min(floor, maxSchedulePartID))
	for _, talk := range schedule {
		if validPartID(talk.ID) {
			highest = max(highest, talk.ID)
		}
	}
	claimed := make(map[int]bool, len(schedule))
	for i := range schedule {
		if !validPartID(schedule[i].ID) || claimed[schedule[i].ID] {
			highest++
			schedule[i].ID = highest
		}
		claimed[schedule[i].ID] = true
	}
	// Only reachable after thousands of added parts in one lineage of edits; a
	// fresh numbering is better than an id in the ad-hoc range.
	if highest > maxSchedulePartID {
		for i := range schedule {
			schedule[i].ID = i + 1
		}
	}
	for i := range schedule {
		schedule[i].Title = truncateRunes(strings.TrimSpace(schedule[i].Title), maxTitleRunes)
		if schedule[i].Title == "" {
			schedule[i].Title = fmt.Sprintf("Part %d", i+1)
		}
		schedule[i].Duration = clamp(schedule[i].Duration, 60, 7200)
		schedule[i].Closing = clamp(schedule[i].Closing, 0, schedule[i].Duration)
		// Config schedules are never temporary; a temporary part that leaks
		// into a save would otherwise be silently purged at runtime while
		// still sitting in the config file.
		schedule[i].Temporary = false
		schedule[i].CreatedAt = time.Time{}
	}
}

var isoWeekPattern = regexp.MustCompile(`^\d{4}-W\d{2}$`)

func validPartID(id int) bool {
	return id >= 1 && id <= maxSchedulePartID
}

// truncateRunes cuts s to at most n characters without splitting one.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return strings.TrimSpace(string(runes[:n]))
}

func clamp(value, minValue, maxValue int) int {
	return min(max(value, minValue), maxValue)
}
