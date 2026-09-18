package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// updateTestServer wires the update paths into a temp dir so the handlers do not
// touch /var/lib, and pins the "latest release" lookup.
func updateTestServer(t *testing.T, latest string, lookupErr error) (*server, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	srv, err := newServer(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srv.updateTriggerPath = filepath.Join(stateDir, "update-requested")
	srv.updateStatusPath = filepath.Join(stateDir, "update-status.json")
	// Updates wait out the pre-meeting countdown, so on the wall clock these
	// tests would fail whenever CI ran in the five minutes before a meeting.
	noon := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) // Thursday
	srv.clock = func() time.Time { return noon }

	original := latestReleaseTagFunc
	latestReleaseTagFunc = func(context.Context, string) (string, error) { return latest, lookupErr }
	t.Cleanup(func() { latestReleaseTagFunc = original })

	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}
	return srv, mux, srv.updateTriggerPath
}

func getUpdateInfo(t *testing.T, mux http.Handler) UpdateInfo {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/update", nil)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /api/update: %d", res.Code)
	}
	var info UpdateInfo
	if err := json.Unmarshal(res.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	return info
}

func TestUpdateInfoReportsAvailableRelease(t *testing.T) {
	version = "v1.0.0"
	t.Cleanup(func() { version = "dev" })

	_, mux, _ := updateTestServer(t, "v1.1.0", nil)
	info := getUpdateInfo(t, mux)

	if !info.Supported {
		t.Fatal("expected the updater to be reported as supported")
	}
	if !info.UpdateAvailable || info.Latest != "v1.1.0" {
		t.Fatalf("expected v1.1.0 to be offered, got %+v", info)
	}
	if !info.CanUpdate {
		t.Fatal("expected an idle timer to allow updating")
	}
}

func TestUpdateAvailable(t *testing.T) {
	cases := []struct {
		current string
		latest  string
		want    bool
	}{
		{"v1.0.0", "v1.1.0", true},
		{"v1.0.0", "v1.0.0", false},
		// `go run`: no tag was stamped in, so there is nothing to compare.
		{"dev", "v1.1.0", false},
		{"unknown", "v1.1.0", false},
		{"v1.0.0", "", false},
		// git describe of a build made after the latest release: installing it
		// would be a downgrade, not an update.
		{"v1.1.0-3-gabc1234", "v1.1.0", false},
		{"v1.1.0-dirty", "v1.1.0", false},
		// git describe from before the latest release: a real update.
		{"v1.0.0-3-gabc1234", "v1.1.0", true},
	}
	for _, tc := range cases {
		if got := updateAvailable(tc.current, tc.latest); got != tc.want {
			t.Errorf("updateAvailable(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

// A dev build has no tag to compare against, so it must never claim an update
// is available — otherwise every laptop offers to "update" to the latest tag.
func TestUpdateInfoNeverOffersUpdateForDevBuild(t *testing.T) {
	version = "dev"
	_, mux, _ := updateTestServer(t, "v1.1.0", nil)

	if info := getUpdateInfo(t, mux); info.UpdateAvailable {
		t.Fatalf("dev build should not offer an update: %+v", info)
	}
}

func TestUpdateStartWritesTriggerFile(t *testing.T) {
	version = "v1.0.0"
	t.Cleanup(func() { version = "dev" })

	srv, mux, trigger := updateTestServer(t, "v1.1.0", nil)

	req := httptest.NewRequest(http.MethodPost, "/api/update", nil)
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	// 202, not 200: the updater restarts this process, so the work outlives the
	// request.
	if res.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", res.Code, res.Body)
	}
	if _, err := os.Stat(trigger); err != nil {
		t.Fatalf("expected the trigger file the .path unit watches: %v", err)
	}
	// No leftover staging file for the .path unit to trip over.
	if _, err := os.Stat(trigger + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("staging file should have been renamed away")
	}
	if info := getUpdateInfo(t, mux); !info.Pending {
		t.Fatal("expected pending=true once the trigger exists")
	}
}

func TestUpdateStartRemovesStaleTriggerTempFile(t *testing.T) {
	version = "v1.0.0"
	t.Cleanup(func() { version = "dev" })

	srv, mux, trigger := updateTestServer(t, "v1.1.0", nil)
	if err := os.WriteFile(trigger+".tmp", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/update", nil)
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", res.Code, res.Body)
	}
	data, err := os.ReadFile(trigger)
	if err != nil {
		t.Fatalf("expected trigger file: %v", err)
	}
	if string(data) != "update\n" {
		t.Fatalf("expected fresh trigger contents, got %q", data)
	}
	if _, err := os.Stat(trigger + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("stale staging file should have been removed")
	}
}

func TestUpdateStartRequiresToken(t *testing.T) {
	_, mux, trigger := updateTestServer(t, "v1.1.0", nil)

	req := httptest.NewRequest(http.MethodPost, "/api/update", nil)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", res.Code)
	}
	if _, err := os.Stat(trigger); !os.IsNotExist(err) {
		t.Fatal("an unauthenticated request must not request an update")
	}
}

// Restarting rebuilds state with the timer reset to idle, which would blank a
// running countdown on the projector mid-talk.
func TestUpdateStartRefusedWhileMeetingRuns(t *testing.T) {
	srv, mux, trigger := updateTestServer(t, "v1.1.0", nil)

	srv.mu.Lock()
	srv.state.Status = StatusRunning
	srv.mu.Unlock()

	if info := getUpdateInfo(t, mux); info.CanUpdate {
		t.Fatal("expected canUpdate=false while a meeting runs")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/update", nil)
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusConflict {
		t.Fatalf("expected 409 while running, got %d", res.Code)
	}
	if _, err := os.Stat(trigger); !os.IsNotExist(err) {
		t.Fatal("no update should have been requested mid-meeting")
	}
}

// A dev machine has no state directory, so the page hides the button rather
// than offering one that cannot work.
func TestUpdateUnsupportedWithoutStateDir(t *testing.T) {
	srv, mux, _ := updateTestServer(t, "v1.1.0", nil)
	srv.updateTriggerPath = filepath.Join(t.TempDir(), "missing", "update-requested")

	if info := getUpdateInfo(t, mux); info.Supported {
		t.Fatal("expected supported=false without a state directory")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/update", nil)
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("expected 409 when unsupported, got %d", res.Code)
	}
}

func TestUpdateInfoSurfacesCheckErrorAndStatusFile(t *testing.T) {
	srv, mux, _ := updateTestServer(t, "", os.ErrDeadlineExceeded)

	written := `{"phase":"failed","message":"checksum mismatch","version":"v1.0.0","latest":"v1.1.0","at":"2026-07-09T04:00:00+00:00"}`
	if err := os.WriteFile(srv.updateStatusPath, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}

	info := getUpdateInfo(t, mux)
	if info.CheckError == "" {
		t.Fatal("expected the GitHub lookup error to surface")
	}
	if info.UpdateAvailable {
		t.Fatal("a failed check must not offer an update")
	}
	if info.Status == nil || info.Status.Phase != "failed" || info.Status.Message != "checksum mismatch" {
		t.Fatalf("expected the updater's status file to be read back, got %+v", info.Status)
	}
}

// A hall on flaky Wi-Fi must not forget that a release exists the moment one
// check fails, or the page claims "up to date" while an update sits waiting.
func TestUpdateCheckKeepsLastKnownTagOnFailure(t *testing.T) {
	version = "v1.0.0"
	t.Cleanup(func() { version = "dev" })

	fail := false
	original := latestReleaseTagFunc
	latestReleaseTagFunc = func(context.Context, string) (string, error) {
		if fail {
			return "", errors.New("could not reach GitHub")
		}
		return "v1.1.0", nil
	}
	t.Cleanup(func() { latestReleaseTagFunc = original })

	dir := t.TempDir()
	srv, err := newServer(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.updateTriggerPath = filepath.Join(dir, "update-requested")
	srv.updateStatusPath = filepath.Join(dir, "update-status.json")
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	srv.clock = func() time.Time { return now }

	if info := getUpdateInfo(t, mux); !info.UpdateAvailable {
		t.Fatal("expected the first check to find v1.1.0")
	}

	// Force a re-check that fails.
	fail = true
	now = now.Add(updateForceMinInterval)
	req := httptest.NewRequest(http.MethodGet, "/api/update?refresh=1", nil)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	var info UpdateInfo
	if err := json.Unmarshal(res.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}

	if info.CheckError == "" {
		t.Fatal("expected the failure to be reported")
	}
	if !info.UpdateAvailable || info.Latest != "v1.1.0" {
		t.Fatalf("a failed check must keep offering the known release, got %+v", info)
	}
}

// The GitHub API is rate-limited per IP and the setup page polls, so repeated
// loads must not each spend a call.
func TestUpdateCheckIsCached(t *testing.T) {
	calls := 0
	original := latestReleaseTagFunc
	latestReleaseTagFunc = func(context.Context, string) (string, error) {
		calls++
		return "v1.1.0", nil
	}
	t.Cleanup(func() { latestReleaseTagFunc = original })

	dir := t.TempDir()
	srv, err := newServer(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.updateTriggerPath = filepath.Join(dir, "update-requested")
	srv.updateStatusPath = filepath.Join(dir, "update-status.json")
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	srv.clock = func() time.Time { return now }

	getUpdateInfo(t, mux)
	getUpdateInfo(t, mux)
	if calls != 1 {
		t.Fatalf("expected the second load to hit the cache, got %d calls", calls)
	}

	// "Check again" needs no token, so it cannot be a way for anything on the
	// network to spend the hall's GitHub allowance: straight after a check, it
	// is answered from the cache too.
	refresh := func() {
		req := httptest.NewRequest(http.MethodGet, "/api/update?refresh=1", nil)
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}
	refresh()
	if calls != 1 {
		t.Fatalf("expected refresh=1 right after a check to be throttled, got %d calls", calls)
	}

	// A little later it bypasses the cache.
	now = now.Add(updateForceMinInterval)
	refresh()
	if calls != 2 {
		t.Fatalf("expected refresh=1 to force a check, got %d calls", calls)
	}
}

// The check is cached for everyone, so it must not run on the asking phone's
// request context: a phone that navigates away mid-check used to leave "could
// not reach GitHub" on every setup page for fifteen minutes.
func TestUpdateCheckSurvivesAbandonedRequest(t *testing.T) {
	version = "v1.0.0"
	t.Cleanup(func() { version = "dev" })

	original := latestReleaseTagFunc
	latestReleaseTagFunc = func(ctx context.Context, _ string) (string, error) {
		if ctx.Err() != nil {
			return "", errors.New("could not reach GitHub")
		}
		return "v1.1.0", nil
	}
	t.Cleanup(func() { latestReleaseTagFunc = original })

	dir := t.TempDir()
	srv, err := newServer(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.updateTriggerPath = filepath.Join(dir, "update-requested")
	srv.updateStatusPath = filepath.Join(dir, "update-status.json")
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/update", nil).WithContext(ctx)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	info := getUpdateInfo(t, mux)
	if info.CheckError != "" || info.Latest != "v1.1.0" {
		t.Fatalf("an abandoned request poisoned the cached check: %+v", info)
	}
}

// Updating restarts the app, which blanks the TV and resets the timer. Idle
// between two parts is still the middle of a meeting.
func TestUpdateRefusedBetweenPartsOfAMeeting(t *testing.T) {
	version = "v1.0.0"
	t.Cleanup(func() { version = "dev" })
	srv, mux, trigger := updateTestServer(t, "v1.1.0", nil)
	now := time.Date(2026, 7, 9, 19, 0, 0, 0, time.UTC) // Thursday, 19:00 meeting
	srv.clock = func() time.Time { return now }

	post := func(path string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
		req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		return res.Code
	}

	post("/api/control/start")
	now = now.Add(time.Minute)
	post("/api/control/next")
	if info := getUpdateInfo(t, mux); info.CanUpdate {
		t.Fatal("offered an update between two parts of a meeting")
	}
	if code := post("/api/update"); code != http.StatusConflict {
		t.Fatalf("an update between parts was accepted: %d", code)
	}
	if _, err := os.Stat(trigger); err == nil {
		t.Fatal("the update trigger was written mid-meeting")
	}

	post("/api/control/end")
	if info := getUpdateInfo(t, mux); !info.CanUpdate {
		t.Fatal("expected updates to be allowed once the meeting ended")
	}

	// The countdown to the next meeting is on the TV too.
	now = time.Date(2026, 7, 10, 18, 57, 0, 0, time.UTC) // Friday, 3 minutes before 19:00
	if info := getUpdateInfo(t, mux); info.CanUpdate {
		t.Fatal("offered an update during the pre-meeting countdown")
	}
}

// GitHub's "latest" can be older than what is running — a release deleted or
// rolled back — and installing it would be a downgrade.
func TestUpdateAvailableNeverDowngrades(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v0.3.15", "v0.3.9", false},
		{"v0.3.9", "v0.3.15", true},
		{"v0.4.0", "v0.3.99", false},
		{"v0.3.15-2-gabc1234", "v0.3.14", false},
		{"v1.0.0", "v1.0.1", true},
		// Tags that do not parse fall back to "different means newer".
		{"v1.0.0", "nightly", true},
	}
	for _, tc := range cases {
		if got := updateAvailable(tc.current, tc.latest); got != tc.want {
			t.Errorf("updateAvailable(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}
