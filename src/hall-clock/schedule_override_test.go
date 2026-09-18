package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// overrideHarness drives the HTTP API with a movable clock, which is what the
// override window turns on.
type overrideHarness struct {
	t   *testing.T
	srv *server
	mux http.Handler
	now time.Time
}

func newOverrideHarness(t *testing.T) *overrideHarness {
	t.Helper()
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &overrideHarness{t: t, srv: srv, now: time.Date(2026, 7, 9, 19, 0, 0, 0, time.UTC)} // Thursday
	srv.clock = func() time.Time { return h.now }
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}
	h.mux = mux
	return h
}

func (h *overrideHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *overrideHarness) post(path, body string) State {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("X-Wall-Clock-Token", h.srv.config.ControlToken)
	res := httptest.NewRecorder()
	h.mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		h.t.Fatalf("%s returned %d: %s", path, res.Code, res.Body.String())
	}
	var state State
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		h.t.Fatal(err)
	}
	return state
}

func (h *overrideHarness) state() State {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	res := httptest.NewRecorder()
	h.mux.ServeHTTP(res, req)
	var state State
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		h.t.Fatal(err)
	}
	return state
}

func (h *overrideHarness) saveEditedSchedule(openingSeconds int) State {
	h.t.Helper()
	config := Config{
		DeviceName:       "Hall Clock",
		MeetingType:      "midweek",
		MeetingStartTime: "19:00",
		MeetingStarts:    defaultMeetingStarts("19:00"),
		PrestartSeconds:  300,
		Schedule: []Talk{
			{Title: "Opening Comments", Duration: openingSeconds, Closing: 30},
			{Title: "Treasures From God's Word", Duration: 600, Closing: 120},
		},
	}
	body, err := json.Marshal(config)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.post("/api/config", string(body))
}

// The window is three hours, matching circuitOverseerDuration. Asserted against
// a literal so the tests below cannot drift along with the constant.
func TestScheduleOverrideWindowIsThreeHours(t *testing.T) {
	if scheduleOverrideDuration != 3*time.Hour {
		t.Fatalf("schedule edits must expire after 3h, got %v", scheduleOverrideDuration)
	}
}

// effectiveMidweekSchedule is the single authority on which program governs,
// and it is consulted on paths that never run recalculate (boot, GET
// /api/config, the setup page). Pin every combination of window and meeting.
func TestEffectiveMidweekScheduleHonoursExpiryAndRunState(t *testing.T) {
	now := time.Date(2026, 7, 9, 19, 0, 0, 0, time.UTC)
	baseline := []Talk{{ID: 1, Title: "Opening Comments", Duration: 60, Closing: 30}}
	edited := []Talk{{ID: 1, Title: "Opening Comments", Duration: 120, Closing: 30}}

	open := Config{Schedule: baseline, ScheduleOverride: edited, ScheduleOverrideExpiresAt: now.Add(time.Minute)}
	lapsed := Config{Schedule: baseline, ScheduleOverride: edited, ScheduleOverrideExpiresAt: now.Add(-time.Minute)}
	none := Config{Schedule: baseline}

	cases := []struct {
		name       string
		config     Config
		inProgress bool
		want       []Talk
	}{
		{"window open, between meetings", open, false, edited},
		{"window open, meeting in progress", open, true, edited},
		{"window lapsed, between meetings", lapsed, false, baseline},
		// The crux: a meeting that outruns the window keeps its edited program,
		// idle between two parts as much as mid-part.
		{"window lapsed, meeting in progress", lapsed, true, edited},
		{"no edit, between meetings", none, false, baseline},
		{"no edit, meeting in progress", none, true, baseline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveMidweekSchedule(tc.config, tc.inProgress, now); !sameSchedule(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestScheduleEditAppliesThenExpiresToBaseline(t *testing.T) {
	h := newOverrideHarness(t)
	baseline := append([]Talk(nil), h.srv.config.Schedule...)

	got := h.saveEditedSchedule(120)
	if got.Schedule[0].Duration != 120 || len(got.Schedule) != 2 {
		t.Fatalf("edit should apply immediately, got %d parts, first=%ds", len(got.Schedule), got.Schedule[0].Duration)
	}
	if got.ScheduleOverrideExpiresAt == nil {
		t.Fatal("expected an override expiry to be reported to the UI")
	}

	// The edit must never overwrite the congregation's baseline program.
	if !sameSchedule(h.srv.config.Schedule, baseline) {
		t.Fatalf("baseline was clobbered by an edit: %+v", h.srv.config.Schedule)
	}

	// Still inside the 3-hour window.
	h.advance(3*time.Hour - time.Minute)
	if got := h.state(); got.Schedule[0].Duration != 120 {
		t.Fatalf("edit should still apply inside the window, got %ds", got.Schedule[0].Duration)
	}

	// Past the window: the next congregation gets the baseline back.
	h.advance(2 * time.Minute)
	after := h.state()
	if !sameSchedule(after.Schedule, baseline) {
		t.Fatalf("expected baseline after expiry, got %+v", after.Schedule)
	}
	if after.ScheduleOverrideExpiresAt != nil {
		t.Fatal("expected the override expiry to clear once it lapsed")
	}
}

func TestScheduleEditNeverExpiresMidMeeting(t *testing.T) {
	h := newOverrideHarness(t)
	baseline := append([]Talk(nil), h.srv.config.Schedule...)

	h.saveEditedSchedule(120)
	h.post("/api/control/start", "")

	// Three hours into a running meeting the edit must hold: the clock on the
	// wall cannot change parts under the brother who is speaking.
	h.advance(3*time.Hour + time.Hour)
	running := h.state()
	if running.Schedule[0].Duration != 120 {
		t.Fatalf("a running meeting must keep its edited schedule, got %ds", running.Schedule[0].Duration)
	}

	// Moving between parts leaves the clock idle, but the meeting is not over:
	// the edit must survive it. This used to bring the removed parts back at
	// the first part change after the window closed.
	h.post("/api/control/next", "")
	between := h.state()
	if between.Schedule[0].Duration != 120 {
		t.Fatalf("the edit lapsed between two parts of a running meeting, got %ds", between.Schedule[0].Duration)
	}

	// Once the meeting is ended, the expired edit gives way to the baseline.
	h.post("/api/control/end", "")
	ended := h.state()
	if !sameSchedule(ended.Schedule, baseline) {
		t.Fatalf("expected baseline once the meeting ended, got %+v", ended.Schedule)
	}
}

func TestSavingBaselineClearsOverride(t *testing.T) {
	h := newOverrideHarness(t)
	h.saveEditedSchedule(120)
	if len(h.srv.config.ScheduleOverride) == 0 {
		t.Fatal("expected an override after an edit")
	}

	// Re-saving the untouched baseline is not an edit.
	baseline := h.srv.config.Schedule
	config := Config{
		DeviceName: "Hall Clock", MeetingType: "midweek", MeetingStartTime: "19:00",
		MeetingStarts: defaultMeetingStarts("19:00"), PrestartSeconds: 300,
		Schedule: append([]Talk(nil), baseline...),
	}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	h.post("/api/config", string(body))

	if len(h.srv.config.ScheduleOverride) != 0 || !h.srv.config.ScheduleOverrideExpiresAt.IsZero() {
		t.Fatalf("saving the baseline should clear the override, got %+v exp=%v",
			h.srv.config.ScheduleOverride, h.srv.config.ScheduleOverrideExpiresAt)
	}
}

func TestResavingUnchangedEditDoesNotExtendWindow(t *testing.T) {
	h := newOverrideHarness(t)
	h.saveEditedSchedule(120)
	firstExpiry := h.srv.config.ScheduleOverrideExpiresAt

	// An hour later the operator saves again without touching the schedule (say,
	// they renamed the device). That must not buy the edit three more hours.
	h.advance(time.Hour)
	h.saveEditedSchedule(120)

	if !h.srv.config.ScheduleOverrideExpiresAt.Equal(firstExpiry) {
		t.Fatalf("unchanged re-save extended the window: %v -> %v", firstExpiry, h.srv.config.ScheduleOverrideExpiresAt)
	}
}

// Regression: a save during a meeting that has outrun the override window used
// to snap the live timer back to the baseline mid-part, because the schedule
// resolver checked only the clock while the clearing sweep checked only idle.
func TestSaveDuringOverrunMeetingDoesNotRevertLiveTimer(t *testing.T) {
	h := newOverrideHarness(t)

	h.saveEditedSchedule(120)
	h.post("/api/control/start", "")
	h.advance(3*time.Hour + 30*time.Minute)

	// The setup page must still be serving the edited program, so the form
	// cannot post a baseline the clock is not running.
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	res := httptest.NewRecorder()
	h.mux.ServeHTTP(res, req)
	var served Config
	if err := json.Unmarshal(res.Body.Bytes(), &served); err != nil {
		t.Fatal(err)
	}
	if served.Schedule[0].Duration != 120 {
		t.Fatalf("editor served %ds while the wall clock runs 120s", served.Schedule[0].Duration)
	}

	// Operator renames the device mid-meeting; the schedule must not move.
	after := h.saveEditedSchedule(120)
	if after.DurationSeconds != 120 {
		t.Fatalf("running meeting reverted to %ds mid-part", after.DurationSeconds)
	}
	if after.Status != StatusRunning {
		t.Fatalf("expected the meeting to still be running, got %s", after.Status)
	}
}

// Regression: a stale browser tab posting the still-edited schedule after the
// window lapsed used to grant it a fresh three hours.
func TestSaveAfterExpiryDoesNotRearmOverride(t *testing.T) {
	h := newOverrideHarness(t)
	h.saveEditedSchedule(120)
	h.post("/api/control/start", "")

	h.advance(3*time.Hour + 30*time.Minute)
	h.saveEditedSchedule(120)

	expiry := h.srv.config.ScheduleOverrideExpiresAt
	if !expiry.IsZero() && expiry.After(h.now) {
		t.Fatalf("expired one-session edit resurrected until %v (now %v)", expiry, h.now)
	}

	// And once the meeting is over, the baseline returns.
	h.post("/api/control/end", "")
	if got := h.state(); got.Schedule[0].Duration != 60 {
		t.Fatalf("expected baseline once the meeting ended, got %ds", got.Schedule[0].Duration)
	}
}

func TestNewBaselineClearsActiveOverride(t *testing.T) {
	h := newOverrideHarness(t)
	h.saveEditedSchedule(120)

	// An import or template lands a new baseline; the stale edit must not shadow
	// it. Mirrors the production call pattern: set the baseline, then rebuild.
	imported := []Talk{{ID: 1, Title: "Opening Comments", Duration: 90, Closing: 30}}
	h.srv.mu.Lock()
	h.srv.setBaselineScheduleLocked(imported)
	h.srv.applyActiveScheduleChangeLocked(h.now)
	h.srv.mu.Unlock()

	if len(h.srv.config.ScheduleOverride) != 0 || !h.srv.config.ScheduleOverrideExpiresAt.IsZero() {
		t.Fatal("a new baseline must clear the override")
	}
	if got := h.state(); got.Schedule[0].Duration != 90 {
		t.Fatalf("expected the new baseline to run, got %ds", got.Schedule[0].Duration)
	}
}
