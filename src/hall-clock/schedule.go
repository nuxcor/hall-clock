package main

import (
	"strings"
	"time"
)

// sessionWindow is how long a setting that belongs to one meeting session stays
// in effect after the operator turns it on: circuit-overseer mode and a
// hand-edited schedule both use it, so a box shared by congregations hands the
// next meeting a clean slate. The two must stay equal — a CO visit and the
// edited program it runs against have to expire together.
const sessionWindow = 3 * time.Hour

// sessionWindowActive reports whether a session-scoped setting is still in
// effect. A zero expiry means "never turned on".
func sessionWindowActive(expiresAt time.Time, now time.Time) bool {
	return !expiresAt.IsZero() && now.Before(expiresAt)
}

// sessionWindowExpiryPtr returns the expiry for the state snapshot, or nil when
// the setting is not in effect (so the UI can show/omit a countdown).
func sessionWindowExpiryPtr(expiresAt time.Time, now time.Time) *time.Time {
	if !sessionWindowActive(expiresAt, now) {
		return nil
	}
	e := expiresAt
	return &e
}

// circuitOverseerDuration is how long the CO-visit schedule stays active after
// the operator turns it on, so it applies to one meeting session and then
// clears itself (a box shared by congregations without a CO visit is unaffected).
const circuitOverseerDuration = sessionWindow

// circuitOverseerActive reports whether CO mode is currently in effect.
func circuitOverseerActive(expiresAt time.Time, now time.Time) bool {
	return sessionWindowActive(expiresAt, now)
}

// circuitOverseerExpiryPtr returns the expiry for the state snapshot, or nil
// when CO mode is not active (so the UI can show/omit a countdown).
func circuitOverseerExpiryPtr(expiresAt time.Time, now time.Time) *time.Time {
	return sessionWindowExpiryPtr(expiresAt, now)
}

// scheduleOverrideDuration is how long a hand-edited schedule stays in effect
// after the operator saves it, so the edit applies to one meeting session and
// then clears itself. A box shared by congregations gives the next meeting the
// baseline program rather than the previous congregation's tweaks.
const scheduleOverrideDuration = sessionWindow

// setBaselineScheduleLocked installs a new baseline midweek program (an import,
// a template, or a language switch) and drops any hand-edited override: the
// operator asked for this program, so a stale edit must not keep shadowing it.
//
// This only updates config. The caller must follow with
// applyActiveScheduleChangeLocked to rebuild the running talks.
func (s *server) setBaselineScheduleLocked(schedule []Talk) {
	s.config.Schedule = schedule
	s.config.ScheduleOverride = nil
	s.config.ScheduleOverrideExpiresAt = time.Time{}
}

// scheduleOverrideApplies is the single authority on whether a hand-edited
// schedule governs. It is deliberately not a pure function of the clock: an
// edit that was governing when the meeting started keeps governing until the
// meeting is over, because the program on the wall must never change under the
// meeting it is timing. Between meetings, the window decides.
//
// "Over" is meetingInProgress, not an idle timer. Every Next leaves the clock
// idle, so gating on idle let an edit saved at five o'clock lapse at the first
// part change after eight, halfway through a seven o'clock meeting, and bring
// back the parts the operator had removed.
//
// Every read of the midweek program goes through here. Splitting this decision
// across a time-only check and an idle-gated sweep is what let a running
// meeting snap back to the baseline mid-part.
func scheduleOverrideApplies(config Config, meetingInProgress bool, now time.Time) bool {
	if len(config.ScheduleOverride) == 0 {
		return false
	}
	if meetingInProgress {
		return true
	}
	return sessionWindowActive(config.ScheduleOverrideExpiresAt, now)
}

// effectiveMidweekSchedule is the midweek program the clock should run right
// now: the operator's edit while it governs, and the congregation's baseline
// once it no longer does.
func effectiveMidweekSchedule(config Config, meetingInProgress bool, now time.Time) []Talk {
	if scheduleOverrideApplies(config, meetingInProgress, now) {
		return config.ScheduleOverride
	}
	return config.Schedule
}

func (s *server) effectiveMidweekScheduleLocked(now time.Time) []Talk {
	return effectiveMidweekSchedule(s.config, s.meetingInProgressLocked(now), now)
}

func (s *server) scheduleOverrideAppliesLocked(now time.Time) bool {
	return scheduleOverrideApplies(s.config, s.meetingInProgressLocked(now), now)
}

// derivedClosingSeconds is the WOL import's definition of a part's closing
// bell: how many seconds before time runs out the clock turns amber. It is the
// only place the rule is written down — parseMidweekTimings and every save go
// through it.
func derivedClosingSeconds(minutes int) int {
	return min(120, minutes*30)
}

// applyImportedClosingSeconds overwrites the closing bell on a submitted
// schedule. The operator sees the value but cannot edit it: the import defines
// it, so a save must never carry a client's idea of what it should be.
//
// Parts are matched to the baseline by title, which is what the import names
// them. A renamed part, an added part, or one whose bell no longer fits its
// shortened duration falls back to the import's own formula — a bell as long as
// the part would paint the whole thing amber.
func applyImportedClosingSeconds(schedule []Talk, baseline []Talk) {
	imported := make(map[string]int, len(baseline))
	for _, talk := range baseline {
		imported[closingKey(talk.Title)] = talk.Closing
	}
	for i := range schedule {
		closing, ok := imported[closingKey(schedule[i].Title)]
		if !ok || closing >= schedule[i].Duration {
			closing = derivedClosingSeconds(schedule[i].Duration / 60)
		}
		schedule[i].Closing = clamp(closing, 0, schedule[i].Duration)
	}
}

func closingKey(title string) string {
	return strings.ToLower(strings.TrimSpace(title))
}

func defaultSchedule() []Talk {
	return []Talk{
		{ID: 1, Title: "Opening Comments", Duration: 60, Closing: 30},
		{ID: 2, Title: "Treasures From God's Word", Duration: 600, Closing: 120},
		{ID: 3, Title: "Spiritual Gems", Duration: 600, Closing: 120},
		{ID: 4, Title: "Bible Reading", Duration: 240, Closing: 120},
		{ID: 5, Title: "Apply Yourself to the Field Ministry", Duration: 300, Closing: 120},
		{ID: 6, Title: "Living as Christians", Duration: 900, Closing: 120},
		{ID: 7, Title: "Congregation Bible Study", Duration: 1800, Closing: 120},
		{ID: 8, Title: "Concluding Comments", Duration: 180, Closing: 60},
	}
}

// weekendTitles translates the weekend program. The weekend parts are fixed
// rather than imported from WOL, so every language has to be spelled out here.
var weekendTitles = map[string]struct {
	publicTalk        string
	watchtowerStudy   string
	watchtowerSummary string
}{
	"en": {"Public Talk", "Watchtower Study", "Watchtower Summary"},
	"es": {"Discurso público", "Estudio de La Atalaya", "Estudio de La Atalaya"},
	"tw": {"Baguam Kasa", "Ɔwɛn-Aban Adesua", "Ɔwɛn-Aban Adesua"},
}

func weekendTitlesFor(language string) struct {
	publicTalk        string
	watchtowerStudy   string
	watchtowerSummary string
} {
	if titles, ok := weekendTitles[normalizeMidweekLanguage(language)]; ok {
		return titles
	}
	return weekendTitles["en"]
}

func weekendSchedule(language string) []Talk {
	titles := weekendTitlesFor(language)
	return []Talk{
		{ID: 1, Title: titles.publicTalk, Duration: 1800, Closing: 300},
		{ID: 2, Title: titles.watchtowerStudy, Duration: 3600, Closing: 300},
	}
}

func meetingTypeForTime(now time.Time) string {
	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
		return "weekend"
	}
	return "midweek"
}

func scheduleForMeetingType(meetingType string, midweekSchedule []Talk, circuitOverseer bool, midweekLanguage string) []Talk {
	if meetingType == "weekend" {
		if circuitOverseer {
			return circuitOverseerWeekendSchedule(midweekLanguage)
		}
		return weekendSchedule(midweekLanguage)
	}
	if circuitOverseer {
		return circuitOverseerMidweekSchedule(midweekSchedule)
	}
	return midweekSchedule
}

// congregationBibleStudyMarkers matches the Congregation Bible Study part by the
// title WOL publishes for it in each supported language, so CO mode finds the
// part to replace no matter which language the week was imported in.
var congregationBibleStudyMarkers = map[string]string{
	"en": "bible study",
	"es": "estudio bíblico",
	"tw": "bible adesua",
}

// serviceTalkTitles translates the service talk the CO gives in place of the
// midweek Congregation Bible Study. WOL never publishes this part, so unlike
// every other midweek title it cannot be imported.
var serviceTalkTitles = map[string]string{
	"en": "Service Talk",
	"es": "Discurso de servicio",
	"tw": "Ɔsom Kasa",
}

// congregationBibleStudyLanguage reports the language of a Congregation Bible
// Study title. The markers do not overlap, so at most one can match.
func congregationBibleStudyLanguage(title string) (string, bool) {
	lowered := strings.ToLower(strings.TrimSpace(title))
	for language, marker := range congregationBibleStudyMarkers {
		if strings.Contains(lowered, marker) {
			return language, true
		}
	}
	return "", false
}

func serviceTalkTitle(language string) string {
	if title := serviceTalkTitles[normalizeMidweekLanguage(language)]; title != "" {
		return title
	}
	return serviceTalkTitles["en"]
}

// circuitOverseerWeekendSchedule is the weekend program during a circuit
// overseer visit: a 30-minute public talk, a 30-minute Watchtower summary, and
// the CO's 30-minute service talk.
func circuitOverseerWeekendSchedule(language string) []Talk {
	titles := weekendTitlesFor(language)
	return []Talk{
		{ID: 1, Title: titles.publicTalk, Duration: 1800, Closing: 300},
		{ID: 2, Title: titles.watchtowerSummary, Duration: 1800, Closing: 300},
		{ID: 3, Title: serviceTalkTitle(language), Duration: 1800, Closing: 300},
	}
}

// circuitOverseerMidweekSchedule replaces the Congregation Bible Study with the
// CO's 30-minute service talk, leaving the rest of the midweek program intact.
// It returns a copy so the saved schedule is never mutated.
//
// The service talk is titled in the language of the part it replaces, not the
// language recorded in config. Those disagree whenever the config flag is stale
// (or was never written), and the schedule on the projector has to read as one
// language: an English program with one Spanish part in the middle is worse than
// an English program.
func circuitOverseerMidweekSchedule(base []Talk) []Talk {
	out := make([]Talk, 0, len(base))
	replaced := false
	for _, talk := range base {
		partLanguage, isBibleStudy := congregationBibleStudyLanguage(talk.Title)
		if !replaced && isBibleStudy {
			out = append(out, Talk{
				ID:       talk.ID,
				Title:    serviceTalkTitle(partLanguage),
				Duration: 1800,
				Closing:  talk.Closing,
			})
			replaced = true
			continue
		}
		out = append(out, talk)
	}
	return out
}

func sameSchedule(a, b []Talk) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// isWeekendSchedule reports whether a saved schedule is really the weekend
// template, in any language — a midweek schedule must never be one of those.
// Ids are ignored: they survive edits now, so a copy of the template need not be
// numbered 1 and 2.
func isWeekendSchedule(schedule []Talk) bool {
	positional := append([]Talk(nil), schedule...)
	for i := range positional {
		positional[i].ID = i + 1
	}
	for language := range weekendTitles {
		weekend := weekendSchedule(language)
		normalizeSchedule(weekend)
		if sameSchedule(positional, weekend) {
			return true
		}
	}
	return false
}
