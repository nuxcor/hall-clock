package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// The app runs as an unprivileged user and cannot replace its own binary or
	// restart its own unit. It asks for an update by creating the trigger file;
	// a systemd .path unit watching that file starts the root-owned updater,
	// which reports back through the status file. Both live in the service's
	// StateDirectory (/var/lib/hall-clock): unlike the RuntimeDirectory, it
	// survives the restart that an update performs, so the result is still
	// there to show once the app comes back up.
	defaultUpdateTriggerPath = "/var/lib/hall-clock/update-requested"
	defaultUpdateStatusPath  = "/var/lib/hall-clock/update-status.json"

	defaultUpdateRepo = "nuxcor/hall-clock"

	// updateCheckTTL caps how often we ask GitHub for the latest tag. The setup
	// page asks on every load, and the API is rate-limited per IP.
	updateCheckTTL = 15 * time.Minute

	// updateForceMinInterval is how soon "check again" may go back to GitHub.
	// The link needs no token, and unthrottled it let anything on the network
	// spend the hall's 60 requests an hour — the updater's own check included.
	updateForceMinInterval = 30 * time.Second
)

// UpdateStatus mirrors the JSON that hall-clock-update.sh writes. Phases:
// checking, downloading, restarting, updated, up-to-date, available, failed,
// deferred.
type UpdateStatus struct {
	Phase   string `json:"phase"`
	Message string `json:"message,omitempty"`
	Version string `json:"version,omitempty"`
	Latest  string `json:"latest,omitempty"`
	At      string `json:"at,omitempty"`
}

// UpdateInfo is what the setup page renders.
type UpdateInfo struct {
	Version         string `json:"version"`
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	CheckError      string `json:"checkError,omitempty"`
	// Supported is false on a dev machine or any install without the updater
	// wired up, so the page can hide the button instead of offering a dead one.
	Supported bool          `json:"supported"`
	CanUpdate bool          `json:"canUpdate"`
	Pending   bool          `json:"pending"`
	Status    *UpdateStatus `json:"status,omitempty"`
}

// latestReleaseTagFunc is swapped in tests.
var latestReleaseTagFunc = latestReleaseTag

func latestReleaseTag(ctx context.Context, repo string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "hall-clock-local-appliance/0.1")

	client := http.Client{Timeout: 12 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return "", errors.New("could not reach GitHub")
	}
	defer res.Body.Close()

	// GitHub hides private and missing repos behind the same 404 it returns for a
	// repo with no releases, so this cannot be narrowed further from here.
	if res.StatusCode == http.StatusNotFound {
		return "", errors.New("no releases published yet (or the repo is private)")
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", fmt.Errorf("GitHub returned HTTP %d", res.StatusCode)
	}

	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		return "", errors.New("could not read the latest release")
	}
	if payload.TagName == "" {
		return "", errors.New("latest release has no tag")
	}
	return payload.TagName, nil
}

// updateChecker caches the latest published tag so repeated setup-page loads do
// not each spend a GitHub API call.
type updateChecker struct {
	mu        sync.Mutex
	repo      string
	tag       string
	err       error
	checkedAt time.Time
}

func (u *updateChecker) latest(ctx context.Context, now time.Time, force bool) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fresh := updateCheckTTL
	if force {
		fresh = updateForceMinInterval
	}
	if !u.checkedAt.IsZero() && now.Sub(u.checkedAt) < fresh {
		return u.tag, u.err
	}
	tag, err := latestReleaseTagFunc(ctx, u.repo)
	u.err = err
	u.checkedAt = now
	// Keep the last tag we did see. A hall on flaky Wi-Fi would otherwise forget
	// that an update exists the moment one check fails, and the page would say
	// "up to date" while a release sits there waiting.
	if err == nil {
		u.tag = tag
	}
	return u.tag, u.err
}

// updateAvailable reports whether latest is worth installing over current.
//
// current is whatever -ldflags stamped in: a release tag on a CI build, but
// "dev" under `go run`, and a `git describe` string like "v1.2.0-3-gabc1234" for
// a binary built from a working tree. A describe string that starts with the
// latest tag is a build made *after* that release, so offering it is a downgrade
// dressed up as an update.
//
// Versions are compared by order, not just for difference: when GitHub's latest
// is older than what is running — a release deleted or rolled back — offering
// it would be a downgrade too.
func updateAvailable(current, latest string) bool {
	if latest == "" || current == "dev" || current == "unknown" {
		return false
	}
	if current == latest || strings.HasPrefix(current, latest+"-") {
		return false
	}
	currentVersion, currentOK := parseReleaseVersion(current)
	latestVersion, latestOK := parseReleaseVersion(latest)
	if currentOK && latestOK {
		return compareReleaseVersions(latestVersion, currentVersion) > 0
	}
	return true
}

// parseReleaseVersion reads the vMAJOR.MINOR.PATCH at the front of a tag or a
// git describe string ("v1.2.0-3-gabc1234", "v1.2.0-dirty").
func parseReleaseVersion(tag string) ([3]int, bool) {
	var out [3]int
	core, _, _ := strings.Cut(strings.TrimPrefix(tag, "v"), "-")
	fields := strings.Split(core, ".")
	if len(fields) != 3 {
		return out, false
	}
	for i, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func compareReleaseVersions(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			return cmp.Compare(a[i], b[i])
		}
	}
	return 0
}

func readUpdateStatus(path string) *UpdateStatus {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var status UpdateStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil
	}
	return &status
}

// updateSupported reports whether the updater is wired up on this box. The
// trigger file's directory is created by systemd's StateDirectory, so its
// absence means this is a dev machine or a manual install.
func updateSupported(triggerPath string) bool {
	info, err := os.Stat(filepath.Dir(triggerPath))
	return err == nil && info.IsDir()
}

// updateAllowedLocked reports whether an update may restart the app now. The
// restart blanks the TV and resets the timer, so it waits for a gap between
// meetings: not while one is in progress — idle between parts included — and
// not during the countdown to the next.
func (s *server) updateAllowedLocked(now time.Time) bool {
	s.recalculateLocked(now)
	return !s.state.MeetingInProgress && !s.state.PrestartActive
}

func (s *server) handleUpdateInfo(w http.ResponseWriter, r *http.Request) {
	now := s.clock()
	s.mu.Lock()
	allowed := s.updateAllowedLocked(now)
	s.mu.Unlock()

	info := UpdateInfo{
		Version:   version,
		Supported: updateSupported(s.updateTriggerPath),
		CanUpdate: allowed,
		Status:    readUpdateStatus(s.updateStatusPath),
	}
	if _, err := os.Stat(s.updateTriggerPath); err == nil {
		info.Pending = true
	}

	// ?refresh=1 is the "check again" link; a normal page load uses the cache.
	force := r.URL.Query().Get("refresh") == "1"
	// Detached from the request: the answer is cached for everyone, so a phone
	// that navigates away mid-check must not leave "could not reach GitHub"
	// on the setup page for the next fifteen minutes. The client's own timeout
	// still bounds the call.
	latest, err := s.updates.latest(context.WithoutCancel(r.Context()), now, force)
	if err != nil {
		info.CheckError = err.Error()
	}
	// A failed check still offers the last release we know about, so a flaky
	// network does not hide an available update.
	if latest != "" {
		info.Latest = latest
		info.UpdateAvailable = updateAvailable(version, latest)
	}

	writeJSON(w, info)
}

func (s *server) handleUpdateStart(w http.ResponseWriter, r *http.Request) {
	if !updateSupported(s.updateTriggerPath) {
		http.Error(w, "self-update is not configured on this device", http.StatusConflict)
		return
	}

	s.mu.Lock()
	allowed := s.updateAllowedLocked(s.clock())
	s.mu.Unlock()
	if !allowed {
		http.Error(w, "updates can only be installed between meetings", http.StatusConflict)
		return
	}

	// Write-then-rename so the watching .path unit never sees a partial file.
	tmp := s.updateTriggerPath + ".tmp"
	os.Remove(tmp)
	if err := os.WriteFile(tmp, []byte("update\n"), 0o644); err != nil {
		os.Remove(tmp)
		log.Printf("update: could not stage trigger: %v", err)
		http.Error(w, "could not request an update", http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmp, s.updateTriggerPath); err != nil {
		os.Remove(tmp)
		log.Printf("update: could not request update: %v", err)
		http.Error(w, "could not request an update", http.StatusInternalServerError)
		return
	}

	// The updater will restart this process, so answer before that happens.
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, UpdateInfo{
		Version:   version,
		Supported: true,
		CanUpdate: allowed,
		Pending:   true,
		Status:    readUpdateStatus(s.updateStatusPath),
	})
}
