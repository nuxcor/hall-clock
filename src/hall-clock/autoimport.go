package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const autoImportHour = 3

var defaultMidweekLanguageSources = map[string]string{
	"en": "https://wol.jw.org/en/wol/d/r1/lp-e/202026241",
	"es": "https://wol.jw.org/es/wol/d/r4/lp-s/202026241",
	"tw": "https://wol.jw.org/tw/wol/d/r33/lp-tw/202026241",
}

// Every language the controller offers, in the order the sweep tries them.
// Each has a default source above, so all of them resolve even on a hall that
// never imported anything.
var supportedMidweekLanguages = []string{"en", "es", "tw"}

var fetchWOLPageFunc = fetchWOLPage

func importMidweekFromURL(ctx context.Context, sourceURL string) ([]Talk, error) {
	body, err := fetchWOLPageFunc(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	return parseMidweekTimings(body)
}

func fetchWOLPage(ctx context.Context, sourceURL string) (string, error) {
	if !strings.HasPrefix(sourceURL, "https://wol.jw.org/") && !strings.HasPrefix(sourceURL, "http://wol.jw.org/") {
		return "", errors.New("only wol.jw.org URLs are supported")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "hall-clock-local-appliance/0.1")

	client := http.Client{Timeout: 12 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not fetch midweek page: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", fmt.Errorf("midweek page returned HTTP %d", res.StatusCode)
	}

	return readLimitedString(res.Body, 2<<20)
}

var wolDocURLPattern = regexp.MustCompile(`^https?://wol\.jw\.org/([a-z-]+)/wol/[a-z]+/(r\d+)/(lp-[a-z0-9-]+)/`)

// weeklyMeetingsURL builds the date-addressable WOL page for the current ISO
// week, keeping the language/library segments of a previously used URL so
// non-English configurations stay in their own language.
func weeklyMeetingsURL(exampleURL string, now time.Time) string {
	lang, rsconf, lib := "en", "r1", "lp-e"
	if m := wolDocURLPattern.FindStringSubmatch(exampleURL); m != nil {
		lang, rsconf, lib = m[1], m[2], m[3]
	}
	year, week := now.ISOWeek()
	return fmt.Sprintf("https://wol.jw.org/%s/wol/meetings/%s/%s/%d/%d", lang, rsconf, lib, year, week)
}

func wolLanguage(sourceURL string) string {
	if m := wolDocURLPattern.FindStringSubmatch(sourceURL); m != nil {
		return m[1]
	}
	return ""
}

var wolDocIDPattern = regexp.MustCompile(`(\d{9})/?$`)

// wolDocID extracts the workbook document id from a WOL doc URL. Workbook
// docids are shared across languages for a given week, which makes them usable
// as a week identity check between imports.
func wolDocID(sourceURL string) string {
	if m := wolDocIDPattern.FindStringSubmatch(strings.TrimSpace(sourceURL)); m != nil {
		return m[1]
	}
	return ""
}

func normalizeMidweekLanguage(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "en", "english":
		return "en"
	case "es", "spanish":
		return "es"
	case "tw", "twi":
		return "tw"
	default:
		return ""
	}
}

func (s *server) midweekLanguageSourceLocked(language string) (string, bool) {
	language = normalizeMidweekLanguage(language)
	if language == "" {
		return "", false
	}
	if source := strings.TrimSpace(s.config.MidweekLanguageSources[language]); source != "" {
		return source, true
	}
	for _, start := range s.config.MeetingStarts {
		if source := strings.TrimSpace(start.MidweekURL); wolLanguage(source) == language {
			return source, true
		}
	}
	if wolLanguage(s.config.MidweekURL) == language {
		return s.config.MidweekURL, true
	}
	if source := defaultMidweekLanguageSources[language]; source != "" {
		return source, true
	}
	return "", false
}

func meetingStartLanguage(start MeetingStart) string {
	if language := normalizeMidweekLanguage(start.Language); language != "" {
		return language
	}
	return normalizeMidweekLanguage(wolLanguage(start.MidweekURL))
}

func meetingStartSource(start MeetingStart, fallbackURL string) autoImportSource {
	source := autoImportSource{
		exampleURL:   strings.TrimSpace(start.MidweekURL),
		importedWeek: start.MidweekImportedWeek,
		startID:      start.ID,
	}
	if source.exampleURL != "" {
		return source
	}
	language := meetingStartLanguage(start)
	if language != "" {
		source.exampleURL = defaultMidweekLanguageSources[language]
	}
	if source.exampleURL == "" {
		source.exampleURL = fallbackURL
		source.startID = 0
	}
	return source
}

// findWorkbookDocURL extracts the midweek workbook document link from a weekly
// meetings page. Workbook docids are 9 digits, which distinguishes them from
// the Watchtower study article also linked on that page.
func findWorkbookDocURL(page string) (string, bool) {
	m := regexp.MustCompile(`href="(/[a-z-]+/wol/d/r\d+/lp-[a-z0-9-]+/\d{9})"`).FindStringSubmatch(page)
	if m == nil {
		return "", false
	}
	return "https://wol.jw.org" + m[1], true
}

func isoWeekString(now time.Time) string {
	year, week := now.ISOWeek()
	return fmt.Sprintf("%d-W%02d", year, week)
}

func (s *server) autoImportLoop() {
	for {
		now := s.clock()
		s.mu.Lock()
		enabled := s.config.AutoImportMidweek
		_, due := s.nextAutoImportSourceLocked(now)
		nextCheck := s.nextAutoImportCheckAtLocked(now)
		s.mu.Unlock()

		if enabled && due {
			s.autoImportTick(context.Background(), now)
			time.Sleep(time.Hour)
			continue
		}

		// Nothing due, but an offered language may still be missing this
		// week's items — a workbook that was not published when the primary
		// import ran, or one the hall has simply never selected. Sweeping here
		// is what makes a language switch instant rather than a two-fetch wait
		// mid-meeting.
		if enabled {
			s.prefetchMidweekLanguages(context.Background(), now)
		}

		// Cap the sleep: nextCheck can be days away, and config changes (an
		// operator enabling auto-import mid-week) must not wait for Monday.
		sleep := time.Until(nextCheck)
		if sleep > 15*time.Minute {
			sleep = 15 * time.Minute
		}
		time.Sleep(sleep)
	}
}

// autoImportTick pulls the current week's midweek program. The caller controls
// the Monday 3:00 AM schedule; this method still guards against duplicate
// imports and disabled auto-import settings.

// midweekLanguageCachedLocked reports whether this week's items for a language
// are already cached and usable, i.e. whether a switch to it would be instant.
func (s *server) midweekLanguageCachedLocked(now time.Time, language string) bool {
	cached, ok := s.config.MidweekLanguageSchedules[language]
	if !ok || cached.ImportedWeek != isoWeekString(now) || len(cached.Schedule) == 0 {
		return false
	}
	return !s.cachedLanguageScheduleStaleLocked(cached)
}

// prefetchMidweekLanguages warms the per-language cache for every language the
// controller offers, so switching language mid-meeting reads from cache instead
// of making the operator wait on two WOL fetches with the meeting watching.
//
// Every offered language, not just the ones this hall configured: the switches
// that cannot be predicted from config — a visiting group, a one-off foreign
// talk — are exactly the ones that used to stall. At three languages the whole
// sweep costs at most six requests a week, and the throttle below caps the
// price of the ones that fail.
//
// It never changes the active language: that is the difference between this and
// importMidweekLanguage, which applies what it imports. Each language is stored
// and persisted on its own, so a failure on the second does not discard the
// first, and a language whose workbook is not published yet simply stays
// missing and is retried on the next tick.
func (s *server) prefetchMidweekLanguages(ctx context.Context, now time.Time) {
	s.mu.Lock()
	enabled := s.config.AutoImportMidweek
	inMeeting := s.currentMidweekMeetingActiveLocked(now)
	missing := []string{}
	for _, language := range supportedMidweekLanguages {
		if !s.midweekLanguageCachedLocked(now, language) {
			missing = append(missing, language)
		}
	}
	throttled := !s.lastPrefetchSweep.IsZero() && now.Sub(s.lastPrefetchSweep) < time.Hour
	if enabled && !inMeeting && len(missing) > 0 && !throttled {
		s.lastPrefetchSweep = now
	}
	s.mu.Unlock()

	if !enabled || inMeeting || len(missing) == 0 || throttled {
		return
	}

	for _, language := range missing {
		if err := s.prefetchMidweekLanguage(ctx, now, language); err != nil {
			log.Printf("auto-import: could not pre-load %s items: %v", languageName(language), err)
			continue
		}
		log.Printf("auto-import: pre-loaded %s items for %s", languageName(language), isoWeekString(now))
	}
}

// prefetchMidweekLanguage fetches one language's items for this week and stores
// them in the cache, leaving the active schedule untouched.
func (s *server) prefetchMidweekLanguage(ctx context.Context, now time.Time, language string) error {
	s.mu.Lock()
	sourceURL, ok := s.midweekLanguageSourceLocked(language)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no source configured for %s", languageName(language))
	}

	// The stored source points at whichever week it was saved in, so only its
	// language/library segments are trusted; the date-addressable weekly
	// meetings page decides which document is this week's.
	page, err := fetchWOLPageFunc(ctx, weeklyMeetingsURL(sourceURL, now))
	if err != nil {
		return err
	}
	docURL, ok := findWorkbookDocURL(page)
	if !ok {
		return fmt.Errorf("no workbook link on the weekly meetings page")
	}
	schedule, err := importMidweekFromURL(ctx, docURL)
	if err != nil {
		return err
	}
	if err := validateImportedLanguage(language, schedule); err != nil {
		return err
	}

	s.mu.Lock()
	if s.config.MidweekLanguageSources == nil {
		s.config.MidweekLanguageSources = map[string]string{}
	}
	s.config.MidweekLanguageSources[language] = docURL
	s.storeMidweekLanguageScheduleLocked(language, isoWeekString(now), docURL, schedule)
	s.mu.Unlock()

	// Persist per language: a partial sweep still leaves the hall better off.
	if err := s.persistConfig(); err != nil {
		return fmt.Errorf("could not save %s items: %w", languageName(language), err)
	}
	return nil
}

func (s *server) autoImportTick(ctx context.Context, now time.Time) {
	s.runPrimaryAutoImport(ctx, now)
	// Warm the other offered languages. Runs even when the primary
	// import was not due or failed: once the active language is in for the
	// week, "due" goes false forever, and a sweep gated on it would never
	// pre-load the language somebody is about to switch to.
	s.prefetchMidweekLanguages(ctx, now)
}

func (s *server) runPrimaryAutoImport(ctx context.Context, now time.Time) {
	s.mu.Lock()
	enabled := s.config.AutoImportMidweek
	source, due := s.nextAutoImportSourceLocked(now)
	s.mu.Unlock()

	currentWeek := isoWeekString(now)
	if !enabled || !due {
		return
	}

	page, err := fetchWOLPageFunc(ctx, weeklyMeetingsURL(source.exampleURL, now))
	if err != nil {
		log.Printf("auto-import: %v", err)
		return
	}
	docURL, ok := findWorkbookDocURL(page)
	if !ok {
		log.Printf("auto-import: no workbook link on weekly meetings page")
		return
	}
	schedule, err := importMidweekFromURL(ctx, docURL)
	if err != nil {
		log.Printf("auto-import: %v", err)
		return
	}
	if err := validateImportedLanguage(wolLanguage(docURL), schedule); err != nil {
		log.Printf("auto-import: %v", err)
		return
	}

	s.mu.Lock()
	_, state, ok := s.applyAutoImportedScheduleLocked(now, source, docURL, schedule)
	s.mu.Unlock()
	if !ok {
		log.Printf("auto-import: skipped applying stale import from %s", docURL)
		return
	}

	if err := s.persistConfig(); err != nil {
		log.Printf("auto-import: could not save config: %v", err)
	}
	s.broadcast(state)
	log.Printf("auto-import: applied midweek schedule for %s from %s", currentWeek, docURL)
}

func (s *server) applyAutoImportedScheduleLocked(now time.Time, source autoImportSource, docURL string, schedule []Talk) (Config, State, bool) {
	currentSource, due := s.nextAutoImportSourceLocked(now)
	if !due || !sameAutoImportSource(source, currentSource) {
		return Config{}, State{}, false
	}

	currentWeek := isoWeekString(now)
	importLanguage := wolLanguage(docURL)
	if s.config.MidweekLanguageSources == nil {
		s.config.MidweekLanguageSources = map[string]string{}
	}
	if importLanguage != "" {
		s.config.MidweekLanguageSources[importLanguage] = docURL
	}
	s.storeMidweekLanguageScheduleLocked(importLanguage, currentWeek, docURL, schedule)
	if source.startID != 0 {
		for i := range s.config.MeetingStarts {
			if s.config.MeetingStarts[i].ID == source.startID {
				s.config.MeetingStarts[i].MidweekURL = docURL
				s.config.MeetingStarts[i].MidweekImportedWeek = currentWeek
				break
			}
		}
	}
	// An operator's explicit language choice outranks the rotation: cache the
	// import for its congregation (above) but leave the active language and
	// baseline alone until the override lapses.
	if now.Before(s.config.MidweekLanguageOverrideUntil) && importLanguage != "" && importLanguage != s.config.MidweekLanguage {
		state := s.snapshotLocked()
		return s.config, state, true
	}
	s.config.MidweekURL = docURL
	s.config.MidweekLanguage = importLanguage
	s.config.MidweekImportedWeek = currentWeek
	// Nobody asked for this program, so it must not throw away one somebody
	// did: an hourly retry that succeeded at ten to seven used to wipe the edit
	// made for the seven o'clock meeting. The new baseline lands behind the
	// edit and takes over once the edit lapses.
	if s.scheduleOverrideAppliesLocked(now) {
		s.config.Schedule = schedule
	} else {
		s.setBaselineScheduleLocked(schedule)
	}
	// Never swap the program under a meeting in progress — a late-running or
	// unconfigured meeting is outside the suppression window. Idle is not the
	// test: the clock is idle between every pair of parts. The new baseline
	// lands at the first recalculation after the meeting instead.
	if !s.meetingInProgressLocked(now) {
		s.applyActiveScheduleChangeLocked(now)
	}
	state := s.snapshotLocked()
	return s.config, state, true
}

func (s *server) storeMidweekLanguageScheduleLocked(language, importedWeek, sourceURL string, schedule []Talk) {
	language = normalizeMidweekLanguage(language)
	if language == "" || importedWeek == "" || len(schedule) == 0 {
		return
	}
	if s.config.MidweekLanguageSchedules == nil {
		s.config.MidweekLanguageSchedules = map[string]MidweekLanguageSchedule{}
	}
	cached := make([]Talk, len(schedule))
	copy(cached, schedule)
	normalizeSchedule(cached)
	s.config.MidweekLanguageSchedules[language] = MidweekLanguageSchedule{
		ImportedWeek: importedWeek,
		URL:          sourceURL,
		Schedule:     cached,
	}
}

// applyWeekendLanguageLocked switches the language of the weekend programme.
// That programme is a local template whose two titles come from the language
// alone (see weekendSchedule), so it needs nothing from WOL — but the switch
// used to run the midweek path regardless, which meant a Sunday with flaky hall
// wifi or an unpublished workbook refused the change outright.
//
// The midweek baseline is deliberately left alone: this changes what is on
// screen now, not which workbook the hall imports. The pre-load sweep keeps the
// midweek cache warm separately.
func (s *server) applyWeekendLanguageLocked(now time.Time, language string) (Config, State, bool, string) {
	language = normalizeMidweekLanguage(language)
	if language == "" {
		return Config{}, State{}, false, "unsupported language"
	}
	if s.state.Status != StatusIdle {
		return Config{}, State{}, false, "language can only be changed while idle"
	}

	s.config.MidweekLanguage = language
	// The operator chose this language; hold that choice for the session so the
	// idle sync and the import rotation can't silently flip it back.
	s.config.MidweekLanguageOverrideUntil = now.Add(circuitOverseerDuration)
	s.state.MidweekLanguage = language
	s.applyActiveScheduleChangeLocked(now)
	state := s.snapshotLocked()
	return s.config, state, true, ""
}

func (s *server) applyCachedMidweekLanguageScheduleLocked(now time.Time, language string) (Config, State, bool, string) {
	language = normalizeMidweekLanguage(language)
	if language == "" {
		return Config{}, State{}, false, "unsupported language"
	}
	if s.state.Status != StatusIdle {
		return Config{}, State{}, false, "language can only be changed while idle"
	}
	cached, ok := s.config.MidweekLanguageSchedules[language]
	if !ok || cached.ImportedWeek != isoWeekString(now) || len(cached.Schedule) == 0 {
		return Config{}, State{}, false, fmt.Sprintf("%s items are not imported for this week yet", languageName(language))
	}
	if s.cachedLanguageScheduleStaleLocked(cached) {
		return Config{}, State{}, false, fmt.Sprintf("%s items are not imported for this week yet", languageName(language))
	}
	s.config.MeetingType = "midweek"
	s.config.MidweekURL = cached.URL
	s.config.MidweekLanguage = language
	s.config.MidweekImportedWeek = cached.ImportedWeek
	// The operator chose this language; hold that choice for the session so the
	// idle sync and the import rotation can't silently flip it back.
	s.config.MidweekLanguageOverrideUntil = now.Add(circuitOverseerDuration)
	s.setBaselineScheduleLocked(append([]Talk(nil), cached.Schedule...))
	normalizeSchedule(s.config.Schedule)
	s.state.MidweekLanguage = language
	s.applyActiveScheduleChangeLocked(now)
	// Snapshot before copying config: snapshotLocked recalculates, and the
	// returned pair must agree about what was applied.
	state := s.snapshotLocked()
	return s.config, state, true, ""
}

// cachedLanguageScheduleStaleLocked reports whether a cached language schedule
// was stamped with the wrong week. Workbook docids are language-independent per
// week, so a cache whose docid disagrees with the active import of the same
// week holds a stale document recorded as current; callers treat it as missing
// so a fresh import replaces it instead of serving last week's items.
func (s *server) cachedLanguageScheduleStaleLocked(cached MidweekLanguageSchedule) bool {
	id := wolDocID(cached.URL)
	if id == "" || s.config.MidweekImportedWeek != cached.ImportedWeek {
		return false
	}
	activeID := wolDocID(s.config.MidweekURL)
	return activeID != "" && activeID != id
}

// importMidweekLanguage fetches a language's parts from WOL and applies them.
// The caller must NOT hold s.mu: this blocks on the network, so it takes the
// lock only to read the source URL and again to apply the result.
func (s *server) importMidweekLanguage(ctx context.Context, now time.Time, language string) (Config, State, bool, string) {
	language = normalizeMidweekLanguage(language)
	if language == "" {
		return Config{}, State{}, false, "unsupported language"
	}
	// midweekLanguageSourceLocked reads s.config maps, which the auto-import
	// goroutine writes; reading them unlocked aborts the process.
	s.mu.Lock()
	sourceURL, ok := s.midweekLanguageSourceLocked(language)
	s.mu.Unlock()
	if !ok {
		return Config{}, State{}, false, fmt.Sprintf("%s items are not available for this hall", languageName(language))
	}

	// The stored source is a specific week's document, so fetching it directly
	// would import whatever week it was saved in and stamp it as current. Only
	// its language/library segments are trusted; the date-addressable weekly
	// meetings page decides which document is actually this week's.
	page, err := fetchWOLPageFunc(ctx, weeklyMeetingsURL(sourceURL, now))
	if err != nil {
		return Config{}, State{}, false, fmt.Sprintf("could not import %s items: %v", languageName(language), err)
	}
	docURL, ok := findWorkbookDocURL(page)
	if !ok {
		return Config{}, State{}, false, fmt.Sprintf("could not find this week's %s items on WOL", languageName(language))
	}
	schedule, err := importMidweekFromURL(ctx, docURL)
	if err != nil {
		return Config{}, State{}, false, fmt.Sprintf("could not import %s items: %v", languageName(language), err)
	}
	if err := validateImportedLanguage(language, schedule); err != nil {
		return Config{}, State{}, false, err.Error()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Status != StatusIdle {
		return Config{}, State{}, false, "language can only be changed while idle"
	}
	importedWeek := isoWeekString(now)
	s.config.MeetingType = "midweek"
	s.config.MidweekURL = docURL
	s.config.MidweekLanguage = language
	s.config.MidweekImportedWeek = importedWeek
	// The operator chose this language; hold that choice for the session so the
	// idle sync and the import rotation can't silently flip it back.
	s.config.MidweekLanguageOverrideUntil = now.Add(circuitOverseerDuration)
	if s.config.MidweekLanguageSources == nil {
		s.config.MidweekLanguageSources = map[string]string{}
	}
	s.config.MidweekLanguageSources[language] = docURL
	s.storeMidweekLanguageScheduleLocked(language, importedWeek, docURL, schedule)
	s.setBaselineScheduleLocked(append([]Talk(nil), schedule...))
	s.applyActiveScheduleChangeLocked(now)
	// Snapshot before copying config: snapshotLocked recalculates, and the
	// returned pair must agree about what was applied.
	state := s.snapshotLocked()
	return s.config, state, true, ""
}

func sameAutoImportSource(a, b autoImportSource) bool {
	return a.exampleURL == b.exampleURL && a.startID == b.startID
}

func validateImportedLanguage(language string, schedule []Talk) error {
	language = normalizeMidweekLanguage(language)
	if language == "" {
		return nil
	}
	// Symmetric on purpose: English used to be waved through, which let a
	// poisoned source cache a Twi workbook as "English" — the one language
	// with no check was the one that got the wrong items.
	if language == "en" {
		if !looksLikeEnglishMidweekSchedule(schedule) {
			return errors.New("WOL returned non-English titles for English")
		}
		return nil
	}
	if looksLikeEnglishMidweekSchedule(schedule) {
		return fmt.Errorf("WOL returned English titles for %s", languageName(language))
	}
	return nil
}

// healMidweekLanguageBookkeeping drops per-language bookkeeping whose URL
// disagrees with the language it is filed under. Config written before the
// weekend-language fix above can hold such entries — a Twi source or cached
// schedule filed under "en" — and every path that reads them trusts the key,
// so a switch to English would put Twi items on screen labelled English.
// Dropping is safe: sources fall back to the built-in defaults and a missing
// cache is re-imported, both in the right language.
//
// The active language gets the same reconciliation — the applied schedule came
// from MidweekURL, so when the two disagree the URL is the truth — but only
// once any operator override has lapsed: inside the override window a weekend
// language choice legitimately disagrees with the midweek URL.
func healMidweekLanguageBookkeeping(config *Config, now time.Time) {
	for language, cached := range config.MidweekLanguageSchedules {
		if actual := wolLanguage(cached.URL); actual != "" && actual != language {
			delete(config.MidweekLanguageSchedules, language)
		}
	}
	for language, source := range config.MidweekLanguageSources {
		if actual := wolLanguage(source); actual != "" && actual != language {
			delete(config.MidweekLanguageSources, language)
		}
	}
	if now.Before(config.MidweekLanguageOverrideUntil) {
		return
	}
	if actual := wolLanguage(config.MidweekURL); actual != "" && actual != config.MidweekLanguage {
		config.MidweekLanguage = actual
	}
}

func looksLikeEnglishMidweekSchedule(schedule []Talk) bool {
	englishTitles := map[string]struct{}{
		"opening comments":                     {},
		"spiritual gems":                       {},
		"bible reading":                        {},
		"starting a conversation":              {},
		"following up":                         {},
		"making disciples":                     {},
		"explaining your beliefs":              {},
		"talk":                                 {},
		"congregation bible study":             {},
		"concluding comments":                  {},
		"local needs":                          {},
		"living as christians":                 {},
		"apply yourself to the field ministry": {},
	}

	matches := 0
	for _, talk := range schedule {
		if _, ok := englishTitles[strings.ToLower(strings.TrimSpace(talk.Title))]; ok {
			matches++
		}
	}
	return matches >= 2
}

func languageName(language string) string {
	switch normalizeMidweekLanguage(language) {
	case "es":
		return "Spanish"
	case "tw":
		return "Twi"
	default:
		return "English"
	}
}

type autoImportSource struct {
	exampleURL   string
	importedWeek string
	startID      int
}

func (s *server) nextAutoImportSourceLocked(now time.Time) (autoImportSource, bool) {
	if !shouldAutoImportNow(now, s.config.AutoImportMidweek, "") {
		return autoImportSource{}, false
	}
	if s.currentMidweekMeetingActiveLocked(now) {
		return autoImportSource{}, false
	}

	start, ok := nextMidweekMeetingStart(now, s.config.MeetingStarts)
	if !ok {
		if s.config.MidweekImportedWeek == isoWeekString(now) {
			return autoImportSource{}, false
		}
		return autoImportSource{exampleURL: s.config.MidweekURL, importedWeek: s.config.MidweekImportedWeek}, true
	}

	source := meetingStartSource(start, s.config.MidweekURL)
	if source.startID == 0 {
		source.importedWeek = s.config.MidweekImportedWeek
	}
	if source.importedWeek == isoWeekString(now) {
		return source, false
	}
	return source, true
}

func (s *server) nextAutoImportCheckAtLocked(now time.Time) time.Time {
	if !s.config.AutoImportMidweek || now.Before(currentWeekAutoImportAt(now)) {
		return nextAutoImportAt(now)
	}

	nextCheck := now.Add(time.Hour)
	if _, startAt, ok := nextMidweekMeetingStartAt(now, s.config.MeetingStarts); ok && startAt.After(now) && startAt.Before(nextCheck) {
		nextCheck = startAt
	}
	if _, activeUntil, ok := s.currentMidweekMeetingWindowLocked(now); ok && activeUntil.After(now) && activeUntil.Before(nextCheck) {
		nextCheck = activeUntil
	}
	return nextCheck
}

func nextMidweekMeetingStart(now time.Time, starts []MeetingStart) (MeetingStart, bool) {
	start, _, ok := nextMidweekMeetingStartAt(now, starts)
	return start, ok
}

func nextMidweekMeetingStartAt(now time.Time, starts []MeetingStart) (MeetingStart, time.Time, bool) {
	weekStart := currentWeekAutoImportAt(now)
	weekStart = time.Date(weekStart.Year(), weekStart.Month(), weekStart.Day(), 0, 0, 0, 0, now.Location())

	var selected MeetingStart
	var selectedAt time.Time
	found := false
	for _, start := range starts {
		if start.Day < int(time.Monday) || start.Day > int(time.Friday) {
			continue
		}
		parsed, err := parseClockTime(start.Time)
		if err != nil {
			continue
		}
		startAt := weekStart.AddDate(0, 0, start.Day-int(time.Monday))
		startAt = time.Date(startAt.Year(), startAt.Month(), startAt.Day(), parsed.hour, parsed.minute, 0, 0, now.Location())
		if startAt.Before(now) {
			continue
		}
		if !found || startAt.Before(selectedAt) {
			selected = start
			selectedAt = startAt
			found = true
		}
	}
	return selected, selectedAt, found
}

func (s *server) currentMidweekMeetingActiveLocked(now time.Time) bool {
	_, _, ok := s.currentMidweekMeetingWindowLocked(now)
	return ok
}

func (s *server) currentMidweekMeetingWindowLocked(now time.Time) (time.Time, time.Time, bool) {
	var latestStart time.Time
	found := false
	for _, start := range s.config.MeetingStarts {
		if start.Day < int(time.Monday) || start.Day > int(time.Friday) || start.Day != int(now.Weekday()) {
			continue
		}
		parsed, err := parseClockTime(start.Time)
		if err != nil {
			continue
		}
		startAt := time.Date(now.Year(), now.Month(), now.Day(), parsed.hour, parsed.minute, 0, 0, now.Location())
		if !startAt.Before(now) {
			continue
		}
		if !found || startAt.After(latestStart) {
			latestStart = startAt
			found = true
		}
	}
	if !found {
		return time.Time{}, time.Time{}, false
	}

	activeUntil := latestStart.Add(circuitOverseerDuration)
	for _, start := range s.config.MeetingStarts {
		if start.Day < int(time.Monday) || start.Day > int(time.Friday) {
			continue
		}
		parsed, err := parseClockTime(start.Time)
		if err != nil {
			continue
		}
		startAt := time.Date(now.Year(), now.Month(), now.Day(), parsed.hour, parsed.minute, 0, 0, now.Location())
		if start.Day != int(now.Weekday()) || !startAt.After(latestStart) {
			continue
		}
		if startAt.Before(activeUntil) {
			activeUntil = startAt
		}
	}
	if !now.Before(activeUntil) {
		return time.Time{}, time.Time{}, false
	}
	return latestStart, activeUntil, true
}

func shouldAutoImportNow(now time.Time, enabled bool, importedWeek string) bool {
	if !enabled || importedWeek == isoWeekString(now) {
		return false
	}
	return !now.Before(currentWeekAutoImportAt(now))
}

func currentWeekAutoImportAt(now time.Time) time.Time {
	daysSinceMonday := (int(now.Weekday()) - int(time.Monday) + 7) % 7
	year, month, day := now.Date()
	return time.Date(year, month, day-daysSinceMonday, autoImportHour, 0, 0, 0, now.Location())
}

func nextAutoImportAt(now time.Time) time.Time {
	current := currentWeekAutoImportAt(now)
	if now.Before(current) {
		return current
	}
	return current.AddDate(0, 0, 7)
}

func readLimitedString(reader io.Reader, maxBytes int64) (string, error) {
	var buf bytes.Buffer
	limited := io.LimitReader(reader, maxBytes+1)
	if _, err := buf.ReadFrom(limited); err != nil {
		return "", err
	}
	if int64(buf.Len()) > maxBytes {
		return "", errors.New("midweek page is too large")
	}
	return buf.String(), nil
}

func parseMidweekTimings(input string) ([]Talk, error) {
	text := htmlToText(input)
	text = strings.ReplaceAll(text, "\u00a0", " ")

	var talks []Talk
	seen := map[string]struct{}{}
	previousTitle := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		titleText, minutesText, ok := extractTiming(line)
		if !ok {
			if !looksLikeTimingDetail(line) {
				previousTitle = cleanTimingTitle(line)
			}
			continue
		}

		title := cleanTimingTitle(titleText)
		if title == "" {
			title = previousTitle
		}
		if title == "" {
			continue
		}
		minutes, err := parsePositiveInt(minutesText)
		if err != nil || minutes <= 0 || minutes > 120 {
			continue
		}
		key := strings.ToLower(fmt.Sprintf("%s:%d", title, minutes))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		talks = append(talks, Talk{
			ID:       len(talks) + 1,
			Title:    title,
			Duration: minutes * 60,
			Closing:  derivedClosingSeconds(minutes),
		})
		previousTitle = ""
	}

	if len(talks) == 0 {
		return nil, errors.New("no timing slots found")
	}
	normalizeSchedule(talks)
	return talks, nil
}

var timingPattern = regexp.MustCompile(`(?i)\(\s*(?:(\d{1,3})\s*(?:min|mins|minutes|simma)\.?|(?:min|mins|minutes|simma)\.?\s*(\d{1,3}))\s*\)`)

func extractTiming(line string) (string, string, bool) {
	match := timingPattern.FindStringSubmatchIndex(line)
	if match == nil {
		return "", "", false
	}
	minutes := ""
	if match[2] >= 0 {
		minutes = line[match[2]:match[3]]
	} else {
		minutes = line[match[4]:match[5]]
	}
	return strings.TrimSpace(line[:match[0]]), minutes, true
}

func looksLikeTimingDetail(line string) bool {
	return strings.HasPrefix(line, "(") || strings.HasSuffix(line, ")")
}

func htmlToText(input string) string {
	replacements := []struct {
		old string
		new string
	}{
		{"</p>", "\n"},
		{"</h1>", "\n"},
		{"</h2>", "\n"},
		{"</h3>", "\n"},
		{"</h4>", "\n"},
		{"</div>", "\n"},
		{"</li>", "\n"},
		{"<br>", "\n"},
		{"<br/>", "\n"},
		{"<br />", "\n"},
	}
	for _, replacement := range replacements {
		input = strings.ReplaceAll(input, replacement.old, replacement.new)
	}

	tagPattern := regexp.MustCompile(`<[^>]+>`)
	input = tagPattern.ReplaceAllString(input, " ")
	entityPattern := regexp.MustCompile(`&[^;\s]+;`)
	input = entityPattern.ReplaceAllStringFunc(input, decodeHTMLEntity)
	spacePattern := regexp.MustCompile(`[ \t]+`)
	input = spacePattern.ReplaceAllString(input, " ")
	linePattern := regexp.MustCompile(`\n\s+`)
	return linePattern.ReplaceAllString(input, "\n")
}

func decodeHTMLEntity(entity string) string {
	switch entity {
	case "&amp;":
		return "&"
	case "&quot;":
		return `"`
	case "&#39;", "&apos;":
		return "'"
	case "&nbsp;":
		return " "
	case "&lt;":
		return "<"
	case "&gt;":
		return ">"
	default:
		return " "
	}
}

func cleanTimingTitle(title string) string {
	title = strings.TrimSpace(title)
	if strings.Contains(title, "|") {
		parts := strings.Split(title, "|")
		title = parts[len(parts)-1]
	}
	title = regexp.MustCompile(`^[\s\d.:-]+`).ReplaceAllString(title, "")
	title = regexp.MustCompile(`\s+`).ReplaceAllString(title, " ")
	title = strings.Trim(title, " -:\t\r\n")
	if len(title) < 2 {
		return ""
	}
	return title
}

func parsePositiveInt(value string) (int, error) {
	var parsed int
	_, err := fmt.Sscanf(value, "%d", &parsed)
	return parsed, err
}
