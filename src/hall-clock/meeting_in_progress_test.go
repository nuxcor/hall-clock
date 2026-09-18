package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// postStatus posts like h.post but hands back the status instead of failing on
// anything other than 200.
func (h *overrideHarness) postStatus(path, body string) int {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("X-Wall-Clock-Token", h.srv.config.ControlToken)
	res := httptest.NewRecorder()
	h.mux.ServeHTTP(res, req)
	return res.Code
}

// saveSchedule posts the setup form with this programme, ids and all, the way
// the setup page sends back what it loaded.
func (h *overrideHarness) saveSchedule(schedule []Talk) State {
	h.t.Helper()
	body, err := json.Marshal(Config{
		DeviceName:       "Hall Clock",
		MeetingType:      "midweek",
		MeetingStartTime: "19:00",
		MeetingStarts:    h.srv.config.MeetingStarts,
		PrestartSeconds:  300,
		Schedule:         schedule,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.post("/api/config", string(body))
}

func (h *overrideHarness) at(hour, minute int) {
	h.now = time.Date(h.now.Year(), h.now.Month(), h.now.Day(), hour, minute, 0, 0, time.UTC)
}

// Every Next leaves the clock idle, so idle never meant "between meetings".
func TestMeetingInProgressSpansIdleBetweenParts(t *testing.T) {
	h := newOverrideHarness(t)
	if h.state().MeetingInProgress {
		t.Fatal("no meeting has started yet")
	}

	h.post("/api/control/start", "")
	h.advance(time.Minute)
	h.post("/api/control/next", "")
	between := h.state()
	if between.Status != StatusIdle || !between.MeetingInProgress {
		t.Fatalf("idle between two parts is still a meeting, got status=%s inProgress=%v", between.Status, between.MeetingInProgress)
	}

	// A song and a prayer later, still the same meeting.
	h.advance(10 * time.Minute)
	if !h.state().MeetingInProgress {
		t.Fatal("a ten-minute gap between parts ended the meeting")
	}

	// Half an hour with nothing on the clock is not a meeting any more.
	h.advance(meetingIdleGap)
	if h.state().MeetingInProgress {
		t.Fatal("a clock left idle for half an hour still claims a meeting")
	}
}

func TestEndMeetingEndsTheMeeting(t *testing.T) {
	h := newOverrideHarness(t)
	h.post("/api/control/start", "")
	h.advance(time.Minute)
	h.post("/api/control/next", "")
	if ended := h.post("/api/control/end", ""); ended.MeetingInProgress {
		t.Fatal("End meeting left a meeting in progress")
	}
}

// A shared hall: the first congregation never presses End, and the second's
// session begins with its prestart countdown at 18:55.
func TestNextSessionEndsAMeetingLeftIdle(t *testing.T) {
	h := newOverrideHarness(t)
	h.srv.mu.Lock()
	h.srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{
		{Day: int(time.Thursday), Time: "17:00"},
		{Day: int(time.Thursday), Time: "19:00"},
	}, "19:00")
	h.srv.mu.Unlock()

	// Finished at a quarter to and left idle: over the moment the countdown opens.
	h.at(17, 0)
	h.post("/api/control/start", "")
	h.at(18, 45)
	h.post("/api/control/next", "")
	h.at(18, 54)
	if !h.state().MeetingInProgress {
		t.Fatal("the first meeting ended before the second one's session")
	}
	h.at(18, 55)
	if h.state().MeetingInProgress {
		t.Fatal("the next congregation's countdown began inside the last meeting")
	}
}

// Running late, idle for a song between parts as the countdown opens: still the
// same meeting, in its own language, until it has really stopped.
func TestLateMeetingKeepsItsLanguageAcrossTheNextCountdown(t *testing.T) {
	h := newOverrideHarness(t)
	h.srv.mu.Lock()
	h.srv.config.MidweekLanguage = "en"
	h.srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{
		{Day: int(time.Thursday), Time: "17:00", Language: "en"},
		{Day: int(time.Thursday), Time: "19:00", Language: "es"},
	}, "19:00")
	h.srv.config.MidweekLanguageSchedules = map[string]MidweekLanguageSchedule{
		"es": {
			ImportedWeek: isoWeekString(h.now),
			URL:          "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
			Schedule:     []Talk{{ID: 1, Title: "Comentarios de introducción", Duration: 60, Closing: 30}},
		},
	}
	h.srv.mu.Unlock()

	h.at(17, 0)
	h.post("/api/control/start", "")
	h.runPartOver(45 * time.Second)
	h.at(18, 53)
	h.post("/api/control/next", "")
	h.at(18, 56)
	late := h.state()
	if !late.MeetingInProgress || late.MidweekLanguage != "en" || late.MeetingOvertimeSeconds == 0 {
		t.Fatalf("a late meeting was handed over mid-song: inProgress=%v language=%q behind=%ds",
			late.MeetingInProgress, late.MidweekLanguage, late.MeetingOvertimeSeconds)
	}

	// Ten minutes with nothing on the clock, inside the next session: it is over.
	h.at(19, 3)
	if got := h.state(); got.MeetingInProgress || got.MidweekLanguage != "es" {
		t.Fatalf("expected the handover after ten idle minutes: inProgress=%v language=%q", got.MeetingInProgress, got.MidweekLanguage)
	}
}

// An import that lands between two parts used to swap the program there, on a
// day with no start configured (so outside the import's own suppression).
func TestAutoImportWaitsForTheMeetingToEnd(t *testing.T) {
	h := newOverrideHarness(t)
	h.srv.mu.Lock()
	h.srv.config.AutoImportMidweek = true
	h.srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{{Day: int(time.Monday), Time: "19:00"}}, "19:00")
	h.srv.mu.Unlock()

	h.post("/api/control/start", "")
	h.advance(time.Minute)
	h.post("/api/control/next", "")
	lined := h.state().CurrentTalkTitle

	imported := []Talk{{ID: 1, Title: "Opening Comments", Duration: 60}, {ID: 2, Title: "Other Treasures", Duration: 900}}
	h.srv.mu.Lock()
	source, due := h.srv.nextAutoImportSourceLocked(h.now)
	var ok bool
	if due {
		_, _, ok = h.srv.applyAutoImportedScheduleLocked(h.now, source, "https://wol.jw.org/en/wol/d/r1/lp-e/202026241", imported)
	}
	h.srv.mu.Unlock()
	if !ok {
		t.Fatalf("setup: expected the import to apply (due=%v)", due)
	}
	if got := h.state().CurrentTalkTitle; got != lined {
		t.Fatalf("an import swapped the program between parts: %q is now %q", lined, got)
	}

	h.post("/api/control/end", "")
	if got := h.state().Schedule[1].Title; got != "Other Treasures" {
		t.Fatalf("expected the imported program once the meeting ended, got %q", got)
	}
}

// Numbering new parts from the schedule alone gave a part added in the same
// save as a deletion the deleted part's id — and its timer.
func TestNewPartNeverReusesADeletedPartsID(t *testing.T) {
	h := newOverrideHarness(t)
	last := h.srv.config.Schedule[len(h.srv.config.Schedule)-1]
	h.selectPart(last.ID)
	h.post("/api/control/start", "")

	programme := append([]Talk(nil), h.srv.config.Schedule[:len(h.srv.config.Schedule)-1]...)
	programme = append(programme, Talk{Title: "Added in setup", Duration: 300})
	state := h.saveSchedule(programme)

	added := state.Schedule[len(state.Schedule)-1]
	if added.ID == last.ID {
		t.Fatalf("the new part took the deleted part's id %d", last.ID)
	}
	if code := h.postStatus("/api/control/next", fmt.Sprintf(`{"fromTalkId":%d}`, added.ID)); code != http.StatusPreconditionFailed {
		t.Fatalf("a tap naming the new part was taken for the deleted one: %d", code)
	}
}

// These routes never read a body before, and a script posting one must not
// start failing.
func TestAdvanceIgnoresABodyThatIsNotJSON(t *testing.T) {
	h := newOverrideHarness(t)
	first := h.state().CurrentTalkID
	if code := h.postStatus("/api/control/next", "go"); code != http.StatusOK {
		t.Fatalf("a non-JSON body broke Next: %d", code)
	}
	if h.state().CurrentTalkID == first {
		t.Fatal("Next did not advance")
	}
}

func TestPastedTimingsAreBounded(t *testing.T) {
	h := newOverrideHarness(t)
	var text strings.Builder
	for i := range maxScheduleParts + 1 {
		fmt.Fprintf(&text, "Item %d (1 min.)\n", i)
	}
	body, _ := json.Marshal(map[string]any{"text": text.String(), "apply": true})
	if code := h.postStatus("/api/import/midweek-text", string(body)); code != http.StatusBadRequest {
		t.Fatalf("a %d-item paste was accepted: %d", maxScheduleParts+1, code)
	}
}

func TestMeetingStartImportedWeekMustBeAWeek(t *testing.T) {
	starts := normalizeMeetingStarts([]MeetingStart{
		{Day: 4, Time: "19:00", MidweekImportedWeek: "2026-W38"},
		{Day: 5, Time: "19:00", MidweekImportedWeek: strings.Repeat("x", 20000)},
	}, "19:00")
	if starts[0].MidweekImportedWeek != "2026-W38" || starts[1].MidweekImportedWeek != "" {
		t.Fatalf("got %q and %d bytes", starts[0].MidweekImportedWeek, len(starts[1].MidweekImportedWeek))
	}
}

// Turned on well before the meeting, CO mode outlasts its window in the middle.
// It used to lapse at the first part change after that, swapping the service
// talk out of the programme halfway through the visit.
func TestCircuitOverseerModeHoldsForTheWholeMeeting(t *testing.T) {
	h := newOverrideHarness(t)
	h.at(16, 45)
	h.post("/api/control/circuit-overseer", `{"on":true}`)

	h.at(19, 0)
	h.post("/api/control/start", "")
	h.at(19, 50) // CO window closed at 19:45
	h.post("/api/control/next", "")

	between := h.state()
	if !between.CircuitOverseer || !hasTitle(between.Schedule, "Service Talk") {
		t.Fatalf("CO mode lapsed between two parts: co=%v schedule=%+v", between.CircuitOverseer, between.Schedule)
	}

	ended := h.post("/api/control/end", "")
	if ended.CircuitOverseer || hasTitle(ended.Schedule, "Service Talk") {
		t.Fatalf("CO mode should lapse once the meeting is over: co=%v", ended.CircuitOverseer)
	}
}

// Two congregations two hours apart: the second one's 30-minute language lead
// opens before the first one's closing parts.
func TestLanguageFollowsTheNextCongregationOnlyAfterTheMeeting(t *testing.T) {
	h := newOverrideHarness(t)
	h.srv.mu.Lock()
	h.srv.config.MidweekLanguage = "en"
	h.srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{
		{Day: int(time.Thursday), Time: "17:00", Language: "en"},
		{Day: int(time.Thursday), Time: "19:00", Language: "es"},
	}, "19:00")
	h.srv.config.MidweekLanguageSchedules = map[string]MidweekLanguageSchedule{
		"es": {
			ImportedWeek: isoWeekString(h.now),
			URL:          "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
			Schedule:     []Talk{{ID: 1, Title: "Comentarios de introducción", Duration: 60, Closing: 30}},
		},
	}
	h.srv.mu.Unlock()

	h.at(17, 0)
	h.post("/api/control/start", "")
	h.at(18, 41)
	h.post("/api/control/next", "")
	if got := h.state().MidweekLanguage; got != "en" {
		t.Fatalf("the language switched between two parts of the English meeting, got %q", got)
	}

	h.post("/api/control/end", "")
	if got := h.state().MidweekLanguage; got != "es" {
		t.Fatalf("expected the next congregation's language once the meeting ended, got %q", got)
	}
}

// The setup page sends back the ids it loaded. Removing an earlier part used to
// renumber everything after it, so the running part took on its neighbour's
// title and time.
func TestEditingAnEarlierPartLeavesTheRunningPartAlone(t *testing.T) {
	h := newOverrideHarness(t)
	h.selectPart(5)
	h.post("/api/control/start", "")
	h.advance(100 * time.Second)
	before := h.state()

	programme := append([]Talk(nil), h.srv.config.Schedule...)
	programme = append(programme[:2], programme[3:]...) // drop part 3
	h.saveSchedule(programme)

	after := h.state()
	if after.Status != StatusRunning || after.CurrentTalkTitle != before.CurrentTalkTitle || after.RemainingSeconds != before.RemainingSeconds {
		t.Fatalf("editing an earlier part disturbed the running one: before %q %ds, after %q %ds (%s)",
			before.CurrentTalkTitle, before.RemainingSeconds, after.CurrentTalkTitle, after.RemainingSeconds, after.Status)
	}
}

// With the last part on, the same edit left its id pointing at nothing, and the
// clock stopped and went back to the opening comments.
func TestEditingDuringTheLastPartKeepsItRunning(t *testing.T) {
	h := newOverrideHarness(t)
	last := h.srv.config.Schedule[len(h.srv.config.Schedule)-1]
	h.selectPart(last.ID)
	h.post("/api/control/start", "")
	h.advance(30 * time.Second)

	programme := append([]Talk(nil), h.srv.config.Schedule...)
	programme = append(programme[:2], programme[3:]...)
	h.saveSchedule(programme)

	after := h.state()
	if after.Status != StatusRunning || after.CurrentTalkID != last.ID || after.CurrentTalkTitle != last.Title {
		t.Fatalf("the last part was stopped by an edit: %s on %q", after.Status, after.CurrentTalkTitle)
	}
}

// An ad-hoc part took the highest id plus one — the same id the next part a
// save added would take. Next then bounced between the two all meeting.
func TestAdhocPartNeverSharesAnIDWithTheProgramme(t *testing.T) {
	h := newOverrideHarness(t)
	h.post("/api/control/adhoc-part", `{"title":"Announcements","seconds":120}`)

	programme := append([]Talk(nil), h.srv.config.Schedule...)
	programme = append(programme, Talk{Title: "Added in setup", Duration: 300})
	state := h.saveSchedule(programme)

	seen := map[int]string{}
	for _, talk := range state.Schedule {
		if other, dup := seen[talk.ID]; dup {
			t.Fatalf("%q and %q share id %d", other, talk.Title, talk.ID)
		}
		seen[talk.ID] = talk.Title
	}

	// Next walks the whole list once, never landing on an item twice.
	h.selectPart(state.Schedule[0].ID)
	visited := map[int]bool{state.Schedule[0].ID: true}
	for h.postStatus("/api/control/next", "") == http.StatusOK {
		id := h.state().CurrentTalkID
		if visited[id] {
			t.Fatalf("Next came back to item %d", id)
		}
		visited[id] = true
	}
	if len(visited) != len(state.Schedule) {
		t.Fatalf("Next reached %d of %d items", len(visited), len(state.Schedule))
	}
}

// A retry after a timeout that had in fact landed, or a tap queued behind one,
// names the item the phone was looking at — which is no longer current.
func TestAdvanceFromAStaleItemIsRefused(t *testing.T) {
	h := newOverrideHarness(t)
	first := h.state().CurrentTalkID
	tap := fmt.Sprintf(`{"fromTalkId":%d}`, first)

	h.post("/api/control/next", tap)
	second := h.state().CurrentTalkID
	if second == first {
		t.Fatal("setup: the first tap did not advance")
	}

	if code := h.postStatus("/api/control/next", tap); code != http.StatusPreconditionFailed {
		t.Fatalf("a stale tap was not refused: %d", code)
	}
	if got := h.state().CurrentTalkID; got != second {
		t.Fatalf("a stale tap moved the meeting on again: %d, want %d", got, second)
	}
}

// Reset part re-selects "the current item". From a phone whose stream has
// fallen behind, that is an item the clock has already left, and restarting it
// dragged the meeting back a part.
func TestResetFromAStaleItemIsRefused(t *testing.T) {
	h := newOverrideHarness(t)
	first := h.state().CurrentTalkID
	h.post("/api/control/next", "")
	second := h.state().CurrentTalkID

	stale := fmt.Sprintf(`{"talkId":%d,"fromTalkId":%d}`, first, first)
	if code := h.postStatus("/api/control/select", stale); code != http.StatusPreconditionFailed {
		t.Fatalf("a stale reset was not refused: %d", code)
	}
	if got := h.state().CurrentTalkID; got != second {
		t.Fatalf("a stale reset moved the clock back to %d, want %d", got, second)
	}

	// The picker names only its target, and keeps working as before.
	h.post("/api/control/select", fmt.Sprintf(`{"talkId":%d}`, first))
	if got := h.state().CurrentTalkID; got != first {
		t.Fatalf("picking an item did not select it: %d", got)
	}
}

// The session now begins when the prestart window opens. It used to begin at
// the start time itself, so a first part started a few seconds early lost its
// overtime the moment 19:00 came round.
func TestEarlyStartKeepsItsOvertime(t *testing.T) {
	h := newOverrideHarness(t)
	h.now = time.Date(2026, 7, 9, 18, 59, 50, 0, time.UTC)
	h.post("/api/control/start", "") // Opening Comments, one minute
	h.advance(90 * time.Second)      // 30 seconds over, at 19:01:20
	h.post("/api/control/next", "")
	if got := h.state().MeetingOvertimeSeconds; got != 30 {
		t.Fatalf("an early start lost its overtime: got %ds, want 30", got)
	}
}

// Started before the prestart window even opened, and run across it.
func TestVeryEarlyStartKeepsItsOvertime(t *testing.T) {
	h := newOverrideHarness(t)
	h.at(18, 50)
	h.post("/api/control/start", "")
	h.at(19, 0) // nine minutes over
	h.post("/api/control/next", "")
	if got := h.state().MeetingOvertimeSeconds; got != 540 {
		t.Fatalf("got %ds, want 540", got)
	}
}

// Length and time left used to be clamped separately, so two taps of −1 min on
// the one-minute opening put the meeting a minute behind before anyone spoke.
func TestAdjustMovesLengthAndTimeLeftTogether(t *testing.T) {
	h := newOverrideHarness(t)
	h.post("/api/control/adjust", `{"deltaSeconds":-60}`)
	h.post("/api/control/adjust", `{"deltaSeconds":-60}`)
	idle := h.state()
	if idle.DurationSeconds != 60 || idle.RemainingSeconds != 60 || idle.MeetingOvertimeSeconds != 0 {
		t.Fatalf("got length %ds, left %ds, behind %ds; want 60/60/0", idle.DurationSeconds, idle.RemainingSeconds, idle.MeetingOvertimeSeconds)
	}

	// Running, near the floor: whatever the clamp does, the time already spoken
	// must not move.
	h.selectPart(2) // ten minutes
	h.post("/api/control/start", "")
	h.advance(400 * time.Second)
	for _, delta := range []int{-600, 600} {
		st := h.post("/api/control/adjust", fmt.Sprintf(`{"deltaSeconds":%d}`, delta))
		if st.ElapsedSeconds != 400 {
			t.Fatalf("adjusting by %ds changed the time spoken to %ds", delta, st.ElapsedSeconds)
		}
	}
}

// Elapsed time is counted in whole seconds. Pausing and adjusting used to
// restart the count from now, handing the speaker back the part-second already
// used every time.
func TestPauseAndAdjustKeepTheFractionOfASecond(t *testing.T) {
	h := newOverrideHarness(t)
	h.post("/api/control/start", "")
	h.advance(1500 * time.Millisecond)
	h.post("/api/control/pause", "")
	h.post("/api/control/start", "")
	h.advance(600 * time.Millisecond)
	if got := h.state().ElapsedSeconds; got != 2 {
		t.Fatalf("2.1s run with a pause in the middle shows %ds elapsed, want 2", got)
	}

	h.advance(900 * time.Millisecond) // 3.0s
	h.post("/api/control/adjust", `{"deltaSeconds":60}`)
	h.advance(600 * time.Millisecond) // 3.6s
	if got := h.state().ElapsedSeconds; got != 3 {
		t.Fatalf("3.6s run with an adjust in the middle shows %ds elapsed, want 3", got)
	}
}

// An hourly retry that succeeds before the meeting must not throw away the edit
// made for it: nobody asked for the imported program.
func TestAutoImportLandsBehindAnActiveEdit(t *testing.T) {
	h := newOverrideHarness(t)
	h.at(17, 30)
	h.saveEditedSchedule(120)

	url := "https://wol.jw.org/en/wol/d/r1/lp-e/202026241"
	imported := []Talk{{ID: 1, Title: "Opening Comments", Duration: 90, Closing: 30}}
	h.srv.mu.Lock()
	h.srv.config.AutoImportMidweek = true
	source, due := h.srv.nextAutoImportSourceLocked(h.now)
	var ok bool
	if due {
		_, _, ok = h.srv.applyAutoImportedScheduleLocked(h.now, source, url, imported)
	}
	h.srv.mu.Unlock()
	if !due || !ok {
		t.Fatalf("setup: expected the import to be due and applied (due=%v ok=%v)", due, ok)
	}

	if got := h.state().Schedule[0].Duration; got != 120 {
		t.Fatalf("an automatic import wiped the operator's edit: opening is %ds", got)
	}
	if !sameSchedule(h.srv.config.Schedule, imported) {
		t.Fatalf("the import should still become the baseline, got %+v", h.srv.config.Schedule)
	}

	// Once the edit lapses, the imported program takes over.
	h.advance(sessionWindow)
	if got := h.state().Schedule[0].Duration; got != 90 {
		t.Fatalf("expected the imported baseline after the edit lapsed, got %ds", got)
	}
}

// Checking the window and closing it in two critical sections let two phones
// claiming together both pair through a window meant for one. The gap between
// the two was a few instructions wide, so it takes many rounds to hit: a
// thousand caught the old code in nine runs out of ten, in well under a second.
func TestAddAPhoneWindowPairsExactlyOnePhone(t *testing.T) {
	srv, mux := newSecuredTestServer(t)
	const rounds, claimants = 1000, 64
	for round := range rounds {
		srv.mu.Lock()
		srv.openPairingLocked(srv.clock(), pairingWindow, true)
		srv.mu.Unlock()

		var wg sync.WaitGroup
		var mu sync.Mutex
		paired := 0
		start := make(chan struct{})
		for range claimants {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				req := httptest.NewRequest(http.MethodPost, "/api/pairing/claim", strings.NewReader(`{"pin":""}`))
				res := httptest.NewRecorder()
				mux.ServeHTTP(res, req)
				if res.Code == http.StatusOK {
					mu.Lock()
					paired++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		if paired != 1 {
			t.Fatalf("round %d: a one-phone window paired %d phones", round, paired)
		}
	}
}

// Coming up on defaults drops the PIN and opens the first-boot window to anyone
// on the network. A hall that had locked its clock must stay locked.
func TestUnreadableConfigComesBackFromTheLastGoodCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	good := Config{DeviceName: "East Hall", ControlToken: "token", ControlPIN: "2468", PrestartSeconds: 300}
	if err := saveConfig(path, good); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"deviceName":"Ea`), 0o600); err != nil {
		t.Fatal(err)
	}

	srv, err := newServer(path)
	if err != nil {
		t.Fatal(err)
	}
	if srv.config.ControlPIN != "2468" || srv.config.ControlToken != "token" {
		t.Fatalf("expected the last good copy, got pin=%q token=%q", srv.config.ControlPIN, srv.config.ControlToken)
	}
	if !srv.pairingUntil.IsZero() {
		t.Fatal("a hall with a PIN came up with pairing open")
	}
}

// The startup save can fail on the same card that corrupted the config. The
// unreadable file used to be moved aside, so the next boot saw no config at all
// and came up on defaults with pairing open.
func TestUnreadableConfigFallsBackOnEveryBootUntilSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := saveConfig(path, Config{ControlToken: "token", ControlPIN: "2468"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"controlPin":"24`), 0o600); err != nil {
		t.Fatal(err)
	}
	for boot := 1; boot <= 2; boot++ {
		config, err := loadConfig(path)
		if err != nil || config.ControlPIN != "2468" {
			t.Fatalf("boot %d: expected the last good copy, got pin=%q (%v)", boot, config.ControlPIN, err)
		}
	}
}

// Deleting config.json is the documented reset, and the backup must not undo it.
func TestDeletedConfigIsStillAReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := saveConfig(path, Config{ControlToken: "token", ControlPIN: "2468"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.ControlPIN != "" || config.ControlToken != "" {
		t.Fatal("the backup resurrected a config that was deliberately deleted")
	}
}

func TestNormalizeScheduleKeepsIDs(t *testing.T) {
	schedule := []Talk{{ID: 3, Title: "a"}, {Title: "b"}, {ID: 3, Title: "c"}, {ID: 1, Title: "d"}, {ID: temporaryPartIDBase, Title: "e"}}
	normalizeSchedule(schedule)
	got := make([]int, len(schedule))
	for i, talk := range schedule {
		got[i] = talk.ID
	}
	want := []int{3, 4, 5, 1, 6}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got ids %v, want %v", got, want)
	}
}

func TestWeekendTemplateIsRecognisedWhateverItsIDs(t *testing.T) {
	weekend := weekendSchedule("en")
	weekend[0].ID, weekend[1].ID = 7, 9
	if !isWeekendSchedule(weekend) {
		t.Fatal("a renumbered weekend template slipped through as a midweek programme")
	}
}

func TestSettingsAreBounded(t *testing.T) {
	h := newOverrideHarness(t)
	state := h.post("/api/control/adhoc-part", fmt.Sprintf(`{"title":%q,"seconds":120}`, strings.Repeat("x", 10000)))
	for _, talk := range state.Schedule {
		if len([]rune(talk.Title)) > maxTitleRunes {
			t.Fatalf("an ad-hoc title of %d characters was kept", len([]rune(talk.Title)))
		}
	}

	tooMany := make([]Talk, maxScheduleParts+1)
	for i := range tooMany {
		tooMany[i] = Talk{Title: "part", Duration: 60}
	}
	body, _ := json.Marshal(Config{Schedule: tooMany})
	if code := h.postStatus("/api/config", string(body)); code != http.StatusBadRequest {
		t.Fatalf("a %d-part schedule was accepted: %d", len(tooMany), code)
	}
}

func hasTitle(schedule []Talk, title string) bool {
	for _, talk := range schedule {
		if talk.Title == title {
			return true
		}
	}
	return false
}
