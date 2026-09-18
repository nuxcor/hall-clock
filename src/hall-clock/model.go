package main

import "time"

type TimerStatus string

const (
	StatusIdle    TimerStatus = "idle"
	StatusRunning TimerStatus = "running"
	StatusPaused  TimerStatus = "paused"
)

type Talk struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Duration  int    `json:"durationSeconds"`
	Closing   int    `json:"closingSeconds"`
	Temporary bool   `json:"temporary,omitempty"`
	// CreatedAt is only set on temporary parts and ties the part to the
	// meeting session it was added in, so it can be purged when the next
	// congregation's meeting starts.
	CreatedAt time.Time `json:"-"`
}

type Config struct {
	Version           int    `json:"version,omitempty"`
	DeviceName        string `json:"deviceName"`
	AdvertisedBaseURL string `json:"advertisedBaseUrl"`
	ControlToken      string `json:"controlToken"`
	// ControlPIN is the pairing PIN, stored as typed so /setup can show the
	// hall which one is current. Empty means none has been set yet, which is
	// what opens the first-boot grace window — see pairingGrace. handleConfig
	// builds its response from an explicit allowlist, so this never rides along
	// with the settings; only the token-protected GET /api/pairing/pin returns it.
	ControlPIN               string                             `json:"controlPin,omitempty"`
	MeetingType              string                             `json:"meetingType"`
	MeetingStartTime         string                             `json:"meetingStartTime"`
	MeetingStarts            []MeetingStart                     `json:"meetingStarts"`
	PrestartSeconds          int                                `json:"prestartSeconds"`
	MidweekURL               string                             `json:"midweekUrl"`
	MidweekLanguage          string                             `json:"midweekLanguage,omitempty"`
	MidweekLanguageSources   map[string]string                  `json:"midweekLanguageSources,omitempty"`
	MidweekLanguageSchedules map[string]MidweekLanguageSchedule `json:"midweekLanguageSchedules,omitempty"`
	AutoImportMidweek        bool                               `json:"autoImportMidweek"`
	MidweekImportedWeek      string                             `json:"midweekImportedWeek,omitempty"`
	// MidweekLanguageOverrideUntil marks an operator's manual language switch.
	// While now is before this time, the automatic writers — the idle
	// meeting-start language sync and the weekly import rotation — leave the
	// active language alone, so a person's explicit choice is never silently
	// reverted mid-session. Persisted so a reboot doesn't forget the choice.
	MidweekLanguageOverrideUntil time.Time `json:"midweekLanguageOverrideUntil,omitempty"`
	// CircuitOverseerExpiresAt is when the circuit-overseer-visit schedule stops
	// applying. The mode is "active" only while now < this time, so it scopes to
	// a single meeting session and auto-clears (see circuitOverseerDuration).
	// Persisted so it survives a reboot mid-visit and still expires on schedule.
	CircuitOverseerExpiresAt time.Time `json:"circuitOverseerExpiresAt,omitempty"`
	// Schedule is the congregation's baseline midweek program: the last WOL
	// import, applied template, or the built-in default. Operator edits never
	// land here — they go to ScheduleOverride, so the baseline stays recoverable
	// once an edit expires.
	Schedule []Talk `json:"schedule"`
	// ScheduleOverride is a hand-edited midweek program that replaces Schedule
	// until ScheduleOverrideExpiresAt passes. It scopes an edit to one meeting
	// session, so a second congregation sharing the box the same day starts from
	// the baseline (see scheduleOverrideDuration), exactly like CO mode.
	ScheduleOverride []Talk `json:"scheduleOverride,omitempty"`
	// ScheduleOverrideExpiresAt is when ScheduleOverride stops applying.
	// Persisted so it survives a reboot mid-meeting and still expires on time.
	ScheduleOverrideExpiresAt time.Time `json:"scheduleOverrideExpiresAt,omitempty"`
}

type MeetingStart struct {
	ID                  int    `json:"id"`
	Day                 int    `json:"day"`
	Time                string `json:"time"`
	Congregation        string `json:"congregation"`
	Language            string `json:"language,omitempty"`
	MidweekURL          string `json:"midweekUrl,omitempty"`
	MidweekImportedWeek string `json:"midweekImportedWeek,omitempty"`
}

type MidweekLanguageSchedule struct {
	ImportedWeek string `json:"importedWeek"`
	URL          string `json:"url,omitempty"`
	Schedule     []Talk `json:"schedule"`
}

type State struct {
	Status            TimerStatus    `json:"status"`
	DeviceName        string         `json:"deviceName"`
	MeetingType       string         `json:"meetingType"`
	MeetingStartTime  string         `json:"meetingStartTime"`
	MeetingStarts     []MeetingStart `json:"meetingStarts"`
	PrestartLabel     string         `json:"prestartLabel"`
	PrestartSeconds   int            `json:"prestartSeconds"`
	PrestartActive    bool           `json:"prestartActive"`
	PrestartRemaining int            `json:"prestartRemainingSeconds"`
	CurrentTalkID     int            `json:"currentTalkId"`
	CurrentTalkTitle  string         `json:"currentTalkTitle"`
	DurationSeconds   int            `json:"durationSeconds"`
	RemainingSeconds  int            `json:"remainingSeconds"`
	ElapsedSeconds    int            `json:"elapsedSeconds"`
	ClosingSeconds    int            `json:"closingSeconds"`
	OvertimeSeconds   int            `json:"overtimeSeconds"`
	// MeetingOvertimeSeconds is how far the whole meeting is behind: the overtime
	// banked from parts already finished, plus whatever the current part is over
	// right now. Time saved on a short part does not pay it back — a meeting that
	// runs long runs long, and the chairman needs the number that is actually
	// costing him.
	MeetingOvertimeSeconds int `json:"meetingOvertimeSeconds"`
	// MeetingInProgress is true from the first Start of a meeting until it is
	// over, including while the clock sits idle between parts. Every Next
	// leaves the clock idle, so Status cannot answer "is a meeting under way";
	// anything that must wait for the meeting to finish keys off this instead.
	MeetingInProgress        bool       `json:"meetingInProgress"`
	CircuitOverseer          bool       `json:"circuitOverseer"`
	CircuitOverseerExpiresAt *time.Time `json:"circuitOverseerExpiresAt,omitempty"`
	// ScheduleOverrideExpiresAt lets the UI show how long a hand-edited schedule
	// still applies before the baseline returns; nil when no edit is active.
	ScheduleOverrideExpiresAt *time.Time `json:"scheduleOverrideExpiresAt,omitempty"`
	MidweekLanguage           string     `json:"midweekLanguage,omitempty"`
	Schedule                  []Talk     `json:"schedule"`
	Now                       time.Time  `json:"now"`
	PairingActive             bool       `json:"pairingActive"`
	PairingExpiresAt          *time.Time `json:"pairingExpiresAt,omitempty"`
}

// partOverrun is how far one finished part ran past its time. The talk it came
// from is kept so a future undo can identify what it is giving back.
type partOverrun struct {
	talkID  int
	seconds int
}

type clockTime struct {
	hour   int
	minute int
}
