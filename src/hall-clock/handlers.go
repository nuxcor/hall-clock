package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// maxControlBody bounds the small JSON bodies of the control endpoints. A paired
// phone is trusted to drive the clock, not to make a 512 MB Pi buffer whatever
// it cares to send.
const maxControlBody = 64 << 10

func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v)
}

// scheduleSizeOK refuses a programme longer than any saved one may be. Imports
// are checked too: a megabyte of pasted timings is ten thousand parts, held in
// memory and rebroadcast to every screen, numbered into the ad-hoc range, and
// then impossible to save back from the setup page.
func scheduleSizeOK(w http.ResponseWriter, schedule []Talk) bool {
	if len(schedule) > maxScheduleParts {
		http.Error(w, fmt.Sprintf("a schedule can have at most %d items", maxScheduleParts), http.StatusBadRequest)
		return false
	}
	return true
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.snapshot())
}

func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	// One clock reading for the whole response: two would let MeetingType and
	// Schedule be resolved on opposite sides of an expiry boundary.
	now := s.clock()
	s.mu.Lock()
	out := Config{
		DeviceName:               s.config.DeviceName,
		AdvertisedBaseURL:        s.config.AdvertisedBaseURL,
		MeetingType:              meetingTypeForTime(now),
		MeetingStartTime:         s.config.MeetingStartTime,
		MeetingStarts:            append([]MeetingStart(nil), s.config.MeetingStarts...),
		PrestartSeconds:          s.config.PrestartSeconds,
		MidweekURL:               s.config.MidweekURL,
		MidweekLanguage:          s.config.MidweekLanguage,
		MidweekLanguageSources:   copyStringMap(s.config.MidweekLanguageSources),
		MidweekLanguageSchedules: copyMidweekLanguageScheduleMap(s.config.MidweekLanguageSchedules),
		AutoImportMidweek:        s.config.AutoImportMidweek,
		MidweekImportedWeek:      s.config.MidweekImportedWeek,
		// Load the editor with the program that is actually running, so a save
		// from this form can never post a schedule the clock is not using.
		Schedule: append([]Talk(nil), s.effectiveMidweekScheduleLocked(now)...),
	}
	s.mu.Unlock()
	writeJSON(w, out)
}

func (s *server) handleStart(w http.ResponseWriter, r *http.Request) {
	now := s.clock()
	s.mu.Lock()
	// Reconcile before locking the program in: with no SSE subscriber ticking,
	// a pending between-meetings swap (meeting-type flip, language sync, expired
	// override) would otherwise be skipped and the stale program would run the
	// whole meeting.
	s.recalculateLocked(now)
	if s.state.Status != StatusRunning {
		// A resumed part picks up the fraction of a second it had already used.
		s.startedAt = now.Add(-s.pauseCarry)
		s.pauseCarry = 0
		s.state.Status = StatusRunning
		s.meetingActiveAt = now
	}
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(state)
	writeJSON(w, state)
}

func (s *server) handlePause(w http.ResponseWriter, r *http.Request) {
	now := s.clock()
	s.mu.Lock()
	s.recalculateLocked(now)
	if s.state.Status == StatusRunning {
		s.remainingAt = s.state.RemainingSeconds
		elapsed := now.Sub(s.startedAt)
		s.pauseCarry = elapsed - elapsed.Truncate(time.Second)
		s.state.Status = StatusPaused
	}
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(state)
	writeJSON(w, state)
}

// handleEndMeeting stops the clock and returns it to idle, the way finishing a
// meeting should: the running or paused part is released, the current part's
// clock is reset to full, and the meeting's overtime tally is wiped. Idling is
// what lets the next meeting's prestart countdown appear and the session
// reconciliations run -- none of which happen while a timer is left running, so
// without this the next meeting would start degraded until a part was selected.
func (s *server) handleEndMeeting(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.state.Status = StatusIdle
	s.remainingAt = s.state.DurationSeconds
	s.state.RemainingSeconds = s.state.DurationSeconds
	s.state.ElapsedSeconds = 0
	s.state.OvertimeSeconds = 0
	s.retiredOverruns = nil
	s.meetingActiveAt = time.Time{}
	s.pauseCarry = 0
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(state)
	writeJSON(w, state)
}

// handleReset backs the legacy /api/control/reset route. The "Stop timer"
// button it belonged to is gone — it was Next part with a label that claimed
// otherwise, and pause already lives on the primary button. The route stays as
// an alias for /api/control/next so existing links and scripts keep working.
func (s *server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.changeTalk(w, r, 1)
}

func (s *server) handleNext(w http.ResponseWriter, r *http.Request) {
	s.changeTalk(w, r, 1)
}

func (s *server) handlePrevious(w http.ResponseWriter, r *http.Request) {
	s.changeTalk(w, r, -1)
}

func (s *server) handleAdjust(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeltaSeconds int `json:"deltaSeconds"`
	}
	if err := decodeBody(w, r, maxControlBody, &body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.recalculateLocked(s.clock())
	// Move the part's length and its time left by the same amount, so the time
	// already spoken — their difference — never changes. Clamping one and not
	// the other let two taps of −1 min on a one-minute part put the meeting a
	// minute behind before anyone had spoken. The count carries on from the
	// same start rather than restarting at now, which dropped a fraction of a
	// second on every tap.
	duration := max(60, s.state.DurationSeconds+body.DeltaSeconds)
	delta := duration - s.state.DurationSeconds
	s.state.DurationSeconds = duration
	s.state.RemainingSeconds += delta
	s.state.OvertimeSeconds = max(0, -s.state.RemainingSeconds)
	s.remainingAt += delta
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(state)
	writeJSON(w, state)
}

func (s *server) handleSetTime(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seconds int `json:"seconds"`
	}
	if err := decodeBody(w, r, maxControlBody, &body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	seconds := clamp(body.Seconds, 60, 7200)

	s.mu.Lock()
	if s.state.Status != StatusIdle {
		s.mu.Unlock()
		http.Error(w, "time can only be edited while idle", http.StatusConflict)
		return
	}
	s.state.DurationSeconds = seconds
	s.state.RemainingSeconds = seconds
	s.state.ElapsedSeconds = 0
	s.state.OvertimeSeconds = 0
	s.remainingAt = seconds
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(state)
	writeJSON(w, state)
}

func (s *server) handleSelect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TalkID int `json:"talkId"`
		// FromTalkID, when sent, is the item the phone believed was current,
		// as with changeTalk. Reset part re-selects "the current item", and on a
		// phone whose stream has fallen behind that can be one the clock has
		// already left: restarting it would drag the meeting back a part.
		FromTalkID int `json:"fromTalkId"`
	}
	if err := decodeBody(w, r, maxControlBody, &body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	now := s.clock()
	s.mu.Lock()
	s.recalculateLocked(now)
	if body.FromTalkID != 0 && body.FromTalkID != s.state.CurrentTalkID {
		s.mu.Unlock()
		http.Error(w, "the clock has already moved on", http.StatusPreconditionFailed)
		return
	}
	// Re-selecting the current part is a restart, not a departure, and a request
	// for a part that does not exist leaves nothing behind.
	if body.TalkID != s.state.CurrentTalkID && s.hasTalkLocked(body.TalkID) {
		s.retireCurrentPartLocked(now)
	}
	ok := s.selectTalkLocked(body.TalkID)
	state := s.snapshotLocked()
	s.mu.Unlock()

	if !ok {
		http.Error(w, "talk not found", http.StatusNotFound)
		return
	}

	s.broadcast(state)
	writeJSON(w, state)
}

func (s *server) handleAdhocPart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title   string `json:"title"`
		Seconds int    `json:"seconds"`
		// AfterTalkID is where the operator chose to put the item: 0 means
		// the start of the meeting, a talk id means right after that item.
		// Absent falls back to after the current item, which keeps an older
		// controller page working across an update.
		AfterTalkID *int `json:"afterTalkId"`
	}
	if err := decodeBody(w, r, maxControlBody, &body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	title := truncateRunes(strings.TrimSpace(body.Title), maxTitleRunes)
	if title == "" {
		title = "Additional item"
	}
	seconds := clamp(body.Seconds, 60, 7200)

	s.mu.Lock()
	insertAt := len(s.talks)
	switch {
	case body.AfterTalkID == nil:
		for i, talk := range s.talks {
			if talk.ID == s.state.CurrentTalkID {
				insertAt = i + 1
				break
			}
		}
	case *body.AfterTalkID == 0:
		insertAt = 0
	default:
		// An id that vanished between the broadcast and the tap lands the
		// item at the end rather than failing a request whose form the
		// operator has already dismissed.
		for i, talk := range s.talks {
			if talk.ID == *body.AfterTalkID {
				insertAt = i + 1
				break
			}
		}
	}
	// Ad-hoc ids come from their own range, above anything a saved programme
	// can use, so a later save or import can never add a part with the same id.
	nextID := temporaryPartIDBase
	for _, talk := range s.talks {
		nextID = max(nextID, talk.ID+1)
	}
	part := Talk{ID: nextID, Title: title, Duration: seconds, Closing: min(60, seconds), Temporary: true, CreatedAt: s.clock()}
	// A fresh slice, never an in-place shift: s.talks can share its backing
	// array with the saved programme (the boot path hands the baseline over
	// as-is), and shifting in place wrote the temporary part into
	// config.Schedule — which the idle reconciler then re-merged, one more
	// copy per tick, until the picker was a wall of duplicates.
	talks := make([]Talk, 0, len(s.talks)+1)
	talks = append(talks, s.talks[:insertAt]...)
	talks = append(talks, part)
	talks = append(talks, s.talks[insertAt:]...)
	s.talks = talks
	s.state.Schedule = s.talks
	// The operator said where the item goes; jumping the clock's selection
	// there as well made adding a later item hijack what was lined up. Only
	// a schedule with nothing selected adopts the new item.
	if s.state.Status == StatusIdle && s.state.CurrentTalkID == 0 {
		s.selectTalkLocked(part.ID)
	}
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(state)
	writeJSON(w, state)
}

func (s *server) handleRemovePart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TalkID int `json:"talkId"`
	}
	if err := decodeBody(w, r, maxControlBody, &body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	idx := -1
	for i, talk := range s.talks {
		if talk.ID == body.TalkID {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		http.Error(w, "talk not found", http.StatusNotFound)
		return
	}
	// The saved programme is edited in /setup; only the ad-hoc additions are
	// disposable from the controller.
	if !s.talks[idx].Temporary {
		s.mu.Unlock()
		http.Error(w, "only temporary items can be removed here", http.StatusConflict)
		return
	}
	if s.talks[idx].ID == s.state.CurrentTalkID && s.state.Status != StatusIdle {
		s.mu.Unlock()
		http.Error(w, "cannot remove the item on the clock", http.StatusConflict)
		return
	}
	removedCurrent := s.talks[idx].ID == s.state.CurrentTalkID
	// Fresh slice for the same reason as in handleAdhocPart: an in-place
	// shift would rewrite a backing array possibly shared with the saved
	// programme.
	talks := make([]Talk, 0, len(s.talks)-1)
	talks = append(talks, s.talks[:idx]...)
	talks = append(talks, s.talks[idx+1:]...)
	s.talks = talks
	s.state.Schedule = s.talks
	if removedCurrent && len(s.talks) > 0 {
		// The item the clock pointed at is gone; point at the one that now
		// holds its slot in the programme (or the new last item).
		s.selectTalkLocked(s.talks[min(idx, len(s.talks)-1)].ID)
	}
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(state)
	writeJSON(w, state)
}

func (s *server) handleCircuitOverseer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if err := decodeBody(w, r, maxControlBody, &body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	now := s.clock()
	s.mu.Lock()
	// CO mode reshapes the whole schedule, so only allow it while idle — a
	// running/paused meeting would flip the flag without cleanly rebuilding.
	if s.state.Status != StatusIdle {
		s.mu.Unlock()
		http.Error(w, "circuit overseer mode can only be changed while idle", http.StatusConflict)
		return
	}
	// Turning it on sets a 3-hour expiry so it applies to this meeting session
	// only; turning it off clears it.
	if body.On {
		s.config.CircuitOverseerExpiresAt = now.Add(circuitOverseerDuration)
	} else {
		s.config.CircuitOverseerExpiresAt = time.Time{}
	}
	s.state.CircuitOverseer = circuitOverseerActive(s.config.CircuitOverseerExpiresAt, now)
	// Rebuild the active schedule for the new mode (swaps CO parts in/out).
	s.applyActiveScheduleChangeLocked(now)
	state := s.snapshotLocked()
	s.mu.Unlock()

	if err := s.persistConfig(); err != nil {
		http.Error(w, "could not save config", http.StatusInternalServerError)
		return
	}

	s.broadcast(state)
	writeJSON(w, state)
}

func (s *server) handleMidweekLanguage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Language string `json:"language"`
	}
	if err := decodeBody(w, r, maxControlBody, &body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	language := normalizeMidweekLanguage(body.Language)
	if language == "" {
		http.Error(w, "unsupported language", http.StatusBadRequest)
		return
	}

	now := s.clock()
	s.mu.Lock()
	// The weekend programme is a local template, so its language switch needs
	// no workbook at all. Routing it through the midweek path made a Sunday
	// switch depend on WOL being reachable.
	weekend := meetingTypeForTime(now) == "weekend"
	var state State
	var ok bool
	var message string
	if weekend {
		_, state, ok, message = s.applyWeekendLanguageLocked(now, language)
	} else {
		_, state, ok, message = s.applyCachedMidweekLanguageScheduleLocked(now, language)
	}
	s.mu.Unlock()
	if !weekend && !ok && strings.Contains(message, "not imported for this week yet") {
		_, state, ok, message = s.importMidweekLanguage(r.Context(), now, language)
	}
	if !ok {
		http.Error(w, message, http.StatusConflict)
		return
	}

	if err := s.persistConfig(); err != nil {
		http.Error(w, "could not save language schedule", http.StatusInternalServerError)
		return
	}

	s.broadcast(state)
	writeJSON(w, state)
}

func (s *server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var body Config
	// Any phone that has paired can post here, and whatever it saves is held in
	// memory and rebroadcast to every screen four times a second: never parse
	// an unbounded body, or keep an unbounded setting, on a 512 MB Pi.
	if err := decodeBody(w, r, 1<<20, &body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(body.Schedule) == 0 {
		http.Error(w, "schedule cannot be empty", http.StatusBadRequest)
		return
	}
	if !scheduleSizeOK(w, body.Schedule) {
		return
	}
	if len(body.MeetingStarts) > maxMeetingStarts {
		http.Error(w, fmt.Sprintf("at most %d meeting start times", maxMeetingStarts), http.StatusBadRequest)
		return
	}
	if len(body.MidweekURL) > maxURLLength || len(body.AdvertisedBaseURL) > maxURLLength {
		http.Error(w, "URL is too long", http.StatusBadRequest)
		return
	}
	for _, start := range body.MeetingStarts {
		if len(start.MidweekURL) > maxURLLength {
			http.Error(w, "URL is too long", http.StatusBadRequest)
			return
		}
	}

	body.DeviceName = truncateRunes(strings.TrimSpace(body.DeviceName), maxNameRunes)
	if body.DeviceName == "" {
		body.DeviceName = "Hall Clock"
	}
	body.MeetingType = normalizeMeetingType(body.MeetingType)
	body.MeetingStartTime = normalizeStartTime(body.MeetingStartTime)
	advertisedURL, err := normalizeAdvertisedControlURL(body.AdvertisedBaseURL)
	if err != nil {
		http.Error(w, "invalid advertisedBaseUrl", http.StatusBadRequest)
		return
	}
	if body.PrestartSeconds == 0 {
		body.PrestartSeconds = 300
	}
	body.PrestartSeconds = clamp(body.PrestartSeconds, 60, 1800)

	s.mu.Lock()
	// The setup page sends back the ids it loaded, and its new parts are numbered
	// above every id the clock holds. A schedule with no ids at all comes from a
	// script or an older page; numbering it by position, as before, is the best
	// guess there is.
	floor := 0
	for _, talk := range body.Schedule {
		if validPartID(talk.ID) {
			floor = s.highestPartIDLocked()
			break
		}
	}
	normalizeScheduleAbove(body.Schedule, floor)
	// Read under the lock: the auto-import goroutine writes this map, and an
	// unsynchronized iteration of it aborts the process.
	existingLanguageSchedules := copyMidweekLanguageScheduleMap(s.config.MidweekLanguageSchedules)
	s.config.DeviceName = body.DeviceName
	s.config.AdvertisedBaseURL = advertisedURL
	s.config.MeetingType = body.MeetingType
	s.config.MeetingStartTime = body.MeetingStartTime
	body.MeetingStarts = normalizeMeetingStarts(body.MeetingStarts, body.MeetingStartTime)
	applyDefaultMeetingStartLanguage(body.MeetingStarts, s.config.MidweekLanguage, s.config.MidweekURL)
	s.config.MeetingStarts = preserveMeetingStartImportState(s.config.MeetingStarts, body.MeetingStarts)
	s.config.PrestartSeconds = body.PrestartSeconds
	s.config.MidweekURL = strings.TrimSpace(body.MidweekURL)
	if language := wolLanguage(s.config.MidweekURL); language != "" {
		s.config.MidweekLanguage = language
		if s.config.MidweekLanguageSources == nil {
			s.config.MidweekLanguageSources = map[string]string{}
		}
		s.config.MidweekLanguageSources[language] = s.config.MidweekURL
	}
	s.config.AutoImportMidweek = body.AutoImportMidweek
	// The closing bell belongs to the import, not to whoever posted this request.
	// Restore it against the baseline before anything compares schedules, so a
	// client that sent a stale or invented bell cannot make an unchanged program
	// look edited.
	applyImportedClosingSeconds(body.Schedule, s.config.Schedule)
	// A hand-edited schedule is an override scoped to this meeting session, not a
	// new baseline: it expires so the next congregation on a shared box starts
	// from the imported program. Saving the baseline back clears the override,
	// and re-saving an unchanged edit keeps its original expiry instead of
	// silently extending it (an unrelated setting change must not buy 3 more hours).
	saveNow := s.clock()
	switch {
	case sameSchedule(body.Schedule, s.config.Schedule):
		s.config.ScheduleOverride = nil
		s.config.ScheduleOverrideExpiresAt = time.Time{}
	case s.scheduleOverrideAppliesLocked(saveNow) && sameSchedule(body.Schedule, s.config.ScheduleOverride):
		// Unchanged edit: leave ScheduleOverrideExpiresAt alone. Testing "applies"
		// rather than "window still open" is what stops a stale browser tab from
		// re-arming an already-expired edit for another three hours.
	default:
		s.config.ScheduleOverride = body.Schedule
		s.config.ScheduleOverrideExpiresAt = saveNow.Add(scheduleOverrideDuration)
	}
	// Client-posted caches are untrusted: keep only known languages with sane
	// schedules, and fall back to the server's own caches rather than letting a
	// stale setup tab clobber what this week's import just wrote.
	if sanitized := sanitizeMidweekLanguageSchedules(body.MidweekLanguageSchedules); sanitized != nil {
		s.config.MidweekLanguageSchedules = sanitized
	} else {
		s.config.MidweekLanguageSchedules = existingLanguageSchedules
	}
	s.state.DeviceName = body.DeviceName
	s.state.MeetingStartTime = body.MeetingStartTime
	s.state.MeetingStarts = body.MeetingStarts
	s.state.PrestartLabel = ""
	s.state.PrestartSeconds = body.PrestartSeconds
	s.applyActiveScheduleChangeLocked(s.clock())
	state := s.snapshotLocked()
	s.mu.Unlock()

	if err := s.persistConfig(); err != nil {
		http.Error(w, "could not save config", http.StatusInternalServerError)
		return
	}

	s.broadcast(state)
	writeJSON(w, state)

	now := s.clock()
	s.mu.Lock()
	_, due := s.nextAutoImportSourceLocked(now)
	// An import installs a new baseline and drops the override. Never let the
	// save that just created an override be the thing that triggers the import
	// which discards it: the operator would watch their edit revert seconds
	// after saving. The weekly Monday import still runs on its own schedule.
	if s.scheduleOverrideAppliesLocked(now) {
		due = false
	}
	s.mu.Unlock()
	if due {
		go s.autoImportTick(context.Background(), now)
	}
}

// sanitizeMidweekLanguageSchedules filters client-posted language caches down
// to known languages with plausible schedules, normalized the same way the
// server's own import path normalizes them. Returns nil when nothing survives,
// so the caller keeps the server's caches instead.
func sanitizeMidweekLanguageSchedules(in map[string]MidweekLanguageSchedule) map[string]MidweekLanguageSchedule {
	out := map[string]MidweekLanguageSchedule{}
	for key, entry := range in {
		language := normalizeMidweekLanguage(key)
		if language == "" || entry.ImportedWeek == "" || len(entry.Schedule) == 0 || len(entry.Schedule) > 100 {
			continue
		}
		schedule := make([]Talk, len(entry.Schedule))
		copy(schedule, entry.Schedule)
		normalizeSchedule(schedule)
		out[language] = MidweekLanguageSchedule{
			ImportedWeek: strings.TrimSpace(entry.ImportedWeek),
			URL:          strings.TrimSpace(entry.URL),
			Schedule:     schedule,
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func copyMidweekLanguageScheduleMap(in map[string]MidweekLanguageSchedule) map[string]MidweekLanguageSchedule {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]MidweekLanguageSchedule, len(in))
	for key, value := range in {
		value.Schedule = append([]Talk(nil), value.Schedule...)
		out[key] = value
	}
	return out
}

func preserveMeetingStartImportState(existing, incoming []MeetingStart) []MeetingStart {
	byID := map[int]MeetingStart{}
	for _, start := range existing {
		if start.ID != 0 {
			byID[start.ID] = start
		}
	}
	for i := range incoming {
		previous, ok := byID[incoming[i].ID]
		if !ok || incoming[i].MidweekImportedWeek != "" {
			continue
		}
		incomingLanguage := meetingStartLanguage(incoming[i])
		previousLanguage := meetingStartLanguage(previous)
		if incoming[i].MidweekURL == "" && incomingLanguage != "" && incomingLanguage == previousLanguage {
			incoming[i].MidweekURL = previous.MidweekURL
		}
		if incoming[i].MidweekURL != previous.MidweekURL || incomingLanguage != previousLanguage {
			continue
		}
		incoming[i].MidweekImportedWeek = previous.MidweekImportedWeek
	}
	return incoming
}

func applyDefaultMeetingStartLanguage(starts []MeetingStart, configLanguage, configURL string) {
	fallback := normalizeMidweekLanguage(configLanguage)
	if fallback == "" {
		fallback = normalizeMidweekLanguage(wolLanguage(configURL))
	}
	if fallback == "" {
		return
	}
	for i := range starts {
		if meetingStartLanguage(starts[i]) == "" {
			starts[i].Language = fallback
		}
	}
}

func (s *server) handleImportMidweek(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL   string `json:"url"`
		Apply bool   `json:"apply"`
	}
	if err := decodeBody(w, r, maxControlBody, &body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	sourceURL := strings.TrimSpace(body.URL)
	if sourceURL == "" {
		http.Error(w, "midweek URL is required", http.StatusBadRequest)
		return
	}

	schedule, err := importMidweekFromURL(r.Context(), sourceURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !scheduleSizeOK(w, schedule) {
		return
	}

	if !body.Apply {
		writeJSON(w, map[string]any{
			"meetingType": "midweek",
			"sourceUrl":   sourceURL,
			"schedule":    schedule,
		})
		return
	}

	s.mu.Lock()
	s.config.MeetingType = "midweek"
	s.config.MidweekURL = sourceURL
	if language := wolLanguage(sourceURL); language != "" {
		s.config.MidweekLanguage = language
		if s.config.MidweekLanguageSources == nil {
			s.config.MidweekLanguageSources = map[string]string{}
		}
		s.config.MidweekLanguageSources[language] = sourceURL
	} else {
		// A URL whose language cannot be read still imports fine, but the
		// previous language must not claim its schedule: the per-language
		// cache store below files under MidweekLanguage, and a stale value
		// would label these items as a language they are not.
		s.config.MidweekLanguage = ""
	}
	importedWeek := isoWeekString(s.clock())
	s.config.MidweekImportedWeek = importedWeek
	s.setBaselineScheduleLocked(schedule)
	s.storeMidweekLanguageScheduleLocked(s.config.MidweekLanguage, importedWeek, sourceURL, schedule)
	s.applyActiveScheduleChangeLocked(s.clock())
	config := s.config
	state := s.snapshotLocked()
	s.mu.Unlock()

	if err := s.persistConfig(); err != nil {
		http.Error(w, "could not save imported schedule", http.StatusInternalServerError)
		return
	}

	s.broadcast(state)
	writeJSON(w, setupResponse(state, config, s.clock()))
}

func (s *server) handleImportMidweekText(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text  string `json:"text"`
		Apply bool   `json:"apply"`
	}
	if err := decodeBody(w, r, 1<<20, &body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	schedule, err := parseMidweekTimings(body.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !scheduleSizeOK(w, schedule) {
		return
	}

	if !body.Apply {
		writeJSON(w, map[string]any{
			"meetingType": "midweek",
			"schedule":    schedule,
		})
		return
	}

	s.mu.Lock()
	s.config.MeetingType = "midweek"
	s.config.MidweekLanguage = ""
	// The active baseline no longer comes from a URL; a leftover MidweekURL
	// stamped with this week would make the docid staleness guard compare
	// language caches against a document that is not what is actually applied.
	s.config.MidweekURL = ""
	s.config.MidweekImportedWeek = isoWeekString(s.clock())
	s.setBaselineScheduleLocked(schedule)
	s.applyActiveScheduleChangeLocked(s.clock())
	config := s.config
	state := s.snapshotLocked()
	s.mu.Unlock()

	if err := s.persistConfig(); err != nil {
		http.Error(w, "could not save imported schedule", http.StatusInternalServerError)
		return
	}

	s.broadcast(state)
	writeJSON(w, setupResponse(state, config, s.clock()))
}

// setupResponse reshapes a state snapshot for the setup page: it carries the
// saved (editable) schedule and meeting type instead of the runtime ones,
// which on weekends resolve to the fixed weekend template. Without this, the
// setup editor would load the weekend parts and a subsequent Save would
// overwrite the saved midweek schedule with them.
func setupResponse(state State, config Config, now time.Time) State {
	state.MeetingType = config.MeetingType
	// Resolve against the status in the snapshot, not a fresh read, so the setup
	// page and the broadcast state always agree about which program is running.
	state.Schedule = append([]Talk(nil), effectiveMidweekSchedule(config, state.MeetingInProgress, now)...)
	return state
}

func (s *server) handleWeekendTemplate(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	language := s.config.MidweekLanguage
	s.mu.Unlock()
	s.applyTemplate(w, "weekend", weekendSchedule(language))
}

func (s *server) handleMidweekTemplate(w http.ResponseWriter, r *http.Request) {
	s.applyTemplate(w, "midweek", defaultSchedule())
}

func (s *server) applyTemplate(w http.ResponseWriter, meetingType string, schedule []Talk) {
	normalizeSchedule(schedule)

	s.mu.Lock()
	s.config.MeetingType = meetingType
	if meetingType == "weekend" && !hasWeekendStart(s.config.MeetingStarts) {
		starts := append(s.config.MeetingStarts, MeetingStart{Day: int(time.Sunday), Time: "10:00"})
		s.config.MeetingStarts = normalizeMeetingStarts(starts, s.config.MeetingStartTime)
		s.state.MeetingStarts = s.config.MeetingStarts
	}
	if meetingType == "midweek" {
		s.setBaselineScheduleLocked(schedule)
	}
	s.applyActiveScheduleChangeLocked(s.clock())
	config := s.config
	state := s.snapshotLocked()
	s.mu.Unlock()

	if err := s.persistConfig(); err != nil {
		http.Error(w, "could not save template", http.StatusInternalServerError)
		return
	}

	s.broadcast(state)
	writeJSON(w, setupResponse(state, config, s.clock()))
}

func (s *server) changeTalk(w http.ResponseWriter, r *http.Request, delta int) {
	// fromTalkId is the item the phone was showing when the operator tapped.
	// Advancing is not idempotent, so a retry after a timeout that had in fact
	// landed, or a second tap queued behind the first, must not move the meeting
	// on twice. Absent — an older page, a script — keeps the old behaviour, and
	// so does a body that is not JSON at all: these routes never read one
	// before, and a script posting `-d go` must not start failing.
	var body struct {
		FromTalkID int `json:"fromTalkId"`
	}
	if err := decodeBody(w, r, maxControlBody, &body); err != nil {
		body.FromTalkID = 0
	}

	now := s.clock()
	s.mu.Lock()
	// Recalculate before reading s.talks, never after: it purges stale ad-hoc
	// parts and can swap the whole schedule, so an index taken beforehand may not
	// survive it.
	s.recalculateLocked(now)
	if body.FromTalkID != 0 && body.FromTalkID != s.state.CurrentTalkID {
		s.mu.Unlock()
		http.Error(w, "the clock has already moved on", http.StatusPreconditionFailed)
		return
	}
	idx := 0
	for i, talk := range s.talks {
		if talk.ID == s.state.CurrentTalkID {
			idx = i
			break
		}
	}
	// A meeting is a list, not a loop. Advancing past the last item used to wrap
	// silently back to the opening comments, which is never what an operator
	// means at the end of the program.
	next := idx + delta
	if next < 0 || next >= len(s.talks) {
		s.mu.Unlock()
		http.Error(w, "no further item in the schedule", http.StatusConflict)
		return
	}
	// The operator is leaving this part, so whatever it ran over is the meeting's.
	s.retireCurrentPartLocked(now)
	s.selectTalkLocked(s.talks[next].ID)
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(state)
	writeJSON(w, state)
}
