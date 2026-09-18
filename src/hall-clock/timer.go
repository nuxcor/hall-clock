package main

import (
	"strings"
	"time"
)

const meetingStartLanguageLead = 30 * time.Minute

// adhocPartGrace keeps ad-hoc items added while setting up ahead of the
// prestart window: an item created up to an hour before the session boundary
// was prepared for this meeting, not left over from the previous one. Without
// it, an item added six minutes early was silently purged at the first idle
// moment after the meeting began.
const adhocPartGrace = time.Hour

// applyScheduleLocked swaps in a new schedule without killing an in-progress
// timer: while running or paused, the current talk keeps counting and an
// edited duration shifts the remaining time like a manual adjust would.
func (s *server) applyScheduleLocked(schedule []Talk) {
	merged := mergeTemporaryParts(schedule, s.talks)
	s.talks = merged
	s.state.Schedule = merged

	if s.state.Status == StatusIdle {
		for _, talk := range merged {
			if talk.ID != s.state.CurrentTalkID {
				continue
			}
			// Keep the operator's selection; refresh the part's metadata and
			// only reset the clock when the part's duration actually changed.
			s.state.CurrentTalkTitle = talk.Title
			s.state.ClosingSeconds = talk.Closing
			if talk.Duration != s.state.DurationSeconds {
				s.state.DurationSeconds = talk.Duration
				s.state.RemainingSeconds = talk.Duration
				s.state.ElapsedSeconds = 0
				s.state.OvertimeSeconds = 0
				s.remainingAt = talk.Duration
			}
			return
		}
		s.selectTalkLocked(merged[0].ID)
		return
	}
	for _, talk := range merged {
		if talk.ID != s.state.CurrentTalkID {
			continue
		}
		s.recalculateLocked(s.clock())
		if delta := talk.Duration - s.state.DurationSeconds; delta != 0 {
			// Shift the baseline rather than restarting the count from now:
			// restarting dropped whatever fraction of a second had already run.
			s.state.DurationSeconds = talk.Duration
			s.state.RemainingSeconds += delta
			s.remainingAt += delta
		}
		s.state.CurrentTalkTitle = talk.Title
		s.state.ClosingSeconds = talk.Closing
		return
	}
	s.selectTalkLocked(merged[0].ID)
}

func mergeTemporaryParts(base []Talk, existing []Talk) []Talk {
	if len(existing) == 0 {
		return append([]Talk(nil), base...)
	}

	type temporaryPart struct {
		index int
		talk  Talk
	}

	var temps []temporaryPart
	for i, talk := range existing {
		if talk.Temporary {
			temps = append(temps, temporaryPart{index: i, talk: talk})
		}
	}
	if len(temps) == 0 {
		return append([]Talk(nil), base...)
	}

	out := make([]Talk, 0, len(base)+len(temps))
	tempIdx := 0
	for i := 0; i <= len(base); i++ {
		for tempIdx < len(temps) && temps[tempIdx].index <= i {
			out = append(out, temps[tempIdx].talk)
			tempIdx++
		}
		if i < len(base) {
			out = append(out, base[i])
		}
	}
	// Temps recorded past the end of a shorter base schedule still belong in
	// the meeting: keep them at the end instead of dropping them.
	for ; tempIdx < len(temps); tempIdx++ {
		out = append(out, temps[tempIdx].talk)
	}
	return out
}

// withoutTemporaryTalks returns talks with ad-hoc parts removed; used when the
// active meeting type switches, since parts added for one meeting have no
// place in the other meeting's schedule.
func withoutTemporaryTalks(talks []Talk) []Talk {
	kept := make([]Talk, 0, len(talks))
	for _, talk := range talks {
		if !talk.Temporary {
			kept = append(kept, talk)
		}
	}
	return kept
}

// sameBaseSchedule reports whether the non-temporary talks in talks match
// base, so live ad-hoc parts don't make an unchanged schedule look changed.
func sameBaseSchedule(talks []Talk, base []Talk) bool {
	i := 0
	for _, talk := range talks {
		if talk.Temporary {
			continue
		}
		if i >= len(base) || talk != base[i] {
			return false
		}
		i++
	}
	return i == len(base)
}

func (s *server) selectTalkLocked(talkID int) bool {
	for _, talk := range s.talks {
		if talk.ID == talkID {
			s.state.Status = StatusIdle
			s.state.CurrentTalkID = talk.ID
			s.state.CurrentTalkTitle = talk.Title
			s.state.DurationSeconds = talk.Duration
			s.state.RemainingSeconds = talk.Duration
			s.state.ElapsedSeconds = 0
			s.state.ClosingSeconds = talk.Closing
			s.state.OvertimeSeconds = 0
			s.remainingAt = talk.Duration
			s.pauseCarry = 0
			return true
		}
	}
	return false
}

// syncCircuitOverseerLocked reconciles the effective CO flag with its expiry —
// auto-deactivating the mode once 3 hours have passed so it never carries over
// to another congregation's meeting on a shared box. Rebuilds the schedule only
// once the meeting is over: a visit that outlasts its window between two parts
// must not swap the service talk out from under the program.
func (s *server) syncCircuitOverseerLocked(now time.Time, inProgress bool) {
	active := circuitOverseerActive(s.config.CircuitOverseerExpiresAt, now)
	s.state.CircuitOverseerExpiresAt = circuitOverseerExpiryPtr(s.config.CircuitOverseerExpiresAt, now)
	if active == s.state.CircuitOverseer {
		return
	}
	if inProgress {
		return
	}
	s.state.CircuitOverseer = active
	s.applyScheduleLocked(scheduleForMeetingType(meetingTypeForTime(now), s.effectiveMidweekScheduleLocked(now), active, s.config.MidweekLanguage))
}

// syncScheduleOverrideLocked retires a hand-edited schedule once it stops
// governing, restoring the congregation's baseline program so a second meeting
// on a shared box does not inherit the previous one's edits.
//
// scheduleOverrideApplies is the only expiry test here: it holds an override in
// place for a meeting in progress, so reaching the clear below already implies
// the meeting is over and no part can be disturbed.
func (s *server) syncScheduleOverrideLocked(now time.Time, inProgress bool) {
	if len(s.config.ScheduleOverride) == 0 {
		return
	}
	if scheduleOverrideApplies(s.config, inProgress, now) {
		return
	}
	s.config.ScheduleOverride = nil
	s.config.ScheduleOverrideExpiresAt = time.Time{}
	s.applyScheduleLocked(scheduleForMeetingType(meetingTypeForTime(now), s.config.Schedule, s.state.CircuitOverseer, s.config.MidweekLanguage))
}

func (s *server) syncActiveScheduleLocked(now time.Time, inProgress bool) {
	if inProgress {
		return
	}
	// Full reconciliation, not just the meeting-type flip: a baseline that
	// changed while a meeting was under way (a deferred auto-import) lands here,
	// once it is over. sameBaseSchedule makes the common no-change tick a cheap
	// comparison.
	s.applyActiveScheduleChangeLocked(now)
}

// meetingIdleGap is how long the clock may sit idle before the meeting it was
// timing is taken to be over. Songs, prayers and the chairman's remarks leave it
// idle for minutes at a time between parts; half an hour with nothing on the
// clock is not a meeting any more.
const meetingIdleGap = 30 * time.Minute

// meetingHandoverIdle is how long the clock must have been idle before the next
// meeting's countdown can end the one it was timing. A meeting that finished
// idle well before the countdown hands over the moment it opens; one running
// late, idle for a song between parts as the countdown opens, keeps its
// language, its program and its overtime until it has really stopped.
const meetingHandoverIdle = 10 * time.Minute

// meetingInProgressLocked reports whether a meeting is under way: a part is on
// the clock, or the clock is idle between parts of a meeting that has not
// finished. Idle alone cannot answer this — every Next leaves the clock idle,
// and the reconciliations that were only meant to run between meetings (an
// expired edit, CO mode, the next congregation's language) used to fire at the
// first part change after their window closed.
//
// A meeting finishes when the operator ends it, after meetingIdleGap of idling,
// or once the next meeting's session has begun and the clock has been idle for
// meetingHandoverIdle.
//
// Recalculation works this out once and hands it down: nothing it reconciles
// can change the answer, since each of those acts only between meetings and
// leaves the clock idle.
func (s *server) meetingInProgressLocked(now time.Time) bool {
	if s.state.Status != StatusIdle {
		return true
	}
	if s.meetingActiveAt.IsZero() {
		return false
	}
	idle := now.Sub(s.meetingActiveAt)
	if idle >= meetingIdleGap {
		return false
	}
	if idle >= meetingHandoverIdle {
		if session, ok := s.meetingSessionLocked(now); ok && session.After(s.meetingActiveAt) {
			return false
		}
	}
	return true
}

// currentOvertimeLocked is how far the current part is past its time as of now.
// It reads the timer rather than s.state.OvertimeSeconds, so it does not depend
// on a recalculate having just run — that hidden ordering requirement is what
// once let a stale overrun be recorded.
func (s *server) currentOvertimeLocked(now time.Time) int {
	remaining := s.state.RemainingSeconds
	if s.state.Status == StatusRunning {
		remaining = s.remainingAt - int(now.Sub(s.startedAt).Seconds())
	}
	return max(0, -remaining)
}

// retireCurrentPartLocked records the overrun of the part the operator is
// leaving. Only overtime is kept: finishing early does not pay back a part that
// ran long.
//
// Called only from the two paths where a person leaves a part (changeTalk and
// handleSelect), never from selectTalkLocked. Schedule rebuilds reselect parts
// internally — a purge, an expiring override, a meeting-type swap — and none of
// those are the operator finishing a talk.
func (s *server) retireCurrentPartLocked(now time.Time) {
	if over := s.currentOvertimeLocked(now); over > 0 {
		s.retiredOverruns = append(s.retiredOverruns, partOverrun{
			talkID:  s.state.CurrentTalkID,
			seconds: over,
		})
	}
}

// meetingOvertimeSecondsLocked is how far the whole meeting is behind: every
// retired part's overrun, plus whatever the current part is over right now.
func (s *server) meetingOvertimeSecondsLocked(now time.Time) int {
	total := s.currentOvertimeLocked(now)
	for _, overrun := range s.retiredOverruns {
		total += overrun.seconds
	}
	return total
}

// highestPartIDLocked is the highest programme id the clock holds anywhere —
// running, saved or edited — so a save can number new parts above all of them.
func (s *server) highestPartIDLocked() int {
	highest := 0
	for _, schedule := range [][]Talk{s.talks, s.config.Schedule, s.config.ScheduleOverride} {
		for _, talk := range schedule {
			if validPartID(talk.ID) {
				highest = max(highest, talk.ID)
			}
		}
	}
	return highest
}

// hasTalkLocked reports whether a part is in the running schedule.
func (s *server) hasTalkLocked(talkID int) bool {
	for _, talk := range s.talks {
		if talk.ID == talkID {
			return true
		}
	}
	return false
}

// meetingSessionLocked identifies the current meeting session by its configured
// start minus the prestart window, so setting up counts as part of the meeting.
// The session begins when that window opens, not when the start time arrives:
// looking only for starts already past left the previous session in force for
// the five minutes before the meeting, and a first part started a few seconds
// early had its overtime wiped the moment the real start time came round.
// Reports false when no start is configured.
func (s *server) meetingSessionLocked(now time.Time) (time.Time, bool) {
	prestart := time.Duration(s.config.PrestartSeconds) * time.Second
	sessionStart, ok := latestMeetingStart(now.Add(prestart), s.config.MeetingStarts)
	if !ok {
		return time.Time{}, false
	}
	return sessionStart.Add(-prestart), true
}

// purgeStaleTemporaryPartsLocked drops ad-hoc parts created before the current
// meeting session: they belong to a previous congregation's meeting. The caller
// runs it only between meetings, so a meeting in progress never loses one.
func (s *server) purgeStaleTemporaryPartsLocked(cutoff time.Time) {
	hasTemporary := false
	for _, talk := range s.talks {
		if talk.Temporary {
			hasTemporary = true
			break
		}
	}
	if !hasTemporary {
		return
	}

	kept := make([]Talk, 0, len(s.talks))
	removed := false
	for _, talk := range s.talks {
		if talk.Temporary && talk.CreatedAt.Before(cutoff) {
			removed = true
			continue
		}
		kept = append(kept, talk)
	}
	if !removed || len(kept) == 0 {
		return
	}
	s.talks = kept
	s.state.Schedule = kept
	for _, talk := range kept {
		if talk.ID == s.state.CurrentTalkID {
			return
		}
	}
	s.selectTalkLocked(kept[0].ID)
}

// syncMeetingStartLanguageLocked applies the language implied by the active or
// imminent meeting start. Shared halls can list each congregation's start time
// with its own language, so an idle clock should follow the congregation whose
// meeting is about to start instead of making the operator switch manually.
//
// Never while a meeting is in progress: the next congregation's lead window can
// open before this one's closing parts, and switching between two of them put
// the end of an English meeting on the wall in Spanish.
func (s *server) syncMeetingStartLanguageLocked(now time.Time, inProgress bool) {
	if inProgress {
		return
	}
	// A person's explicit switch outranks the schedule-implied language for the
	// rest of the session; without this the sync reverts the operator's choice
	// on the very next recalculation.
	if now.Before(s.config.MidweekLanguageOverrideUntil) {
		return
	}
	start, _, ok := languageMeetingStartSlot(now, s.config.MeetingStarts)
	if !ok {
		return
	}
	language := meetingStartLanguage(start)
	if language == "" || language == s.config.MidweekLanguage {
		return
	}

	if meetingTypeForTime(now) == "midweek" {
		cached, ok := s.config.MidweekLanguageSchedules[language]
		if !ok || cached.ImportedWeek != isoWeekString(now) || len(cached.Schedule) == 0 {
			return
		}
		if s.cachedLanguageScheduleStaleLocked(cached) {
			return
		}
		s.config.MidweekURL = cached.URL
		s.config.MidweekImportedWeek = cached.ImportedWeek
		s.setBaselineScheduleLocked(append([]Talk(nil), cached.Schedule...))
		normalizeSchedule(s.config.Schedule)
	} else {
		source := meetingStartSource(start, s.config.MidweekURL)
		if source.exampleURL != "" {
			s.config.MidweekURL = source.exampleURL
		}
	}
	s.config.MidweekLanguage = language
	s.state.MidweekLanguage = language
	s.applyActiveScheduleChangeLocked(now)
}

func languageMeetingStartSlot(now time.Time, starts []MeetingStart) (MeetingStart, time.Time, bool) {
	if start, startAt, ok := upcomingLanguageMeetingStartSlot(now, starts); ok {
		return start, startAt, true
	}
	return latestLanguageMeetingStartSlot(now, starts)
}

func upcomingLanguageMeetingStartSlot(now time.Time, starts []MeetingStart) (MeetingStart, time.Time, bool) {
	var selected MeetingStart
	var selectedAt time.Time
	found := false
	for _, slot := range starts {
		hourMinute, err := parseClockTime(slot.Time)
		if err != nil {
			continue
		}
		for dayOffset := 0; dayOffset <= 7; dayOffset++ {
			day := now.AddDate(0, 0, dayOffset)
			if int(day.Weekday()) != slot.Day {
				continue
			}
			start := time.Date(day.Year(), day.Month(), day.Day(), hourMinute.hour, hourMinute.minute, 0, 0, now.Location())
			windowStart := start.Add(-meetingStartLanguageLead)
			if now.Before(windowStart) || now.After(start) {
				continue
			}
			if !found || start.Before(selectedAt) {
				selected = slot
				selectedAt = start
				found = true
			}
		}
	}
	return selected, selectedAt, found
}

func latestLanguageMeetingStartSlot(now time.Time, starts []MeetingStart) (MeetingStart, time.Time, bool) {
	var selected MeetingStart
	var selectedAt time.Time
	found := false
	for _, slot := range starts {
		hourMinute, err := parseClockTime(slot.Time)
		if err != nil {
			continue
		}
		for dayOffset := -1; dayOffset <= 0; dayOffset++ {
			day := now.AddDate(0, 0, dayOffset)
			if int(day.Weekday()) != slot.Day {
				continue
			}
			start := time.Date(day.Year(), day.Month(), day.Day(), hourMinute.hour, hourMinute.minute, 0, 0, now.Location())
			if start.After(now) || !now.Before(start.Add(circuitOverseerDuration)) {
				continue
			}
			if !found || start.After(selectedAt) {
				selected = slot
				selectedAt = start
				found = true
			}
		}
	}
	return selected, selectedAt, found
}

func latestMeetingStartSlot(now time.Time, starts []MeetingStart) (MeetingStart, time.Time, bool) {
	for dayOffset := 0; dayOffset < 8; dayOffset++ {
		day := now.AddDate(0, 0, -dayOffset)
		weekday := int(day.Weekday())
		var best MeetingStart
		var bestAt time.Time
		found := false
		for _, slot := range starts {
			if slot.Day != weekday {
				continue
			}
			hourMinute, err := parseClockTime(slot.Time)
			if err != nil {
				continue
			}
			start := time.Date(day.Year(), day.Month(), day.Day(), hourMinute.hour, hourMinute.minute, 0, 0, now.Location())
			if start.After(now) {
				continue
			}
			if !found || start.After(bestAt) {
				best = slot
				bestAt = start
				found = true
			}
		}
		if found {
			return best, bestAt, true
		}
	}
	return MeetingStart{}, time.Time{}, false
}

// latestMeetingStart returns the most recent configured meeting start at or
// before now, looking back up to a week.
func latestMeetingStart(now time.Time, starts []MeetingStart) (time.Time, bool) {
	_, start, ok := latestMeetingStartSlot(now, starts)
	return start, ok
}

func (s *server) applyActiveScheduleChangeLocked(now time.Time) {
	activeMeetingType := meetingTypeForTime(now)
	activeSchedule := scheduleForMeetingType(activeMeetingType, s.effectiveMidweekScheduleLocked(now), s.state.CircuitOverseer, s.config.MidweekLanguage)
	if s.state.Status == StatusIdle {
		if s.state.MeetingType != activeMeetingType {
			s.state.MeetingType = activeMeetingType
			s.talks = withoutTemporaryTalks(s.talks)
			s.applyScheduleLocked(activeSchedule)
		} else if !sameBaseSchedule(s.talks, activeSchedule) {
			s.applyScheduleLocked(activeSchedule)
		}
		return
	}
	if s.state.MeetingType == activeMeetingType && activeMeetingType == "midweek" {
		s.applyScheduleLocked(activeSchedule)
	}
}

func (s *server) recalculateLocked(now time.Time) {
	if s.state.Status != StatusIdle {
		s.meetingActiveAt = now
	}
	// Asked once: this runs four times a second per screen, and none of the
	// reconciliations below can change the answer.
	inProgress := s.meetingInProgressLocked(now)
	s.syncMeetingStartLanguageLocked(now, inProgress)
	s.syncCircuitOverseerLocked(now, inProgress)
	s.syncScheduleOverrideLocked(now, inProgress)
	s.syncActiveScheduleLocked(now, inProgress)
	// Both the overtime total and the ad-hoc parts are scoped to one meeting.
	// Clear them once it is over, never merely because the clock is idle — it is
	// idle between every pair of parts.
	if !inProgress {
		s.meetingActiveAt = time.Time{}
		s.retiredOverruns = nil
		if session, ok := s.meetingSessionLocked(now); ok {
			s.purgeStaleTemporaryPartsLocked(session.Add(-adhocPartGrace))
		}
	}
	s.state.MeetingInProgress = inProgress
	s.state.Now = now
	s.state.PrestartActive = false
	s.state.PrestartRemaining = 0
	s.state.PrestartLabel = ""
	if s.state.Status != StatusRunning {
		if s.state.Status == StatusIdle {
			if remaining, label, startTime, ok := prestartRemaining(now, s.config.MeetingStarts, s.config.PrestartSeconds); ok {
				s.state.PrestartActive = true
				s.state.PrestartRemaining = remaining
				s.state.PrestartLabel = label
				s.state.MeetingStartTime = startTime
			}
		}
		return
	}

	elapsed := int(now.Sub(s.startedAt).Seconds())
	remaining := s.remainingAt - elapsed
	s.state.RemainingSeconds = remaining
	s.state.ElapsedSeconds = s.state.DurationSeconds - remaining
	if remaining < 0 {
		s.state.OvertimeSeconds = -remaining
	} else {
		s.state.OvertimeSeconds = 0
	}
}

func prestartRemaining(now time.Time, starts []MeetingStart, prestartSeconds int) (int, string, string, bool) {
	today := int(now.Weekday())
	bestRemaining := 0
	bestLabel := ""
	bestTime := ""
	found := false

	for _, slot := range starts {
		if slot.Day != today {
			continue
		}
		hourMinute, err := parseClockTime(slot.Time)
		if err != nil {
			continue
		}
		start := time.Date(now.Year(), now.Month(), now.Day(), hourMinute.hour, hourMinute.minute, 0, 0, now.Location())
		windowStart := start.Add(-time.Duration(prestartSeconds) * time.Second)
		if now.Before(windowStart) || !now.Before(start) {
			continue
		}
		remaining := int(start.Sub(now).Seconds())
		if !found || remaining < bestRemaining {
			bestRemaining = remaining
			bestLabel = strings.TrimSpace(slot.Congregation)
			bestTime = slot.Time
			found = true
		}
	}

	return bestRemaining, bestLabel, bestTime, found
}
