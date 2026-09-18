package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// midweekClock is a fixed Wednesday. meetingTypeForTime resolves the meeting
// type from the calendar, and newServer derives the active schedule at
// construction, so a test that wants the midweek program has to pin the day
// before the server is built — otherwise the suite fails every Saturday and
// Sunday, CI included.
func midweekClock() func() time.Time {
	now := time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC) // Wednesday
	return func() time.Time { return now }
}

// newMidweekServer builds a server pinned to a midweek day.
func newMidweekServer(t *testing.T, configPath string) *server {
	t.Helper()
	srv, err := newServerWithClock(configPath, midweekClock())
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestProtectedControlRequiresToken(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/control/start", nil)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized response, got %d", res.Code)
	}
}

func TestProtectedControlAcceptsPairingToken(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/control/start", nil)
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected OK response, got %d: %s", res.Code, res.Body.String())
	}

	var state State
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusRunning {
		t.Fatalf("expected running status, got %q", state.Status)
	}
}

func TestMidweekLanguageChangeRequiresIdleTimer(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	startReq := httptest.NewRequest(http.MethodPost, "/api/control/start", nil)
	startReq.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	mux.ServeHTTP(httptest.NewRecorder(), startReq)

	req := httptest.NewRequest(http.MethodPost, "/api/control/midweek-language", strings.NewReader(`{"language":"es"}`))
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusConflict {
		t.Fatalf("expected conflict while timer is running, got %d: %s", res.Code, res.Body.String())
	}
}

func TestMidweekLanguageSwitchUsesCachedSchedule(t *testing.T) {
	srv := newMidweekServer(t, filepath.Join(t.TempDir(), "config.json"))

	srv.mu.Lock()
	srv.config.MidweekLanguageSchedules = map[string]MidweekLanguageSchedule{
		"es": {
			ImportedWeek: "2026-W28",
			URL:          "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
			Schedule: []Talk{
				{ID: 1, Title: "Comentarios de introducción", Duration: 60, Closing: 30},
			},
		},
	}
	config, state, ok, message := srv.applyCachedMidweekLanguageScheduleLocked(
		time.Date(2026, 7, 6, 18, 0, 0, 0, time.UTC),
		"es",
	)
	srv.mu.Unlock()

	if !ok {
		t.Fatalf("expected cached Spanish schedule to apply: %s", message)
	}
	if config.MidweekLanguage != "es" || state.Schedule[0].Title != "Comentarios de introducción" {
		t.Fatalf("expected Spanish schedule, got config=%+v state=%+v", config, state.Schedule)
	}
}

func TestLanguageSwitchUpdatesWeekendSchedule(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC) // Sunday
	srv.clock = func() time.Time { return now }

	srv.mu.Lock()
	srv.config.MidweekLanguageSchedules = map[string]MidweekLanguageSchedule{
		"es": {
			ImportedWeek: isoWeekString(now),
			URL:          "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
			Schedule: []Talk{
				{ID: 1, Title: "Comentarios de introducción", Duration: 60, Closing: 30},
			},
		},
	}
	_, state, ok, message := srv.applyCachedMidweekLanguageScheduleLocked(now, "es")
	srv.mu.Unlock()

	if !ok {
		t.Fatalf("expected cached Spanish schedule to apply on weekend: %s", message)
	}
	if state.MeetingType != "weekend" {
		t.Fatalf("expected active weekend meeting, got %q", state.MeetingType)
	}
	if len(state.Schedule) != 2 || state.Schedule[0].Title != "Discurso público" || state.Schedule[1].Title != "Estudio de La Atalaya" {
		t.Fatalf("expected Spanish weekend schedule, got %+v", state.Schedule)
	}
}

func TestMeetingStartLanguageAutoSwitchesWeekendSchedule(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 11, 18, 30, 0, 0, time.UTC) // Saturday, 30 minutes before 7:00 PM
	srv.clock = func() time.Time { return now }

	srv.mu.Lock()
	srv.config.MidweekLanguage = "en"
	srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{
		{
			Day:      int(time.Saturday),
			Time:     "19:00",
			Language: "tw",
		},
	}, "19:00")
	srv.mu.Unlock()

	state := srv.snapshot()
	if state.MidweekLanguage != "tw" {
		t.Fatalf("expected active language to switch to Twi, got %q", state.MidweekLanguage)
	}
	if state.MeetingType != "weekend" {
		t.Fatalf("expected active weekend meeting, got %q", state.MeetingType)
	}
	if len(state.Schedule) != 2 || state.Schedule[0].Title != "Baguam Kasa" || state.Schedule[1].Title != "Ɔwɛn-Aban Adesua" {
		t.Fatalf("expected Twi weekend schedule, got %+v", state.Schedule)
	}
}

func TestMeetingStartLanguageAutoSwitchesMidweekScheduleFromCache(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 6, 18, 30, 0, 0, time.UTC) // Monday, 30 minutes before 7:00 PM
	srv.clock = func() time.Time { return now }

	srv.mu.Lock()
	srv.config.MidweekLanguage = "en"
	srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{
		{
			Day:      int(time.Monday),
			Time:     "19:00",
			Language: "es",
		},
	}, "19:00")
	srv.config.MidweekLanguageSchedules = map[string]MidweekLanguageSchedule{
		"es": {
			ImportedWeek: isoWeekString(now),
			URL:          "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
			Schedule: []Talk{
				{ID: 1, Title: "Comentarios de introducción", Duration: 60, Closing: 30},
			},
		},
	}
	srv.mu.Unlock()

	state := srv.snapshot()
	if state.MidweekLanguage != "es" {
		t.Fatalf("expected active language to switch to Spanish, got %q", state.MidweekLanguage)
	}
	if state.MeetingType != "midweek" {
		t.Fatalf("expected active midweek meeting, got %q", state.MeetingType)
	}
	if len(state.Schedule) != 1 || state.Schedule[0].Title != "Comentarios de introducción" {
		t.Fatalf("expected cached Spanish midweek schedule, got %+v", state.Schedule)
	}
}

func TestMeetingStartLanguageAutoSwitchPrefersUpcomingStart(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 6, 20, 30, 0, 0, time.UTC) // 30 minutes before the second meeting
	srv.clock = func() time.Time { return now }

	srv.mu.Lock()
	srv.config.MidweekLanguage = "es"
	srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{
		{Day: int(time.Monday), Time: "19:00", Language: "es"},
		{Day: int(time.Monday), Time: "21:00", Language: "tw"},
	}, "19:00")
	srv.config.MidweekLanguageSchedules = map[string]MidweekLanguageSchedule{
		"tw": {
			ImportedWeek: isoWeekString(now),
			URL:          "https://wol.jw.org/tw/wol/d/r33/lp-tw/202026241",
			Schedule: []Talk{
				{ID: 1, Title: "Nnianim Nsɛm", Duration: 60, Closing: 30},
			},
		},
	}
	srv.mu.Unlock()

	state := srv.snapshot()
	if state.MidweekLanguage != "tw" {
		t.Fatalf("expected upcoming Twi start to win, got %q", state.MidweekLanguage)
	}
	if len(state.Schedule) != 1 || state.Schedule[0].Title != "Nnianim Nsɛm" {
		t.Fatalf("expected cached Twi midweek schedule, got %+v", state.Schedule)
	}
}

func TestMidweekLanguageSwitchRejectsMissingCurrentWeekCache(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	srv.mu.Lock()
	_, _, ok, message := srv.applyCachedMidweekLanguageScheduleLocked(
		time.Date(2026, 7, 6, 18, 0, 0, 0, time.UTC),
		"tw",
	)
	srv.mu.Unlock()

	if ok {
		t.Fatal("expected missing Twi cache to be rejected")
	}
	if !strings.Contains(message, "not imported") {
		t.Fatalf("expected cache-miss message, got %q", message)
	}
}

func TestMidweekLanguageSwitchCanImportMissingLanguageOnDemand(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	srv.clock = func() time.Time {
		return time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC)
	}
	originalFetch := fetchWOLPageFunc
	// The stored Twi source is the week-24 example document; the import must
	// resolve the current week via the weekly meetings page rather than fetch
	// the stale document directly, so only 202026999 may serve a schedule.
	fetchWOLPageFunc = func(ctx context.Context, sourceURL string) (string, error) {
		switch {
		case strings.Contains(sourceURL, "/tw/wol/meetings/r33/lp-tw/2026/28"):
			return `<a href="/tw/wol/d/r33/lp-tw/202026999">Workbook</a>`, nil
		case strings.Contains(sourceURL, "/wol/d/r33/lp-tw/202026999"):
			return `
				<h2>July 13-19</h2>
				<p>Ɔkasa (5 min.)</p>
				<p>Adwumayɛ mu nsɛm (10 min.)</p>
				<p>Kyerɛw kronkron akenkan (4 min.)</p>
				<p>Awiei nsɛm (1 min.)</p>
			`, nil
		default:
			return "", fmt.Errorf("unexpected URL: %s", sourceURL)
		}
	}
	defer func() { fetchWOLPageFunc = originalFetch }()

	req := httptest.NewRequest(http.MethodPost, "/api/control/midweek-language", strings.NewReader(`{"language":"tw"}`))
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected on-demand Twi import to succeed, got %d: %s", res.Code, res.Body.String())
	}

	var state State
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.MidweekLanguage != "tw" {
		t.Fatalf("expected Twi state after import, got %q", state.MidweekLanguage)
	}
	if len(state.Schedule) == 0 || state.Schedule[0].Title != "Ɔkasa" {
		t.Fatalf("expected imported schedule in response, got %+v", state.Schedule)
	}
	srv.mu.Lock()
	cached, ok := srv.config.MidweekLanguageSchedules["tw"]
	srv.mu.Unlock()
	if !ok || cached.ImportedWeek != "2026-W28" || len(cached.Schedule) == 0 {
		t.Fatalf("expected Twi cache to be stored after import, got %+v", cached)
	}
	if !strings.HasSuffix(cached.URL, "202026999") {
		t.Fatalf("expected cache to record the weekly page's document, got %q", cached.URL)
	}
}

func TestManualLanguageChoiceSurvivesMeetingStartWindow(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Tuesday 2026-07-21 11:20 UTC, inside the Twi start's forced-language
	// window of [11:10, 14:40). Both languages are cached for the week.
	now := time.Date(2026, 7, 21, 11, 20, 0, 0, time.UTC)
	srv.clock = func() time.Time { return now }
	srv.config.MeetingStarts = []MeetingStart{{ID: 1, Day: 2, Time: "11:40", Language: "tw"}}
	srv.config.MidweekLanguage = "tw"
	srv.config.MidweekImportedWeek = "2026-W30"
	srv.config.MidweekURL = "https://wol.jw.org/tw/wol/d/r33/lp-tw/202026243"
	srv.config.MidweekLanguageSchedules = map[string]MidweekLanguageSchedule{
		"tw": {ImportedWeek: "2026-W30", URL: "https://wol.jw.org/tw/wol/d/r33/lp-tw/202026243", Schedule: []Talk{{ID: 1, Title: "Ɔkasa", Duration: 300}}},
		"en": {ImportedWeek: "2026-W30", URL: "https://wol.jw.org/en/wol/d/r1/lp-e/202026243", Schedule: []Talk{{ID: 1, Title: "Opening Comments", Duration: 300}}},
	}

	srv.mu.Lock()
	_, state, ok, message := srv.applyCachedMidweekLanguageScheduleLocked(now, "en")
	srv.mu.Unlock()
	if !ok {
		t.Fatalf("switch to English failed: %s", message)
	}
	if state.MidweekLanguage != "en" {
		t.Fatalf("switch reverted inside its own response, got %q", state.MidweekLanguage)
	}
	// The next broadcast tick recalculates; the sync must not flip it back.
	state = srv.snapshot()
	if state.MidweekLanguage != "en" {
		t.Fatalf("idle sync reverted the operator's choice, got %q", state.MidweekLanguage)
	}
}

func TestAdhocPartAddedBeforeMeetingSurvivesStart(t *testing.T) {
	// Meeting Tuesday 11:40 with a 5-minute prestart puts the session boundary
	// at 11:35. The operator added the item at 11:33 — prepared for this
	// meeting, not left over from the previous one. The day has to be pinned at
	// construction: newServer picks the schedule from the calendar.
	created := time.Date(2026, 7, 21, 11, 33, 0, 0, time.UTC)
	now := time.Date(2026, 7, 21, 11, 42, 0, 0, time.UTC)
	srv, err := newServerWithClock(filepath.Join(t.TempDir(), "config.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	srv.config.MeetingStarts = []MeetingStart{{ID: 1, Day: 2, Time: "11:40"}}
	srv.config.PrestartSeconds = 300
	srv.mu.Lock()
	srv.talks = append(srv.talks, Talk{ID: 99, Title: "Special announcement", Duration: 300, Temporary: true, CreatedAt: created})
	srv.state.Schedule = srv.talks
	srv.mu.Unlock()

	state := srv.snapshot()
	for _, talk := range state.Schedule {
		if talk.ID == 99 {
			return
		}
	}
	t.Fatal("ad-hoc item created minutes before the meeting was purged at meeting start")
}

func TestMidweekLanguageCacheStampedWithWrongWeekDocIsRejected(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	// 2026-07-21 is ISO week 2026-W30. The active import holds this week's
	// document (…243), but the Twi cache was stamped W30 while holding last
	// week's document (…242) — the poisoned state left behind by imports that
	// fetched a stored week-specific URL directly.
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	srv.config.MidweekURL = "https://wol.jw.org/en/wol/d/r1/lp-e/202026243"
	srv.config.MidweekImportedWeek = "2026-W30"
	srv.config.MidweekLanguageSchedules = map[string]MidweekLanguageSchedule{
		"tw": {
			ImportedWeek: "2026-W30",
			URL:          "https://wol.jw.org/tw/wol/d/r33/lp-tw/202026242",
			Schedule:     []Talk{{ID: 1, Title: "Ɔkasa", Duration: 300}},
		},
	}

	srv.mu.Lock()
	_, _, ok, message := srv.applyCachedMidweekLanguageScheduleLocked(now, "tw")
	srv.mu.Unlock()
	if ok {
		t.Fatal("expected wrong-week Twi cache to be rejected")
	}
	if !strings.Contains(message, "not imported for this week yet") {
		t.Fatalf("expected re-import trigger message, got %q", message)
	}
}

func TestPairingEndpointReturnsTokenlessControlURL(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/pairing", nil)
	req.Host = "hallclock.local:8080"
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected OK pairing response, got %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), "http://hallclock.local:8080\"") {
		t.Fatalf("expected root control URL, got %s", res.Body.String())
	}
}

func TestPairingEndpointUsesConfiguredPublicURL(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("http://hallclock.local:8080/control")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/pairing", nil)
	req.Host = "192.168.1.50:8080"
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected OK pairing response, got %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), "http://hallclock.local:8080/control\"") {
		t.Fatalf("expected configured public URL, got %s", res.Body.String())
	}
}

func TestRequestBaseURLTrustsForwardedFromLoopback(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/pairing", nil)
	req.RemoteAddr = "127.0.0.1:54321" // the co-located reverse proxy
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-Forwarded-Host", "hallclock.local")
	req.Header.Set("X-Forwarded-Proto", "http")

	got := requestBaseURL(req)
	if got != "http://hallclock.local" {
		t.Fatalf("expected portless proxied origin, got %q", got)
	}
}

func TestRequestBaseURLIgnoresForwardedFromUntrustedPeer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/pairing", nil)
	req.RemoteAddr = "192.168.1.77:40000" // a phone on the LAN, not the proxy
	req.Host = "192.168.1.50:8080"
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "https")

	got := requestBaseURL(req)
	if strings.Contains(got, "evil.example") {
		t.Fatalf("must not trust forwarded host from an untrusted peer, got %q", got)
	}
	if got != "http://192.168.1.50:8080" {
		t.Fatalf("expected fallback to request host, got %q", got)
	}
}

func TestPortlessLocalhostIgnoresStaleWallclockOverride(t *testing.T) {
	// Behind a proxy on a standard port the Host has no ":port" (e.g. Caddy on
	// 443 gives Host "localhost"). A stale hallclock.local override must still
	// be ignored so pairing resolves to the reachable request origin.
	cfg := "http://hallclock.local:8080/control"
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
		r := httptest.NewRequest(http.MethodGet, "/api/pairing", nil)
		r.Host = host
		if shouldUseConfiguredAdvertisedURL(cfg, r) {
			t.Fatalf("host %q: expected stale hallclock.local override to be ignored", host)
		}
	}
}

func TestLoopbackRequestIgnoresAnyRemoteOverride(t *testing.T) {
	// The stale-config guard is not about one magic hostname: a renamed Pi
	// (hall-b.local) or a typo saved on the setup page is just as unreachable
	// from a laptop browsing localhost, and the config value now outranks the
	// -public-url flag, so nothing else would catch it.
	for _, cfg := range []string{
		"http://hallclock.local:8080/control",
		"http://hall-b.local",
		"https://hallclok.example.com",
	} {
		for _, host := range []string{"localhost", "127.0.0.1", "[::1]", "127.0.0.1:8480"} {
			r := httptest.NewRequest(http.MethodGet, "/api/pairing", nil)
			r.Host = host
			if shouldUseConfiguredAdvertisedURL(cfg, r) {
				t.Fatalf("config %q from host %q: expected the unreachable override to be ignored", cfg, host)
			}
		}
	}

	// A loopback URL saved deliberately is not stale — it is what the operator
	// is looking at, so it must survive.
	r := httptest.NewRequest(http.MethodGet, "/api/pairing", nil)
	r.Host = "localhost:8480"
	if !shouldUseConfiguredAdvertisedURL("http://localhost:8480", r) {
		t.Fatal("expected a loopback override to be kept for a loopback request")
	}
}

func TestPairingEndpointUsesSavedAdvertisedURLWhenCLIFlagUnset(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.config.AdvertisedBaseURL = "http://hallclock.local/control"

	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/pairing", nil)
	req.Host = "192.168.1.50:8080"
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected OK pairing response, got %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), "http://hallclock.local/control\"") {
		t.Fatalf("expected saved advertised URL, got %s", res.Body.String())
	}
}

func TestPairingEndpointPrefersSavedAdvertisedURLOverCLIFlag(t *testing.T) {
	// The systemd unit always passes -public-url http://<host>.local, so the
	// setup-page URL must outrank the flag or it could never take effect on an
	// appliance — e.g. advertising an HTTPS domain for Android installs.
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.config.AdvertisedBaseURL = "https://hallclock.example.com"

	mux, err := srv.routes("http://hallclock.local")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/pairing", nil)
	req.Host = "hallclock.example.com"
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected OK pairing response, got %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), "https://hallclock.example.com\"") {
		t.Fatalf("expected saved advertised URL to beat the CLI flag, got %s", res.Body.String())
	}
}

func TestPairingEndpointIgnoresWallclockLocalWhenOpenedFromLocalhost(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.config.AdvertisedBaseURL = "http://hallclock.local:8080/control"

	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/pairing", nil)
	req.Host = "localhost:8080"
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected OK pairing response, got %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), "http://") || !strings.Contains(res.Body.String(), ":8080\"") {
		t.Fatalf("expected fallback LAN root URL, got %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), "hallclock.local") {
		t.Fatalf("expected localhost pairing to ignore saved hallclock.local URL, got %s", res.Body.String())
	}
}

func TestSaveConfigKeepsRunningTimer(t *testing.T) {
	srv := newMidweekServer(t, filepath.Join(t.TempDir(), "config.json"))
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	do := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", path, res.Code, res.Body.String())
		}
		return res
	}

	do("/api/control/select", `{"talkId":2}`)
	do("/api/control/start", "")

	config := Config{Schedule: []Talk{
		{Title: "Opening", Duration: 60, Closing: 30},
		{Title: "Renamed Talk", Duration: 900, Closing: 90},
	}}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	res := do("/api/config", string(body))

	var state State
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusRunning {
		t.Fatalf("expected timer to keep running, got %q", state.Status)
	}
	if state.CurrentTalkID != 2 || state.CurrentTalkTitle != "Renamed Talk" {
		t.Fatalf("expected current talk to be preserved, got %d %q", state.CurrentTalkID, state.CurrentTalkTitle)
	}
	// The edited duration applies, but the posted closing bell does not: the
	// import defines it, and "Renamed Talk" matches nothing in the baseline, so
	// it falls back to the import's formula for a 15-minute part.
	if state.DurationSeconds != 900 || state.ClosingSeconds != derivedClosingSeconds(15) {
		t.Fatalf("expected edited timing to apply, got duration=%d closing=%d", state.DurationSeconds, state.ClosingSeconds)
	}
	// Talk 2 started with 600s; the edit adds 300s, so remaining should be ~900.
	if state.RemainingSeconds < 895 || state.RemainingSeconds > 900 {
		t.Fatalf("expected remaining time to shift with the new duration, got %d", state.RemainingSeconds)
	}
}

func TestSaveConfigPreservesMidweekLanguageSchedulesWhenOmitted(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	srv.mu.Lock()
	srv.config.MidweekLanguageSchedules = map[string]MidweekLanguageSchedule{
		"es": {
			ImportedWeek: "2026-W28",
			URL:          "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
			Schedule: []Talk{
				{ID: 1, Title: "Comentarios de introducción", Duration: 60, Closing: 30},
			},
		},
	}
	srv.mu.Unlock()

	body, err := json.Marshal(Config{
		Schedule: []Talk{
			{Title: "Opening", Duration: 60, Closing: 30},
			{Title: "Main Talk", Duration: 600, Closing: 90},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(string(body)))
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected OK response, got %d: %s", res.Code, res.Body.String())
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	got, ok := srv.config.MidweekLanguageSchedules["es"]
	if !ok {
		t.Fatal("expected Spanish language schedule cache to be preserved")
	}
	if got.ImportedWeek != "2026-W28" || len(got.Schedule) != 1 || got.Schedule[0].Title != "Comentarios de introducción" {
		t.Fatalf("expected Spanish cache to remain intact, got %+v", got)
	}
}

func TestSaveConfigResetsWhenCurrentTalkRemoved(t *testing.T) {
	srv := newMidweekServer(t, filepath.Join(t.TempDir(), "config.json"))
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	do := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", path, res.Code, res.Body.String())
		}
		return res
	}

	do("/api/control/select", `{"talkId":3}`)
	do("/api/control/start", "")

	config := Config{Schedule: []Talk{
		{Title: "Only Part", Duration: 300, Closing: 60},
	}}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	res := do("/api/config", string(body))

	var state State
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusIdle {
		t.Fatalf("expected idle status after current talk removed, got %q", state.Status)
	}
	if state.CurrentTalkID != 1 || state.CurrentTalkTitle != "Only Part" {
		t.Fatalf("expected reset to first talk, got %d %q", state.CurrentTalkID, state.CurrentTalkTitle)
	}
}

func TestSetTimeUpdatesIdleCurrentTimer(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/control/time", strings.NewReader(`{"seconds":240}`))
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected OK response, got %d: %s", res.Code, res.Body.String())
	}

	var state State
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusIdle {
		t.Fatalf("expected timer to stay idle, got %q", state.Status)
	}
	if state.DurationSeconds != 240 || state.RemainingSeconds != 240 {
		t.Fatalf("expected edited time to be 240s, got duration=%d remaining=%d", state.DurationSeconds, state.RemainingSeconds)
	}
	if srv.config.Schedule[0].Duration == 240 {
		t.Fatal("did not expect one-off time edit to rewrite saved schedule")
	}
}

func TestAdhocPartAddsTemporaryPartWithoutSavingConfig(t *testing.T) {
	// Pinned to a weekday: this asserts on "Opening Comments", a midweek part,
	// and newServer picks the schedule from the real calendar.
	srv := newMidweekServer(t, filepath.Join(t.TempDir(), "config.json"))
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}
	savedScheduleLength := len(srv.config.Schedule)
	runtimeScheduleLength := len(srv.talks)

	req := httptest.NewRequest(http.MethodPost, "/api/control/adhoc-part", strings.NewReader(`{"title":"Local announcement","seconds":420}`))
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected OK response, got %d: %s", res.Code, res.Body.String())
	}

	var state State
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Schedule) != runtimeScheduleLength+1 {
		t.Fatalf("expected one temporary part, got %d parts", len(state.Schedule))
	}
	// Adding never steals the clock: what was lined up stays lined up, and
	// the new item lands after it (the no-position legacy default).
	if state.CurrentTalkTitle != "Opening Comments" {
		t.Fatalf("expected the selection to stay put, got %q", state.CurrentTalkTitle)
	}
	if state.Schedule[1].Title != "Local announcement" || !state.Schedule[1].Temporary {
		t.Fatalf("expected the temporary part right after the current item, got %+v", state.Schedule)
	}
	if len(srv.config.Schedule) != savedScheduleLength {
		t.Fatalf("expected saved schedule to stay at %d parts, got %d", savedScheduleLength, len(srv.config.Schedule))
	}
}

// The runtime talks list must never share a backing array with the saved
// programme. It used to at boot, and inserting an ad-hoc part shifted the
// shared array in place, writing the temporary part into config.Schedule —
// the idle reconciler then saw baseline and talks disagree forever and
// re-merged one more copy of the part on every tick, hundreds within minutes.
// The JSON round-trip matters: a decoded slice has spare capacity, which is
// what let the in-place shift land in the shared array instead of forcing a
// reallocation.
func TestAdhocInsertNeverTouchesTheSavedProgramme(t *testing.T) {
	seed := Config{
		Version:    currentConfigVersion,
		DeviceName: "Hall Clock",
		Schedule: []Talk{
			{ID: 1, Title: "Opening Comments", Duration: 60},
			{ID: 2, Title: "Spiritual Gems", Duration: 600},
			{ID: 3, Title: "Bible Reading", Duration: 240},
			{ID: 4, Title: "Concluding Comments", Duration: 180},
		},
	}
	path := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := newServer(path)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/control/adhoc-part", strings.NewReader(`{"title":"Review","seconds":300,"afterTalkId":0}`))
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("adhoc add failed: %d %s", res.Code, res.Body.String())
	}

	// The reconciler runs inside every snapshot; a few of them stand in for
	// the 4 Hz broadcast tick.
	for i := 0; i < 5; i++ {
		srv.snapshot()
	}

	temps := 0
	for _, talk := range srv.talks {
		if talk.Temporary {
			temps++
		}
	}
	if temps != 1 {
		t.Fatalf("expected exactly one temporary part after reconciling, got %d", temps)
	}
	for _, talk := range srv.config.Schedule {
		if talk.Temporary || talk.Title == "Review" {
			t.Fatalf("temporary part leaked into the saved programme: %+v", srv.config.Schedule)
		}
	}
}

// A mistyped or no-longer-needed ad-hoc item can be removed from the picker.
// Saved programme items cannot — those are edited in /setup — and the item
// the clock is actively timing is protected until the timer is idle.
func TestRemovePartDisposesOfTemporaryItemsOnly(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}
	do := func(path, body string, want int) State {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != want {
			t.Fatalf("%s returned %d (want %d): %s", path, res.Code, want, res.Body.String())
		}
		var state State
		if want == http.StatusOK {
			if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
				t.Fatal(err)
			}
		}
		return state
	}

	savedID := srv.talks[0].ID
	do("/api/control/remove-part", fmt.Sprintf(`{"talkId":%d}`, savedID), http.StatusConflict)

	before := len(srv.talks)
	added := do("/api/control/adhoc-part", `{"talkId":0,"title":"Announcements","seconds":300,"afterTalkId":0}`, http.StatusOK)
	tempID := added.Schedule[0].ID
	if !added.Schedule[0].Temporary {
		t.Fatalf("expected a temporary part at the start, got %+v", added.Schedule[0])
	}

	removed := do("/api/control/remove-part", fmt.Sprintf(`{"talkId":%d}`, tempID), http.StatusOK)
	if len(removed.Schedule) != before {
		t.Fatalf("expected the schedule back at %d parts, got %d", before, len(removed.Schedule))
	}

	// Removing the item the clock points at (while idle) hands the selection
	// to the item now in its slot instead of leaving it dangling.
	added = do("/api/control/adhoc-part", `{"title":"Note","seconds":120,"afterTalkId":0}`, http.StatusOK)
	tempID = added.Schedule[0].ID
	do("/api/control/select", fmt.Sprintf(`{"talkId":%d}`, tempID), http.StatusOK)
	after := do("/api/control/remove-part", fmt.Sprintf(`{"talkId":%d}`, tempID), http.StatusOK)
	if after.CurrentTalkID != after.Schedule[0].ID {
		t.Fatalf("expected the selection to land on the first item, got %d", after.CurrentTalkID)
	}
}

// The operator chooses the position before the item exists: 0 pins it to the
// start of the meeting, a talk id puts it right after that item, and either
// way the clock's selection stays wherever it was.
func TestAdhocPartHonoursChosenPosition(t *testing.T) {
	// Pinned to a weekday, as above: the assertions name midweek parts.
	srv := newMidweekServer(t, filepath.Join(t.TempDir(), "config.json"))
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}
	do := func(path, body string) State {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", path, res.Code, res.Body.String())
		}
		var state State
		if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		return state
	}

	secondID := srv.talks[1].ID
	afterSecond := do("/api/control/adhoc-part", fmt.Sprintf(`{"title":"After second","seconds":300,"afterTalkId":%d}`, secondID))
	if afterSecond.Schedule[2].Title != "After second" {
		t.Fatalf("expected the item right after the chosen one, got %+v", afterSecond.Schedule)
	}

	atStart := do("/api/control/adhoc-part", `{"title":"Welcome","seconds":120,"afterTalkId":0}`)
	if atStart.Schedule[0].Title != "Welcome" {
		t.Fatalf("expected the item at the start of the meeting, got %+v", atStart.Schedule)
	}
	if atStart.CurrentTalkTitle != "Opening Comments" {
		t.Fatalf("expected the selection to stay put, got %q", atStart.CurrentTalkTitle)
	}
}

func TestAdhocPartDoesNotInterruptRunningTimer(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}
	savedScheduleLength := len(srv.config.Schedule)
	runtimeScheduleLength := len(srv.talks)

	do := func(path, body string) State {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", path, res.Code, res.Body.String())
		}
		var state State
		if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		return state
	}

	running := do("/api/control/start", "")
	state := do("/api/control/adhoc-part", `{"title":"Elder update","seconds":300}`)

	if state.Status != StatusRunning {
		t.Fatalf("expected timer to keep running, got %q", state.Status)
	}
	if state.CurrentTalkID != running.CurrentTalkID || state.CurrentTalkTitle != running.CurrentTalkTitle {
		t.Fatalf("expected current part to stay %d %q, got %d %q", running.CurrentTalkID, running.CurrentTalkTitle, state.CurrentTalkID, state.CurrentTalkTitle)
	}
	if len(state.Schedule) != runtimeScheduleLength+1 {
		t.Fatalf("expected temporary schedule length %d, got %d", runtimeScheduleLength+1, len(state.Schedule))
	}
	if len(state.Schedule) < 2 || state.Schedule[1].Title != "Elder update" {
		t.Fatalf("expected temporary part to be inserted after current part, got %+v", state.Schedule)
	}
	if len(srv.config.Schedule) != savedScheduleLength {
		t.Fatalf("expected saved schedule to stay at %d parts, got %d", savedScheduleLength, len(srv.config.Schedule))
	}
}

func TestSaveConfigPreservesTemporaryParts(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}
	pinMidweek(t, srv)

	do := func(path, body string) State {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", path, res.Code, res.Body.String())
		}
		var state State
		if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		return state
	}

	added := do("/api/control/adhoc-part", `{"title":"Temporary note","seconds":300}`)
	if len(added.Schedule) < 2 || !added.Schedule[1].Temporary {
		t.Fatalf("expected temporary part in live schedule, got %+v", added.Schedule)
	}

	config := Config{
		DeviceName:       "Hall Clock",
		MeetingType:      "midweek",
		MeetingStartTime: "19:00",
		MeetingStarts:    defaultMeetingStarts("19:00"),
		PrestartSeconds:  300,
		Schedule: []Talk{
			{Title: "Opening Comments", Duration: 60, Closing: 30},
			{Title: "Treasures From God's Word", Duration: 600, Closing: 120},
		},
	}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	res := do("/api/config", string(body))

	if len(res.Schedule) != 3 {
		t.Fatalf("expected temporary part to survive config save, got %+v", res.Schedule)
	}
	if !res.Schedule[1].Temporary {
		t.Fatalf("expected temporary part to stay in its slot after config save, got %+v", res.Schedule)
	}
}

// A saved config carrying a stale meeting type (the operator last applied the
// weekend template) must not overwrite the calendar-derived active meeting
// type, or the edited schedule is silently dropped for a running timer.
func TestSaveConfigUpdatesRunningCurrentDurationWithStaleMeetingType(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 7, 9, 19, 0, 0, 0, time.UTC)
	srv.clock = func() time.Time { return fixed }
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	do := func(path, body string) State {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", path, res.Code, res.Body.String())
		}
		var state State
		if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		return state
	}

	do("/api/control/start", "")

	config := Config{
		DeviceName:       "Hall Clock",
		MeetingType:      "weekend",
		MeetingStartTime: "19:00",
		MeetingStarts:    defaultMeetingStarts("19:00"),
		PrestartSeconds:  300,
		Schedule: []Talk{
			{Title: "Opening Comments", Duration: 120, Closing: 30},
			{Title: "Treasures From God's Word", Duration: 600, Closing: 120},
		},
	}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	res := do("/api/config", string(body))

	if res.MeetingType != "midweek" {
		t.Fatalf("expected active meeting type to stay calendar-derived, got %q", res.MeetingType)
	}
	if res.DurationSeconds != 120 || res.RemainingSeconds != 120 {
		t.Fatalf("expected running timer to adopt edited duration, got duration=%d remaining=%d", res.DurationSeconds, res.RemainingSeconds)
	}
	if res.Schedule[0].Duration != 120 {
		t.Fatalf("expected control schedule to reflect edited minutes, got %+v", res.Schedule[0])
	}
}

func TestNormalizeSchedule(t *testing.T) {
	schedule := []Talk{
		{Title: "  ", Duration: 10, Closing: 500},
		{Title: "Talk", Duration: 9000, Closing: -20},
	}

	normalizeSchedule(schedule)

	if schedule[0].ID != 1 || schedule[0].Title != "Part 1" {
		t.Fatalf("unexpected first talk: %+v", schedule[0])
	}
	if schedule[0].Duration != 60 || schedule[0].Closing != 60 {
		t.Fatalf("unexpected first talk timing: %+v", schedule[0])
	}
	if schedule[1].ID != 2 || schedule[1].Duration != 7200 || schedule[1].Closing != 0 {
		t.Fatalf("unexpected second talk: %+v", schedule[1])
	}
}

func TestAutomaticScheduleSwitchesByWeekendDay(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	at := time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC) // Sunday
	srv.syncActiveScheduleLocked(at, srv.meetingInProgressLocked(at))
	if srv.state.MeetingType != "weekend" {
		t.Fatalf("expected weekend meeting type, got %q", srv.state.MeetingType)
	}
	if len(srv.talks) != 2 {
		t.Fatalf("expected 2 weekend parts, got %d", len(srv.talks))
	}
	if srv.talks[0].Duration != 1800 || srv.talks[1].Duration != 3600 {
		t.Fatalf("unexpected weekend schedule: %+v", srv.talks)
	}

	at = time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC) // Monday
	srv.syncActiveScheduleLocked(at, srv.meetingInProgressLocked(at))
	if srv.state.MeetingType != "midweek" {
		t.Fatalf("expected midweek meeting type, got %q", srv.state.MeetingType)
	}
	if len(srv.talks) != len(defaultSchedule()) {
		t.Fatalf("expected midweek schedule, got %+v", srv.talks)
	}
}

func TestDefaultMidweekTemplateUsesCorrectOpeningAndClosing(t *testing.T) {
	schedule := defaultSchedule()
	if schedule[0].Title != "Opening Comments" || schedule[0].Duration != 60 {
		t.Fatalf("unexpected opening part: %+v", schedule[0])
	}

	last := schedule[len(schedule)-1]
	if last.Title != "Concluding Comments" || last.Duration != 180 {
		t.Fatalf("unexpected concluding part: %+v", last)
	}
}

func TestPrestartRemaining(t *testing.T) {
	now := time.Date(2026, 7, 6, 18, 56, 30, 0, time.Local)
	remaining, label, startTime, ok := prestartRemaining(now, []MeetingStart{
		{Day: int(time.Monday), Time: "19:00", Congregation: "Main"},
	}, 300)
	if !ok {
		t.Fatal("expected prestart countdown to be active")
	}
	if remaining != 210 {
		t.Fatalf("expected 210 seconds remaining, got %d", remaining)
	}
	if label != "Main" || startTime != "19:00" {
		t.Fatalf("unexpected slot metadata: %q %q", label, startTime)
	}
}

func TestPrestartRemainingOutsideWindow(t *testing.T) {
	starts := []MeetingStart{{Day: int(time.Monday), Time: "19:00"}}
	beforeWindow := time.Date(2026, 7, 6, 18, 54, 59, 0, time.Local)
	if _, _, _, ok := prestartRemaining(beforeWindow, starts, 300); ok {
		t.Fatal("did not expect countdown before prestart window")
	}

	atStart := time.Date(2026, 7, 6, 19, 0, 0, 0, time.Local)
	if _, _, _, ok := prestartRemaining(atStart, starts, 300); ok {
		t.Fatal("did not expect countdown once meeting start time is reached")
	}
}

func TestPrestartRemainingChoosesNextTodaySlot(t *testing.T) {
	now := time.Date(2026, 7, 6, 19, 27, 0, 0, time.Local)
	remaining, label, startTime, ok := prestartRemaining(now, []MeetingStart{
		{Day: int(time.Monday), Time: "19:00", Congregation: "Earlier"},
		{Day: int(time.Monday), Time: "19:30", Congregation: "Second Congregation"},
		{Day: int(time.Tuesday), Time: "19:30", Congregation: "Wrong Day"},
	}, 300)
	if !ok {
		t.Fatal("expected second Monday slot to be active")
	}
	if remaining != 180 || label != "Second Congregation" || startTime != "19:30" {
		t.Fatalf("unexpected countdown slot: remaining=%d label=%q time=%q", remaining, label, startTime)
	}
}

func TestDefaultMeetingStartsCoverWeekdaysAndSunday(t *testing.T) {
	starts := defaultMeetingStarts("19:30")
	if len(starts) != 6 {
		t.Fatalf("expected 6 default starts, got %d", len(starts))
	}
	if starts[0].Day != int(time.Sunday) || starts[0].Time != "10:00" {
		t.Fatalf("expected Sunday 10:00 first, got %+v", starts[0])
	}
	if starts[1].Day != int(time.Monday) || starts[1].Time != "19:30" {
		t.Fatalf("unexpected first weekday start: %+v", starts[1])
	}
	if starts[5].Day != int(time.Friday) {
		t.Fatalf("unexpected last start: %+v", starts[5])
	}
}

func TestNormalizeMeetingStartsKeepsMultipleTimesPerDaySorted(t *testing.T) {
	starts := normalizeMeetingStarts([]MeetingStart{
		{Day: int(time.Monday), Time: "19:30", Congregation: "Evening"},
		{Day: int(time.Monday), Time: "9:30", Congregation: "Morning"},
		{Day: int(time.Sunday), Time: "10:00"},
	}, "19:00")

	if len(starts) != 3 {
		t.Fatalf("expected all 3 starts to survive, got %d: %+v", len(starts), starts)
	}
	if starts[0].Day != int(time.Sunday) {
		t.Fatalf("expected Sunday first after sorting, got %+v", starts[0])
	}
	if starts[1].Time != "09:30" || starts[1].Congregation != "Morning" {
		t.Fatalf("expected padded morning slot second, got %+v", starts[1])
	}
	if starts[2].Time != "19:30" || starts[2].ID != 3 {
		t.Fatalf("expected evening slot last with ID 3, got %+v", starts[2])
	}
}

func TestWeekendTemplateAddsWeekendStartWhenMissing(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.config.MeetingStarts = []MeetingStart{
		{ID: 1, Day: int(time.Monday), Time: "19:00"},
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/template/weekend", nil)
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected OK response, got %d: %s", res.Code, res.Body.String())
	}

	var state State
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if !hasWeekendStart(state.MeetingStarts) {
		t.Fatalf("expected a weekend start to be added, got %+v", state.MeetingStarts)
	}
	if len(state.MeetingStarts) != 2 {
		t.Fatalf("expected weekday start to be kept alongside, got %+v", state.MeetingStarts)
	}
}

func TestNormalizeStartTime(t *testing.T) {
	if got := normalizeStartTime("09:30"); got != "09:30" {
		t.Fatalf("expected valid time to be preserved, got %q", got)
	}
	if got := normalizeStartTime("bad"); got != "19:00" {
		t.Fatalf("expected invalid time to fall back, got %q", got)
	}
}

func TestWeeklyMeetingsURL(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	got := weeklyMeetingsURL("https://wol.jw.org/en/wol/d/r1/lp-e/202026241", now)
	if got != "https://wol.jw.org/en/wol/meetings/r1/lp-e/2026/28" {
		t.Fatalf("unexpected weekly URL: %s", got)
	}

	got = weeklyMeetingsURL("", now)
	if got != "https://wol.jw.org/en/wol/meetings/r1/lp-e/2026/28" {
		t.Fatalf("expected English defaults without an example URL, got %s", got)
	}

	got = weeklyMeetingsURL("https://wol.jw.org/es/wol/d/r4/lp-s/202026241", now)
	if got != "https://wol.jw.org/es/wol/meetings/r4/lp-s/2026/28" {
		t.Fatalf("expected language segments to carry over, got %s", got)
	}

	got = weeklyMeetingsURL("https://wol.jw.org/tw/wol/d/r33/lp-tw/202026241", now)
	if got != "https://wol.jw.org/tw/wol/meetings/r33/lp-tw/2026/28" {
		t.Fatalf("expected Twi language segments to carry over, got %s", got)
	}
}

func TestFindWorkbookDocURL(t *testing.T) {
	page := `
		<a href="/en/wol/d/r1/lp-e/2026400">Watchtower study article</a>
		<a href="/en/wol/d/r1/lp-e/202026241">Midweek workbook</a>
	`
	got, ok := findWorkbookDocURL(page)
	if !ok {
		t.Fatal("expected to find workbook link")
	}
	if got != "https://wol.jw.org/en/wol/d/r1/lp-e/202026241" {
		t.Fatalf("expected 9-digit workbook docid, got %s", got)
	}

	if _, ok := findWorkbookDocURL(`<a href="/en/wol/d/r1/lp-e/2026400">Watchtower</a>`); ok {
		t.Fatal("did not expect a match without a workbook link")
	}
}

func TestAutoImportTickSkipsWhenDisabledOrCurrent(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	before := srv.snapshot().Schedule

	// The enabled tick legitimately sweeps every offered language, so play an
	// offline hall: nothing fetched may mean anything changed.
	originalFetch := fetchWOLPageFunc
	fetchWOLPageFunc = func(ctx context.Context, sourceURL string) (string, error) {
		return "", fmt.Errorf("offline: %s", sourceURL)
	}
	defer func() { fetchWOLPageFunc = originalFetch }()

	// Disabled: must not touch anything.
	srv.mu.Lock()
	srv.config.AutoImportMidweek = false
	srv.mu.Unlock()
	srv.autoImportTick(t.Context(), now)

	// Enabled but already imported this week: must return before fetching.
	srv.mu.Lock()
	srv.config.AutoImportMidweek = true
	srv.config.MidweekImportedWeek = isoWeekString(now)
	srv.mu.Unlock()
	srv.autoImportTick(t.Context(), now)

	after := srv.snapshot().Schedule
	if len(after) != len(before) {
		t.Fatalf("expected schedule to be untouched, got %d parts", len(after))
	}
}

func TestNextAutoImportSourceUsesUpcomingMeetingLanguage(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 6, 3, 5, 0, 0, time.UTC)
	currentWeek := isoWeekString(now)
	srv.mu.Lock()
	srv.config.AutoImportMidweek = true
	srv.config.MidweekURL = "https://wol.jw.org/en/wol/d/r1/lp-e/202026241"
	srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{
		{
			Day:          int(time.Monday),
			Time:         "19:00",
			Congregation: "Spanish",
			MidweekURL:   "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
		},
		{
			Day:                 int(time.Tuesday),
			Time:                "19:00",
			Congregation:        "Twi",
			MidweekURL:          "https://wol.jw.org/tw/wol/d/r33/lp-tw/202026241",
			MidweekImportedWeek: currentWeek,
		},
	}, "19:00")
	source, due := srv.nextAutoImportSourceLocked(now)
	srv.mu.Unlock()

	if !due {
		t.Fatal("expected upcoming Spanish meeting to be due")
	}
	if source.exampleURL != "https://wol.jw.org/es/wol/d/r4/lp-s/202026241" {
		t.Fatalf("expected Spanish source, got %s", source.exampleURL)
	}
}

func TestNextAutoImportSourceUsesStartLanguageWithoutURL(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 6, 3, 5, 0, 0, time.UTC)
	srv.mu.Lock()
	srv.config.AutoImportMidweek = true
	srv.config.MidweekURL = "https://wol.jw.org/en/wol/d/r1/lp-e/202026241"
	srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{
		{
			Day:      int(time.Monday),
			Time:     "19:00",
			Language: "tw",
		},
	}, "19:00")
	source, due := srv.nextAutoImportSourceLocked(now)
	srv.mu.Unlock()

	if !due {
		t.Fatal("expected upcoming Twi meeting to be due")
	}
	if source.exampleURL != defaultMidweekLanguageSources["tw"] {
		t.Fatalf("expected default Twi source, got %s", source.exampleURL)
	}
}

func TestNextAutoImportSourceWaitsForCurrentMeetingBeforeFutureLanguage(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 6, 4, 0, 0, 0, time.UTC)
	currentWeek := isoWeekString(now)
	srv.mu.Lock()
	srv.config.AutoImportMidweek = true
	srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{
		{
			Day:                 int(time.Monday),
			Time:                "19:00",
			Congregation:        "Spanish",
			MidweekURL:          "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
			MidweekImportedWeek: currentWeek,
		},
		{
			Day:          int(time.Tuesday),
			Time:         "19:00",
			Congregation: "Twi",
			MidweekURL:   "https://wol.jw.org/tw/wol/d/r33/lp-tw/202026241",
		},
	}, "19:00")
	_, due := srv.nextAutoImportSourceLocked(now)
	srv.mu.Unlock()

	if due {
		t.Fatal("did not expect Tuesday Twi import before Monday meeting has passed")
	}
}

func TestNextAutoImportSourceDoesNotOverwriteDuringCurrentMeeting(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 6, 20, 0, 0, 0, time.UTC)
	currentWeek := isoWeekString(now)
	srv.mu.Lock()
	srv.config.AutoImportMidweek = true
	srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{
		{
			Day:                 int(time.Monday),
			Time:                "19:00",
			Congregation:        "Spanish",
			MidweekURL:          "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
			MidweekImportedWeek: currentWeek,
		},
		{
			Day:          int(time.Tuesday),
			Time:         "19:00",
			Congregation: "Twi",
			MidweekURL:   "https://wol.jw.org/tw/wol/d/r33/lp-tw/202026241",
		},
	}, "19:00")
	_, due := srv.nextAutoImportSourceLocked(now)
	srv.mu.Unlock()

	if due {
		t.Fatal("did not expect next language import during the current meeting window")
	}
}

func TestAutoImportNextCheckStaysInCurrentWeekForLaterLanguages(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 6, 3, 5, 0, 0, time.UTC)
	currentWeek := isoWeekString(now)
	srv.mu.Lock()
	srv.config.AutoImportMidweek = true
	srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{
		{
			Day:                 int(time.Monday),
			Time:                "19:00",
			Congregation:        "Spanish",
			MidweekURL:          "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
			MidweekImportedWeek: currentWeek,
		},
		{
			Day:          int(time.Tuesday),
			Time:         "19:00",
			Congregation: "Twi",
			MidweekURL:   "https://wol.jw.org/tw/wol/d/r33/lp-tw/202026241",
		},
	}, "19:00")
	next := srv.nextAutoImportCheckAtLocked(now)
	srv.mu.Unlock()

	want := time.Date(2026, 7, 6, 4, 5, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("expected same-week retry check, got %s", next)
	}
}

func TestAutoImportAllowsNextLanguageAtBackToBackStartBoundary(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	currentWeek := isoWeekString(time.Date(2026, 7, 6, 20, 0, 0, 0, time.UTC))
	srv.mu.Lock()
	srv.config.AutoImportMidweek = true
	srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{
		{
			Day:                 int(time.Monday),
			Time:                "19:00",
			Congregation:        "Spanish",
			MidweekURL:          "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
			MidweekImportedWeek: currentWeek,
		},
		{
			Day:          int(time.Monday),
			Time:         "21:00",
			Congregation: "Twi",
			MidweekURL:   "https://wol.jw.org/tw/wol/d/r33/lp-tw/202026241",
		},
	}, "19:00")
	next := srv.nextAutoImportCheckAtLocked(time.Date(2026, 7, 6, 20, 0, 0, 0, time.UTC))
	source, due := srv.nextAutoImportSourceLocked(time.Date(2026, 7, 6, 21, 0, 0, 0, time.UTC))
	srv.mu.Unlock()

	if want := time.Date(2026, 7, 6, 21, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("expected check at next same-day start boundary, got %s", next)
	}
	if !due || source.exampleURL != "https://wol.jw.org/tw/wol/d/r33/lp-tw/202026241" {
		t.Fatalf("expected Twi source to be due at boundary, due=%v source=%+v", due, source)
	}
}

func TestAutoImportApplyRechecksMeetingWindowAfterFetch(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	beforeStart := time.Date(2026, 7, 6, 18, 59, 0, 0, time.UTC)
	afterStart := time.Date(2026, 7, 6, 19, 1, 0, 0, time.UTC)
	srv.mu.Lock()
	srv.config.AutoImportMidweek = true
	srv.config.MeetingStarts = normalizeMeetingStarts([]MeetingStart{
		{
			Day:          int(time.Monday),
			Time:         "19:00",
			Congregation: "Spanish",
			MidweekURL:   "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
		},
	}, "19:00")
	source, due := srv.nextAutoImportSourceLocked(beforeStart)
	if !due {
		t.Fatal("expected source to be due before meeting start")
	}
	before := append([]Talk(nil), srv.config.Schedule...)
	_, _, ok := srv.applyAutoImportedScheduleLocked(
		afterStart,
		source,
		"https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
		[]Talk{{ID: 1, Title: "Comentarios de introducción", Duration: 60, Closing: 30}},
	)
	after := append([]Talk(nil), srv.config.Schedule...)
	srv.mu.Unlock()

	if ok {
		t.Fatal("expected stale auto-import apply to be rejected after meeting started")
	}
	if len(after) != len(before) || after[0].Title != before[0].Title {
		t.Fatalf("expected schedule to remain unchanged, got %+v", after)
	}
}

func TestMidweekLanguageSourceUsesConfiguredAndDefaultSources(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	srv.mu.Lock()
	srv.config.MidweekLanguageSources = map[string]string{
		"es": "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
	}
	spanish, spanishOK := srv.midweekLanguageSourceLocked("spanish")
	twi, twiOK := srv.midweekLanguageSourceLocked("twi")
	srv.mu.Unlock()

	if !spanishOK || spanish != "https://wol.jw.org/es/wol/d/r4/lp-s/202026241" {
		t.Fatalf("expected configured Spanish source, got ok=%v source=%q", spanishOK, spanish)
	}
	if !twiOK || twi != "https://wol.jw.org/tw/wol/d/r33/lp-tw/202026241" {
		t.Fatalf("expected default Twi source, got ok=%v source=%q", twiOK, twi)
	}
}

func TestValidateImportedLanguageRejectsEnglishTitlesForTwi(t *testing.T) {
	schedule := []Talk{
		{Title: "Opening Comments", Duration: 60},
		{Title: "Spiritual Gems", Duration: 600},
		{Title: "Bible Reading", Duration: 240},
	}

	if err := validateImportedLanguage("tw", schedule); err == nil {
		t.Fatal("expected Twi import with English titles to be rejected")
	}
	if err := validateImportedLanguage("en", schedule); err != nil {
		t.Fatalf("did not expect English import to be rejected: %v", err)
	}
}

func TestNewServerMigratesLegacyConfigToAutoImportOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	config := Config{
		DeviceName:        "Hall Clock",
		MeetingType:       "midweek",
		MeetingStartTime:  "19:00",
		MeetingStarts:     defaultMeetingStarts("19:00"),
		PrestartSeconds:   300,
		AutoImportMidweek: false,
		Schedule:          defaultSchedule(),
	}
	if err := saveConfig(path, config); err != nil {
		t.Fatal(err)
	}

	srv, err := newServer(path)
	if err != nil {
		t.Fatal(err)
	}
	if !srv.config.AutoImportMidweek {
		t.Fatal("expected legacy config to migrate auto-import on")
	}
	if srv.config.Version != currentConfigVersion {
		t.Fatalf("expected config version %d, got %d", currentConfigVersion, srv.config.Version)
	}
}

func TestNewServerPreservesExplicitAutoImportOffAfterMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	config := Config{
		Version:           currentConfigVersion,
		DeviceName:        "Hall Clock",
		MeetingType:       "midweek",
		MeetingStartTime:  "19:00",
		MeetingStarts:     defaultMeetingStarts("19:00"),
		PrestartSeconds:   300,
		AutoImportMidweek: false,
		Schedule:          defaultSchedule(),
	}
	if err := saveConfig(path, config); err != nil {
		t.Fatal(err)
	}

	srv, err := newServer(path)
	if err != nil {
		t.Fatal(err)
	}
	if srv.config.AutoImportMidweek {
		t.Fatal("expected explicit auto-import off to be preserved")
	}
}

func TestAutoImportScheduleRunsMondayAtThree(t *testing.T) {
	loc := time.FixedZone("test", -5*60*60)
	week := ""

	before := time.Date(2026, 7, 6, 2, 59, 0, 0, loc)
	if shouldAutoImportNow(before, true, week) {
		t.Fatal("did not expect import before Monday 3:00 AM")
	}
	if got := nextAutoImportAt(before); !got.Equal(time.Date(2026, 7, 6, 3, 0, 0, 0, loc)) {
		t.Fatalf("unexpected next import time before 3 AM: %s", got)
	}

	at := time.Date(2026, 7, 6, 3, 0, 0, 0, loc)
	if !shouldAutoImportNow(at, true, week) {
		t.Fatal("expected import at Monday 3:00 AM")
	}
	if got := nextAutoImportAt(at); !got.Equal(time.Date(2026, 7, 13, 3, 0, 0, 0, loc)) {
		t.Fatalf("unexpected next import time after 3 AM: %s", got)
	}

	after := time.Date(2026, 7, 7, 9, 30, 0, 0, loc)
	if !shouldAutoImportNow(after, true, week) {
		t.Fatal("expected missed weekly import to remain due after Monday 3:00 AM")
	}
	if shouldAutoImportNow(after, true, isoWeekString(after)) {
		t.Fatal("did not expect import after current week was already imported")
	}
}

func TestParseMidweekTimings(t *testing.T) {
	input := `
		<h2>July 13-19</h2>
		<p>Song 34 and Prayer | Opening Comments (1 min.)</p>
		<p>It Matters Whom We Trust (10 min.)</p>
		<p>Spiritual Gems (10 min.)</p>
		<p>Bible Reading (4 min.)</p>
		<p>Initial Call (3 min.)</p>
		<p>It Matters Whom We Trust (10 min.)</p>
	`

	schedule, err := parseMidweekTimings(input)
	if err != nil {
		t.Fatal(err)
	}

	if len(schedule) != 5 {
		t.Fatalf("expected 5 unique timing slots, got %d: %+v", len(schedule), schedule)
	}
	if schedule[0].Title != "Opening Comments" || schedule[0].Duration != 60 {
		t.Fatalf("unexpected first slot: %+v", schedule[0])
	}
	if schedule[3].Title != "Bible Reading" || schedule[3].Duration != 240 {
		t.Fatalf("unexpected bible reading slot: %+v", schedule[3])
	}
}

func TestParseMidweekTimingsSupportsTranslatedTimingLines(t *testing.T) {
	input := `
		<h3>Jehová merece que le obedezcamos</h3>
		<p>(10 mins.)</p>
		<h3>Busquemos perlas escondidas</h3>
		<p>(10 mins.)</p>
		<h3>Lectura de la Biblia</h3>
		<p>(4 mins.) Jer 13:1-14</p>
		<h3>Ɔkasa</h3>
		<p>(simma 5)</p>
	`

	schedule, err := parseMidweekTimings(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule) != 4 {
		t.Fatalf("expected 4 translated timing slots, got %d: %+v", len(schedule), schedule)
	}
	if schedule[0].Title != "Jehová merece que le obedezcamos" || schedule[0].Duration != 600 {
		t.Fatalf("unexpected Spanish slot: %+v", schedule[0])
	}
	if schedule[3].Title != "Ɔkasa" || schedule[3].Duration != 300 {
		t.Fatalf("unexpected Twi-style slot: %+v", schedule[3])
	}
}

func TestParseMidweekTimingsRejectsEmptyInput(t *testing.T) {
	if _, err := parseMidweekTimings("No timing data here"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestImportMidweekTextEndpointUsesBackendParser(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/import/midweek-text",
		strings.NewReader(`{"text":"Song 1 and Prayer | Opening Comments (1 min.)\nBible Reading (4 min.)"}`),
	)
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected OK response, got %d: %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), "Song 1") {
		t.Fatalf("expected cleaned opening title, got %s", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "Opening Comments") {
		t.Fatalf("expected opening comments in response, got %s", res.Body.String())
	}
}

func TestReadLimitedStringRejectsOversizedImport(t *testing.T) {
	_, err := readLimitedString(strings.NewReader("123456"), 5)
	if err == nil {
		t.Fatal("expected oversized import error")
	}
}

func TestParseMidweekTimingsFromDownloadedFixture(t *testing.T) {
	path := os.Getenv("WALL_CLOCK_WOL_FIXTURE")
	if path == "" {
		t.Skip("set WALL_CLOCK_WOL_FIXTURE to validate against a downloaded WOL page")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := parseMidweekTimings(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule) < 8 {
		t.Fatalf("expected at least 8 timing slots, got %d: %+v", len(schedule), schedule)
	}
	t.Logf("parsed schedule: %+v", schedule)
}

// pinMidweek freezes the server clock to a fixed weekday evening so that
// schedule behaviour is deterministic regardless of the real day the suite
// runs on (otherwise the automatic weekend switch drops temporary parts on
// Sat/Sun), then reconciles state to the midweek schedule.
func pinMidweek(t *testing.T, srv *server) {
	t.Helper()
	fixed := time.Date(2026, 7, 8, 19, 45, 0, 0, time.UTC) // Wednesday, just after the 19:30 start
	srv.clock = func() time.Time { return fixed }
	srv.mu.Lock()
	srv.syncActiveScheduleLocked(fixed, srv.meetingInProgressLocked(fixed))
	srv.mu.Unlock()
}

func addTemporaryPart(t *testing.T, srv *server, mux http.Handler, title string) {
	t.Helper()
	body := fmt.Sprintf(`{"title":%q,"seconds":300}`, title)
	req := httptest.NewRequest(http.MethodPost, "/api/control/adhoc-part", strings.NewReader(body))
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected OK adhoc response, got %d: %s", res.Code, res.Body.String())
	}
}

func backdateTemporaryParts(srv *server, createdAt time.Time) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for i := range srv.talks {
		if srv.talks[i].Temporary {
			srv.talks[i].CreatedAt = createdAt
		}
	}
}

func TestStaleTemporaryPartPurgedWhenNextMeetingStarts(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	addTemporaryPart(t, srv, mux, "Previous congregation's announcement")
	backdateTemporaryParts(srv, time.Now().AddDate(0, 0, -8))

	state := srv.snapshot()
	for _, talk := range state.Schedule {
		if talk.Temporary {
			t.Fatalf("expected stale temporary part to be purged, got %+v", state.Schedule)
		}
	}
	found := false
	for _, talk := range state.Schedule {
		if talk.ID == state.CurrentTalkID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a valid current talk after purge, got id %d", state.CurrentTalkID)
	}
}

func TestTemporaryPartAddedDuringPrestartSurvivesMeetingStart(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	addTemporaryPart(t, srv, mux, "Opening announcement")
	sessionStart, ok := latestMeetingStart(time.Now(), srv.config.MeetingStarts)
	if !ok {
		t.Fatal("expected a recent meeting start in the default config")
	}
	backdateTemporaryParts(srv, sessionStart.Add(-time.Minute))

	state := srv.snapshot()
	hasTemp := false
	for _, talk := range state.Schedule {
		if talk.Temporary {
			hasTemp = true
			break
		}
	}
	if !hasTemp {
		t.Fatalf("expected prestart temporary part to survive the meeting start, got %+v", state.Schedule)
	}
}

func TestStaleTemporaryPartKeptWhileTimerRunning(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	addTemporaryPart(t, srv, mux, "Held over part")

	req := httptest.NewRequest(http.MethodPost, "/api/control/start", nil)
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected OK start response, got %d: %s", res.Code, res.Body.String())
	}

	backdateTemporaryParts(srv, time.Now().AddDate(0, 0, -8))

	state := srv.snapshot()
	hasTemp := false
	for _, talk := range state.Schedule {
		if talk.Temporary {
			hasTemp = true
			break
		}
	}
	if !hasTemp {
		t.Fatalf("expected temporary part to stay while the timer runs, got %+v", state.Schedule)
	}
}

func TestMergeTemporaryPartsKeepsTrailingTempWhenBaseShrinks(t *testing.T) {
	base := []Talk{{ID: 1, Title: "Part 1", Duration: 300}}
	existing := []Talk{
		{ID: 1, Title: "Old 1", Duration: 300},
		{ID: 2, Title: "Old 2", Duration: 300},
		{ID: 3, Title: "Old 3", Duration: 300},
		{ID: 4, Title: "Ad hoc", Duration: 300, Temporary: true},
	}
	merged := mergeTemporaryParts(base, existing)
	if len(merged) != 2 {
		t.Fatalf("expected base part plus temporary part, got %+v", merged)
	}
	if !merged[1].Temporary || merged[1].Title != "Ad hoc" {
		t.Fatalf("expected trailing temporary part to survive a shorter base schedule, got %+v", merged)
	}
}

func TestSetupResponsesReturnSavedScheduleNotRuntime(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	addTemporaryPart(t, srv, mux, "Live ad hoc part")

	req := httptest.NewRequest(http.MethodPost, "/api/template/midweek", nil)
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected OK template response, got %d: %s", res.Code, res.Body.String())
	}

	var state State
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	saved := append([]Talk(nil), srv.config.Schedule...)
	srv.mu.Unlock()
	if len(state.Schedule) != len(saved) {
		t.Fatalf("expected setup response to carry the saved schedule (%d parts), got %d: %+v", len(saved), len(state.Schedule), state.Schedule)
	}
	for _, talk := range state.Schedule {
		if talk.Temporary {
			t.Fatalf("expected no temporary parts in setup response, got %+v", state.Schedule)
		}
	}
}

func TestSaveConfigDefaultsStartLanguageToConfiguredLanguage(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.config.MidweekLanguage = "es"
	srv.config.MidweekURL = "https://wol.jw.org/es/wol/d/r4/lp-s/202026241"
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	body := `{
		"deviceName":"Hall Clock",
		"meetingType":"midweek",
		"meetingStartTime":"19:00",
		"meetingStarts":[{"day":1,"time":"19:00"}],
		"prestartSeconds":300,
		"midweekUrl":"https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
		"schedule":[{"title":"Part 1","durationSeconds":300,"closingSeconds":60}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected OK save response, got %d: %s", res.Code, res.Body.String())
	}

	srv.mu.Lock()
	starts := append([]MeetingStart(nil), srv.config.MeetingStarts...)
	srv.mu.Unlock()
	if len(starts) != 1 || starts[0].Language != "es" {
		t.Fatalf("expected missing start language to inherit Spanish, got %+v", starts)
	}
}

func TestSaveConfigStripsTemporaryFlag(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	body := `{"deviceName":"Hall","schedule":[{"title":"Part 1","durationSeconds":300,"closingSeconds":60,"temporary":true}],"meetingStarts":[{"day":1,"time":"19:00"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected OK save response, got %d: %s", res.Code, res.Body.String())
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, talk := range srv.config.Schedule {
		if talk.Temporary {
			t.Fatalf("expected saved config schedule to have no temporary parts, got %+v", srv.config.Schedule)
		}
	}
}

func TestUnrelatedConfigSaveKeepsSelectionWithTemporaryPart(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}
	pinMidweek(t, srv)

	addTemporaryPart(t, srv, mux, "Staged part")
	srv.mu.Lock()
	stagedID := srv.state.CurrentTalkID
	config := srv.config
	srv.mu.Unlock()

	payload, err := json.Marshal(map[string]any{
		"deviceName":    "Renamed Hall",
		"meetingType":   config.MeetingType,
		"meetingStarts": config.MeetingStarts,
		"schedule":      config.Schedule,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(string(payload)))
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected OK save response, got %d: %s", res.Code, res.Body.String())
	}

	var state State
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.CurrentTalkID != stagedID {
		t.Fatalf("expected selection to stay on part %d after unrelated config save, got %d", stagedID, state.CurrentTalkID)
	}
}

func TestMeetingTypeSwitchDropsTemporaryParts(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	addTemporaryPart(t, srv, mux, "Yesterday's part")

	srv.mu.Lock()
	if meetingTypeForTime(time.Now()) == "weekend" {
		srv.state.MeetingType = "midweek"
	} else {
		srv.state.MeetingType = "weekend"
	}
	srv.mu.Unlock()

	state := srv.snapshot()
	for _, talk := range state.Schedule {
		if talk.Temporary {
			t.Fatalf("expected temporary parts to be dropped on meeting-type switch, got %+v", state.Schedule)
		}
	}
}

func TestCircuitOverseerScheduleTransforms(t *testing.T) {
	// Weekend CO: three 30-minute parts.
	wk := scheduleForMeetingType("weekend", nil, true, "en")
	if len(wk) != 3 {
		t.Fatalf("expected 3 weekend CO parts, got %d: %+v", len(wk), wk)
	}
	if wk[0].Title != "Public Talk" || wk[1].Title != "Watchtower Summary" || wk[2].Title != "Service Talk" {
		t.Fatalf("unexpected weekend CO titles: %+v", wk)
	}
	for _, p := range wk {
		if p.Duration != 1800 {
			t.Fatalf("expected 30-minute weekend CO parts, got %+v", p)
		}
	}

	// Midweek CO: Congregation Bible Study becomes the CO service talk, rest intact.
	base := defaultSchedule()
	mid := scheduleForMeetingType("midweek", base, true, "en")
	if len(mid) != len(base) {
		t.Fatalf("expected midweek CO to keep part count %d, got %d", len(base), len(mid))
	}
	hasCO, hasCBS := false, false
	for _, p := range mid {
		if p.Title == "Service Talk" {
			hasCO = true
		}
		if strings.Contains(strings.ToLower(p.Title), "bible study") {
			hasCBS = true
		}
	}
	if !hasCO || hasCBS {
		t.Fatalf("midweek CO should replace Bible Study with CO talk: %+v", mid)
	}
	// The saved schedule must not be mutated.
	if strings.Contains(strings.ToLower(base[6].Title), "bible study") == false {
		t.Fatalf("base schedule was mutated: %+v", base)
	}

	// Off = unchanged.
	if len(scheduleForMeetingType("weekend", nil, false, "en")) != 2 {
		t.Fatal("weekend without CO should be 2 parts")
	}
}

func TestWeekendScheduleFollowsLanguage(t *testing.T) {
	es := weekendSchedule("es")
	if es[0].Title != "Discurso público" || es[1].Title != "Estudio de La Atalaya" {
		t.Fatalf("unexpected Spanish weekend titles: %+v", es)
	}
	tw := circuitOverseerWeekendSchedule("tw")
	if tw[0].Title != "Baguam Kasa" || tw[1].Title != "Ɔwɛn-Aban Adesua" || tw[2].Title != "Ɔsom Kasa" {
		t.Fatalf("unexpected Twi weekend CO titles: %+v", tw)
	}
	// An unknown or unrecorded language falls back to English.
	if weekendSchedule("")[0].Title != "Public Talk" {
		t.Fatal("expected English weekend fallback")
	}
	// A translated weekend template must still be recognized as one, or it
	// would survive in config.Schedule as a bogus midweek program.
	for _, language := range []string{"en", "es", "tw"} {
		weekend := weekendSchedule(language)
		normalizeSchedule(weekend)
		if !isWeekendSchedule(weekend) {
			t.Fatalf("%s weekend template not recognized: %+v", language, weekend)
		}
	}
	if isWeekendSchedule(defaultSchedule()) {
		t.Fatal("midweek schedule misidentified as weekend")
	}
}

// Titles here are the ones WOL actually publishes for these languages; CO mode
// has to find the Bible Study part without relying on the English wording.
func TestCircuitOverseerMidweekScheduleNonEnglish(t *testing.T) {
	cases := []struct {
		language   string
		bibleStudy string
		wantTitle  string
	}{
		{"es", "Estudio bíblico de la congregación", "Discurso de servicio"},
		{"tw", "Asafo Bible Adesua", "Ɔsom Kasa"},
		// Older configs never recorded the language; infer it from the title.
		{"", "Estudio bíblico de la congregación", "Discurso de servicio"},
		// Stale config language, English schedule: the program must read as one
		// language, so the title wins over the flag.
		{"es", "Congregation Bible Study", "Service Talk"},
		{"tw", "Congregation Bible Study", "Service Talk"},
	}
	for _, tc := range cases {
		t.Run(tc.language, func(t *testing.T) {
			base := []Talk{
				{ID: 1, Title: "Palabras de introducción", Duration: 60, Closing: 30},
				{ID: 2, Title: tc.bibleStudy, Duration: 1800, Closing: 120},
				{ID: 3, Title: "Palabras de conclusión", Duration: 180, Closing: 60},
			}
			got := scheduleForMeetingType("midweek", base, true, tc.language)
			if len(got) != len(base) {
				t.Fatalf("expected %d parts, got %d: %+v", len(base), len(got), got)
			}
			if got[1].Title != tc.wantTitle {
				t.Fatalf("expected Bible Study replaced by %q, got %q", tc.wantTitle, got[1].Title)
			}
			if got[1].Duration != 1800 {
				t.Fatalf("expected a 30-minute CO service talk, got %+v", got[1])
			}
			if base[1].Title != tc.bibleStudy {
				t.Fatalf("base schedule was mutated: %+v", base)
			}
		})
	}
}

func TestCircuitOverseerEndpointPersistsAndSwaps(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	srv, err := newServer(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}
	pinMidweek(t, srv) // deterministic weekday

	req := httptest.NewRequest(http.MethodPost, "/api/control/circuit-overseer", strings.NewReader(`{"on":true}`))
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected OK, got %d: %s", res.Code, res.Body.String())
	}

	var state State
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if !state.CircuitOverseer {
		t.Fatal("expected circuitOverseer true in state")
	}
	for _, p := range state.Schedule {
		if strings.Contains(strings.ToLower(p.Title), "bible study") {
			t.Fatalf("midweek CO schedule should not contain Bible Study: %+v", state.Schedule)
		}
	}

	// Persisted to disk as a future expiry (still active).
	reloaded, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !circuitOverseerActive(reloaded.CircuitOverseerExpiresAt, srv.clock()) {
		t.Fatalf("expected circuitOverseer expiry persisted and active, got %v", reloaded.CircuitOverseerExpiresAt)
	}
}

func TestCircuitOverseerAutoExpiresAfterThreeHours(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	// Pin to a fixed weekday, turn CO on.
	base := time.Date(2026, 7, 8, 19, 0, 0, 0, time.UTC) // Wednesday 19:00
	srv.clock = func() time.Time { return base }
	srv.mu.Lock()
	srv.syncActiveScheduleLocked(base, srv.meetingInProgressLocked(base))
	srv.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/control/circuit-overseer", strings.NewReader(`{"on":true}`))
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("toggle on returned %d: %s", res.Code, res.Body.String())
	}

	// Still active 2h later.
	srv.clock = func() time.Time { return base.Add(2 * time.Hour) }
	if !srv.snapshot().CircuitOverseer {
		t.Fatal("expected CO still active after 2h")
	}

	// Auto-deactivated 3h+ later, and the schedule reverts.
	srv.clock = func() time.Time { return base.Add(3*time.Hour + time.Minute) }
	st := srv.snapshot()
	if st.CircuitOverseer {
		t.Fatal("expected CO to auto-deactivate after 3 hours")
	}
	for _, p := range st.Schedule {
		if p.Title == "Circuit Overseer Service Talk" {
			t.Fatalf("expected schedule to revert after expiry, got %+v", st.Schedule)
		}
	}
}

func TestCircuitOverseerRejectedWhileRunning(t *testing.T) {
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}
	pinMidweek(t, srv)

	do := func(path, body string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		return res.Code
	}

	if code := do("/api/control/start", ""); code != http.StatusOK {
		t.Fatalf("start returned %d", code)
	}
	if code := do("/api/control/circuit-overseer", `{"on":true}`); code != http.StatusConflict {
		t.Fatalf("expected 409 while running, got %d", code)
	}
}
