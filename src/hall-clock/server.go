package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
)

type server struct {
	mu          sync.Mutex
	configPath  string
	config      Config
	state       State
	talks       []Talk
	startedAt   time.Time
	remainingAt int
	subscribers map[chan State]struct{}
	// saveMu serializes config persistence, and saveSeq/lastSavedSeq stop an
	// older snapshot from renaming over a newer one when two writers race —
	// e.g. the hourly import tick against an operator's language switch.
	saveMu       sync.Mutex
	saveSeq      uint64
	lastSavedSeq uint64
	// retiredOverruns is one record per part the operator has finished and moved
	// on from, in order. The meeting's total is their sum plus the live part.
	// Keeping the parts rather than a running sum means a future undo can drop
	// the last record, where a bare total could only be subtracted from.
	//
	// Not persisted: it describes one meeting, and a reboot mid meeting is rare
	// enough that a wrong total is worse than a reset one.
	retiredOverruns []partOverrun
	// meetingActiveAt is the last moment a part was on the clock (running or
	// paused) in the meeting now under way, and zero when none is. The overtime
	// records and every idle-only reconciliation hang off it through
	// meetingInProgressLocked.
	meetingActiveAt time.Time
	// pauseCarry is the part of a second the current part had used when it was
	// paused. Elapsed time is counted in whole seconds, so dropping it handed the
	// speaker up to a second back on every pause.
	pauseCarry time.Duration
	// lastPrefetchSweep throttles retries of the per-language pre-load. A hall
	// whose second language has no workbook published yet would otherwise be
	// refetched every loop iteration — four times an hour, forever. A warm
	// cache costs nothing here: the sweep returns before any network call.
	lastPrefetchSweep time.Time
	// pairingUntil is when the current no-PIN window shuts: either the
	// first-boot grace period or an "add a phone" window a paired controller
	// opened. Zero means a PIN is required. Not persisted — a reboot with a PIN
	// set should never come up open.
	pairingUntil time.Time
	// pairingOneShot marks a window that a single phone consumes: the "add a
	// phone" window a paired controller opens deliberately. The first-boot
	// window is NOT one-shot — it is already bounded by time and by setting a
	// PIN, and consuming it on the first claim stranded everyone else. That
	// claim is silent (ensurePaired pairs with no UI), so simply opening the
	// controller on one origin used to lock the setup page out on another.
	pairingOneShot bool
	// pinFailures counts consecutive wrong PINs and pinLockedUntil is when
	// guessing may resume, so a long-lived PIN cannot be ground down over an
	// evening. Both reset on a success.
	pinFailures    int
	pinLockedUntil time.Time
	// clock is the time source for all scheduling decisions (meeting-type
	// switching, stale-part purging, timer elapsed). Defaults to time.Now;
	// tests override it to pin a specific day so behaviour is deterministic.
	clock func() time.Time
	// webAssets, when set, serves the HTML/CSS/JS live from disk instead of
	// the copy embedded in the binary. Used for local development so edits
	// show up on refresh without a rebuild; nil in production.
	webAssets fs.FS
	// Self-update plumbing: the app writes updateTriggerPath to ask the
	// root-owned updater to run, and reads back updateStatusPath. See update.go.
	updateTriggerPath string
	updateStatusPath  string
	updates           *updateChecker
}

const currentConfigVersion = 1

func init() {
	// The OS mime table usually has no entry for .webmanifest, and FileServer
	// would sniff it as text/plain; browsers want application/manifest+json
	// for the home-screen install manifests under /assets.
	if err := mime.AddExtensionType(".webmanifest", "application/manifest+json"); err != nil {
		panic(err)
	}
}

// newServer builds a server on the wall clock.
func newServer(configPath string) (*server, error) {
	return newServerWithClock(configPath, time.Now)
}

// newServerWithClock builds a server on a caller-supplied time source. The
// initial state is derived at construction — meeting type, active schedule, CO
// mode — so a clock set afterwards is already too late to influence it. Tests
// pin a weekday here: meetingTypeForTime returns "weekend" on Saturday and
// Sunday, which used to make four tests fail every weekend, CI included.
func newServerWithClock(configPath string, clock func() time.Time) (*server, error) {
	config, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}

	if len(config.Schedule) == 0 {
		config.Schedule = defaultSchedule()
	}
	normalizeSchedule(config.Schedule)
	baselineRejected := isWeekendSchedule(config.Schedule)
	if baselineRejected {
		config.Schedule = defaultSchedule()
	}
	if len(config.ScheduleOverride) > 0 {
		normalizeSchedule(config.ScheduleOverride)
		// The override and its baseline are a pair: an edit is only meaningful
		// against the program it was derived from. If we had to throw the baseline
		// away, the edit goes with it rather than shadowing a default the operator
		// never chose.
		if baselineRejected || isWeekendSchedule(config.ScheduleOverride) {
			config.ScheduleOverride = nil
			config.ScheduleOverrideExpiresAt = time.Time{}
		}
	}
	if strings.TrimSpace(config.DeviceName) == "" {
		config.DeviceName = "Hall Clock"
	}
	config.MeetingType = normalizeMeetingType(config.MeetingType)
	config.MeetingStartTime = normalizeStartTime(config.MeetingStartTime)
	config.MeetingStarts = normalizeMeetingStarts(config.MeetingStarts, config.MeetingStartTime)
	if config.PrestartSeconds == 0 {
		config.PrestartSeconds = 300
	}
	config.PrestartSeconds = clamp(config.PrestartSeconds, 60, 1800)
	if config.MidweekLanguage == "" {
		config.MidweekLanguage = wolLanguage(config.MidweekURL)
	}
	if config.MidweekLanguageSources == nil {
		config.MidweekLanguageSources = map[string]string{}
	}
	healMidweekLanguageBookkeeping(&config, clock())
	// File the active URL as its language's source only when the two agree.
	// They can legitimately disagree: switching the weekend language changes
	// MidweekLanguage but deliberately leaves the midweek URL alone, and
	// filing that URL under the weekend's language handed the pre-load sweep
	// a source that imports the wrong language's items — a Twi workbook
	// cached and served as "English".
	if config.MidweekLanguage != "" && wolLanguage(config.MidweekURL) == config.MidweekLanguage {
		config.MidweekLanguageSources[config.MidweekLanguage] = config.MidweekURL
	}
	if config.MidweekLanguage != "" && config.MidweekImportedWeek != "" && len(config.Schedule) > 0 {
		if config.MidweekLanguageSchedules == nil {
			config.MidweekLanguageSchedules = map[string]MidweekLanguageSchedule{}
		}
		cached := append([]Talk(nil), config.Schedule...)
		normalizeSchedule(cached)
		config.MidweekLanguageSchedules[config.MidweekLanguage] = MidweekLanguageSchedule{
			ImportedWeek: config.MidweekImportedWeek,
			URL:          config.MidweekURL,
			Schedule:     cached,
		}
	}
	if config.Version < currentConfigVersion {
		config.AutoImportMidweek = true
		config.Version = currentConfigVersion
	}
	if config.ControlToken == "" {
		config.ControlToken, err = newToken()
		if err != nil {
			return nil, err
		}
	}
	if err := saveConfig(configPath, config); err != nil {
		log.Printf("config: could not persist startup config to %s: %v", configPath, err)
	}

	now := clock()
	coActive := circuitOverseerActive(config.CircuitOverseerExpiresAt, now)
	activeMeetingType := meetingTypeForTime(now)
	activeSchedule := scheduleForMeetingType(activeMeetingType, effectiveMidweekSchedule(config, false, now), coActive, config.MidweekLanguage)
	first := activeSchedule[0]
	return &server{
		configPath: configPath,
		config:     config,
		state: State{
			Status:                   StatusIdle,
			DeviceName:               config.DeviceName,
			MeetingType:              activeMeetingType,
			MeetingStartTime:         config.MeetingStartTime,
			MeetingStarts:            config.MeetingStarts,
			PrestartLabel:            "",
			PrestartSeconds:          config.PrestartSeconds,
			CurrentTalkID:            first.ID,
			CurrentTalkTitle:         first.Title,
			DurationSeconds:          first.Duration,
			RemainingSeconds:         first.Duration,
			ClosingSeconds:           first.Closing,
			CircuitOverseer:          coActive,
			CircuitOverseerExpiresAt: circuitOverseerExpiryPtr(config.CircuitOverseerExpiresAt, now),
			MidweekLanguage:          config.MidweekLanguage,
			Schedule:                 activeSchedule,
			Now:                      now,
		},
		// A copy, not the baseline itself: activeSchedule can be
		// config.Schedule, and the runtime talks list must never share a
		// backing array with the saved programme.
		talks:       append([]Talk(nil), activeSchedule...),
		remainingAt: first.Duration,
		// With no PIN set there is no way in yet, so open the bootstrap window
		// for whoever is installing the appliance. It closes on its own, and
		// setting a PIN closes it early.
		pairingUntil:      firstBootPairingWindow(config, now),
		subscribers:       map[chan State]struct{}{},
		clock:             clock,
		updateTriggerPath: defaultUpdateTriggerPath,
		updateStatusPath:  defaultUpdateStatusPath,
		updates:           &updateChecker{repo: defaultUpdateRepo},
	}, nil
}

// firstBootPairingWindow returns how long pairing stays open without a PIN on
// this start. A configured PIN means the hall is already secured and the
// window never opens; an unset one means nobody could pair at all otherwise.
func firstBootPairingWindow(config Config, now time.Time) time.Time {
	if config.ControlPIN != "" {
		return time.Time{}
	}
	log.Printf("no pairing PIN set: any phone on the network can pair for the next %s — set a PIN in /setup", pairingGrace)
	return now.Add(pairingGrace)
}

func (s *server) routes(publicURL string) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	static := s.webAssets
	if static == nil {
		sub, err := fs.Sub(webFS, "web")
		if err != nil {
			return nil, err
		}
		static = sub
	}

	assets := http.FileServer(http.FS(static))
	if s.webAssets != nil {
		// Live-from-disk mode: stop the browser caching so edits show up on
		// refresh without a rebuild.
		assets = noCache(assets)
	}
	mux.Handle("GET /assets/", assets)
	// Root serves the phone controller so the printed QR can be a clean
	// http://hallclock.local (no /control). The TV kiosk opens /display
	// explicitly, and /control stays as an alias for existing links/QRs.
	mux.HandleFunc("GET /{$}", s.servePage(static, "control.html"))
	mux.HandleFunc("GET /display", s.servePage(static, "display.html"))
	mux.HandleFunc("GET /pair", s.servePage(static, "pair.html"))
	mux.HandleFunc("GET /control", s.servePage(static, "control.html"))
	mux.HandleFunc("GET /setup", s.servePage(static, "setup.html"))
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/update", s.handleUpdateInfo)
	mux.HandleFunc("POST /api/update", s.protect(s.handleUpdateStart))
	mux.HandleFunc("GET /api/pairing", s.handlePairing(publicURL))
	mux.HandleFunc("POST /api/pairing/claim", s.handleClaimPairing(publicURL))
	mux.HandleFunc("POST /api/pairing/enable", s.protect(s.handleEnablePairing))
	mux.HandleFunc("GET /api/pairing/verify", s.protect(s.handleVerifyToken))
	mux.HandleFunc("GET /api/pairing/pin", s.protect(s.handleShowPIN))
	mux.HandleFunc("POST /api/pairing/pin", s.protect(s.handleSetPIN))
	mux.HandleFunc("POST /api/control/start", s.protect(s.handleStart))
	mux.HandleFunc("POST /api/control/pause", s.protect(s.handlePause))
	mux.HandleFunc("POST /api/control/reset", s.protect(s.handleReset))
	mux.HandleFunc("POST /api/control/end", s.protect(s.handleEndMeeting))
	mux.HandleFunc("POST /api/control/next", s.protect(s.handleNext))
	mux.HandleFunc("POST /api/control/previous", s.protect(s.handlePrevious))
	mux.HandleFunc("POST /api/control/adjust", s.protect(s.handleAdjust))
	mux.HandleFunc("POST /api/control/time", s.protect(s.handleSetTime))
	mux.HandleFunc("POST /api/control/select", s.protect(s.handleSelect))
	mux.HandleFunc("POST /api/control/remove-part", s.protect(s.handleRemovePart))
	mux.HandleFunc("POST /api/control/adhoc-part", s.protect(s.handleAdhocPart))
	mux.HandleFunc("POST /api/control/circuit-overseer", s.protect(s.handleCircuitOverseer))
	mux.HandleFunc("POST /api/control/midweek-language", s.protect(s.handleMidweekLanguage))
	mux.HandleFunc("POST /api/config", s.protect(s.handleSaveConfig))
	mux.HandleFunc("POST /api/import/midweek", s.protect(s.handleImportMidweek))
	mux.HandleFunc("POST /api/import/midweek-text", s.protect(s.handleImportMidweekText))
	mux.HandleFunc("POST /api/template/weekend", s.protect(s.handleWeekendTemplate))
	mux.HandleFunc("POST /api/template/midweek", s.protect(s.handleMidweekTemplate))
	mux.HandleFunc("GET /qr.png", s.handleQR(publicURL))

	return mux, nil
}

func (s *server) servePage(static fs.FS, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.webAssets != nil {
			w.Header().Set("Cache-Control", "no-store")
		}
		http.ServeFileFS(w, r, static, name)
	}
}

// noCache wraps a handler so responses are never cached by the browser. Used
// only in live-from-disk dev mode so edits appear on refresh.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *server) protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Wall-Clock-Token")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		s.mu.Lock()
		expected := s.config.ControlToken
		s.mu.Unlock()
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
			http.Error(w, "missing or invalid control token", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// openPairingLocked opens a window during which a phone may pair without the
// PIN. Reusing a live window rather than extending it keeps a burst of clicks
// from holding the door open indefinitely.
func (s *server) openPairingLocked(now time.Time, window time.Duration, oneShot bool) {
	if now.Before(s.pairingUntil) {
		return
	}
	s.pairingUntil = now.Add(window)
	s.pairingOneShot = oneShot
}

// pairingOpenLocked reports whether pairing currently needs no PIN.
func (s *server) pairingOpenLocked(now time.Time) bool {
	return now.Before(s.pairingUntil)
}

func (s *server) handlePairing(publicURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := s.clock()
		s.mu.Lock()
		configuredURL := s.config.AdvertisedBaseURL
		open := s.pairingOpenLocked(now)
		pinSet := s.config.ControlPIN != ""
		pinLength := len([]rune(s.config.ControlPIN))
		expiresAt := s.pairingUntil
		deviceName := s.config.DeviceName
		s.mu.Unlock()

		// The advertised URL carries no token, so a QR printed on a noticeboard
		// stays valid for years and photographing one grants nothing on its
		// own — it opens the controller, which then has to pair.
		body := map[string]any{
			"controlUrl":    advertisedControlURL(publicURL, configuredURL, r),
			"pairingOpen":   open,
			"pinRequired":   !open,
			"pinConfigured": pinSet,
			// The PIN prompt names the hall it is pairing with: two halls on one
			// network look identical otherwise, and a phone that opened the
			// wrong one asks for the wrong PIN with nothing to say so. Already
			// public in every state broadcast.
			"deviceName": deviceName,
		}
		// The PIN's length lets the pairing dialog claim on the final digit
		// instead of asking for a redundant button tap. Within this appliance's
		// trust model that reveals nothing worth guarding: this endpoint is
		// deliberately unauthenticated (see README), and the guessing lockout —
		// not secrecy about length — is what throttles a wrong PIN.
		if pinSet {
			body["pinLength"] = pinLength
		}
		if open {
			body["pairingExpiresAt"] = expiresAt
		}
		writeJSON(w, body)
	}
}

func (s *server) handleClaimPairing(publicURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PIN string `json:"pin"`
		}
		if err := decodeBody(w, r, 4096, &body); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		now := s.clock()
		s.mu.Lock()
		open := s.pairingOpenLocked(now)
		// An "add a phone" window pairs exactly one phone; leaving it open would
		// let anyone who noticed it join for the rest of the five minutes. Spend
		// it here, in the critical section that saw it open: closing it after
		// the unlock let two claims arriving together both walk through. The
		// first-boot window stays open for its full term, so pairing a phone
		// does not strand the laptop somebody is about to set the PIN from.
		if open && s.pairingOneShot {
			s.pairingUntil = time.Time{}
			s.pairingOneShot = false
		}
		locked := now.Before(s.pinLockedUntil)
		expected := s.config.ControlPIN
		if !open && !locked && expected != "" {
			// Spend the attempt here, in the same critical section that read the
			// lockout. Counting failures anywhere after this unlock lets a burst
			// of concurrent guesses all observe an untripped counter and sail
			// past the cap together — measured at 21 of 40 getting through.
			s.pinFailures++
			if s.pinFailures >= maxPINFailures {
				s.pinFailures = 0
				s.pinLockedUntil = now.Add(pinLockout)
			}
		}
		s.mu.Unlock()

		if !open {
			if locked {
				http.Error(w, "too many wrong PINs; wait a few minutes and try again", http.StatusTooManyRequests)
				return
			}
			if expected == "" {
				// No PIN set and no window open: the grace period lapsed before
				// anybody used it. Say so plainly rather than pretend a PIN
				// exists, or the operator will hunt for one nobody ever chose.
				http.Error(w, "pairing is closed; restart the clock to reopen the setup window", http.StatusForbidden)
				return
			}
			// The attempt is already spent, so parallel guessing buys nothing.
			if !pinMatches(expected, strings.TrimSpace(body.PIN)) {
				http.Error(w, "wrong PIN", http.StatusUnauthorized)
				return
			}
		}

		s.mu.Lock()
		s.pinFailures = 0
		s.pinLockedUntil = time.Time{}
		token := s.config.ControlToken
		configuredURL := s.config.AdvertisedBaseURL
		state := s.snapshotLocked()
		s.mu.Unlock()

		s.broadcast(state)
		target := advertisedControlURL(publicURL, configuredURL, r)
		writeJSON(w, map[string]string{
			"token":      token,
			"controlUrl": withToken(target, token),
		})
	}
}

// handleSetPIN sets or changes the pairing PIN. It is token-protected, so the
// caller is an already-paired controller: whoever holds the clock decides who
// else may hold it. Existing phones keep working — the control token does not
// change — so a PIN change is not a way to evict a lost phone.
func (s *server) handleSetPIN(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PIN string `json:"pin"`
	}
	if err := decodeBody(w, r, 4096, &body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	pin, err := normalizePIN(body.PIN)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := s.clock()
	s.mu.Lock()
	s.config.ControlPIN = pin
	s.pinFailures = 0
	s.pinLockedUntil = time.Time{}
	// Setting a PIN closes the bootstrap window: the appliance is secured now,
	// and leaving the door open for the rest of the grace period would undo
	// exactly what the operator just did.
	if s.pairingUntil.After(now) {
		s.pairingUntil = time.Time{}
	}
	state := s.snapshotLocked()
	s.mu.Unlock()

	if err := s.persistConfig(); err != nil {
		http.Error(w, "could not save the PIN", http.StatusInternalServerError)
		return
	}
	s.broadcast(state)
	writeJSON(w, map[string]any{"pinConfigured": true})
}

// handleVerifyToken exists so a browser can ask whether the token it is holding
// still works. A config reset mints a fresh ControlToken, which leaves every
// paired phone holding a dead one — and a client that assumes any stored token
// is good then fails every write with a silent 401 forever.
func (s *server) handleVerifyToken(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
}

// handleShowPIN returns the PIN in force. Token-protected, so the caller is an
// already-paired controller — somebody who could change the PIN anyway, and who
// on this appliance already holds full control of the clock. Being able to read
// it back is what stops a forgotten PIN turning into a reset every few months.
func (s *server) handleShowPIN(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	pin := s.config.ControlPIN
	s.mu.Unlock()

	writeJSON(w, map[string]any{"pin": pin, "pinConfigured": pin != ""})
}

// handleEnablePairing lets a paired controller open a short window so a second
// phone can join without being told the PIN.
func (s *server) handleEnablePairing(w http.ResponseWriter, r *http.Request) {
	now := s.clock()
	s.mu.Lock()
	s.openPairingLocked(now, pairingWindow, true)
	expiresAt := s.pairingUntil
	state := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(state)
	writeJSON(w, map[string]any{"pairingOpen": true, "pairingExpiresAt": expiresAt})
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan State, 8)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	initial := s.snapshotLocked()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subscribers, ch)
		s.mu.Unlock()
		close(ch)
	}()

	writeEvent(w, initial)
	flusher.Flush()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case state := <-ch:
			writeEvent(w, state)
			flusher.Flush()
		case <-ticker.C:
			state := s.snapshot()
			writeEvent(w, state)
			flusher.Flush()
		}
	}
}

func (s *server) handleQR(publicURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		configuredURL := s.config.AdvertisedBaseURL
		s.mu.Unlock()

		// No token in the QR. A printed code sits on a noticeboard for years,
		// and a photograph of it must not be a permanent key to the clock: it
		// points at the controller, which pairs against the display like any
		// other phone.
		target := advertisedControlURL(publicURL, configuredURL, r)

		png, err := qrcode.Encode(target, qrcode.Medium, 512)
		if err != nil {
			http.Error(w, "could not generate QR code", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = bytes.NewReader(png).WriteTo(w)
	}
}

func (s *server) snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *server) snapshotLocked() State {
	s.recalculateLocked(s.clock())
	out := s.state
	out.Schedule = append([]Talk(nil), s.talks...)
	out.MeetingStarts = append([]MeetingStart(nil), s.config.MeetingStarts...)
	out.MidweekLanguage = s.config.MidweekLanguage
	out.ScheduleOverrideExpiresAt = sessionWindowExpiryPtr(s.config.ScheduleOverrideExpiresAt, s.state.Now)
	out.MeetingOvertimeSeconds = s.meetingOvertimeSecondsLocked(s.state.Now)
	// Whether a no-PIN window is open, so the setup page can show it. Nothing
	// secret goes in here: this snapshot is broadcast to every subscriber.
	out.PairingActive = s.pairingOpenLocked(s.state.Now)
	out.PairingExpiresAt = nil
	if out.PairingActive {
		e := s.pairingUntil
		out.PairingExpiresAt = &e
	}
	return out
}

func (s *server) broadcast(state State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- state:
		default:
		}
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeEvent(w http.ResponseWriter, state State) {
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: state\ndata: %s\n\n", data)
}
