package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testPIN = "481902"

func newPairingTestServer(t *testing.T) (*server, *http.ServeMux) {
	t.Helper()
	srv, err := newServer(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}
	return srv, mux
}

// newSecuredTestServer returns a server with a PIN already set, i.e. the state
// a hall is in after setup — the bootstrap window shut.
func newSecuredTestServer(t *testing.T) (*server, *http.ServeMux) {
	t.Helper()
	srv, mux := newPairingTestServer(t)
	setPIN(t, srv, mux, testPIN)
	return srv, mux
}

func setPIN(t *testing.T, srv *server, mux *http.ServeMux, pin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/pairing/pin", strings.NewReader(`{"pin":"`+pin+`"}`))
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	return res
}

func claimPIN(mux *http.ServeMux, pin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/pairing/claim", strings.NewReader(`{"pin":"`+pin+`"}`))
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	return res
}

func pairingBody(t *testing.T, mux *http.ServeMux) map[string]any {
	t.Helper()
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/pairing", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("expected OK pairing status, got %d", res.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// The point of the rework: a LAN client can no longer read a token out of the
// pairing endpoint, which is how it used to bypass every protected route.
func TestPairingEndpointNeverLeaksControlToken(t *testing.T) {
	srv, mux := newSecuredTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/pairing", nil)
	req.Host = "hallclock.local:8080"
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected OK pairing response, got %d", res.Code)
	}
	if strings.Contains(res.Body.String(), srv.config.ControlToken) {
		t.Fatalf("pairing endpoint leaked the control token: %s", res.Body.String())
	}
}

// The PIN is a shared secret for the whole hall, so it must never come back out
// of the API — not in the config the setup page reads, and not in the state
// stream every subscriber gets.
func TestPINNeverLeavesTheServer(t *testing.T) {
	srv, mux := newSecuredTestServer(t)

	for _, path := range []string{"/api/config", "/api/state", "/api/pairing"} {
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		body := res.Body.String()
		if strings.Contains(body, testPIN) {
			t.Fatalf("%s leaked the PIN: %s", path, body)
		}
	}

	state, err := json.Marshal(srv.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state), testPIN) {
		t.Fatalf("broadcast state leaked the PIN: %s", state)
	}
}

// The PIN is stored as typed so /setup can show which one is current. That is a
// deliberate trade — ControlToken sits in the same file in the clear and is
// strictly more powerful — but it must still survive a restart intact.
func TestPINIsPersistedAndVerifies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	srv, err := newServer(path)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}
	if res := setPIN(t, srv, mux, testPIN); res.Code != http.StatusOK {
		t.Fatalf("expected the PIN to save, got %d: %s", res.Code, res.Body.String())
	}

	saved, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ControlPIN != testPIN {
		t.Fatalf("expected the PIN to persist, got %q", saved.ControlPIN)
	}
	if !pinMatches(saved.ControlPIN, testPIN) {
		t.Fatal("expected the persisted PIN to verify")
	}
	if pinMatches(saved.ControlPIN, "000000") {
		t.Fatal("expected a wrong PIN to be rejected")
	}
}

// The pairing status advertises the PIN's length (not the PIN) so the dialog
// can claim on the final digit instead of demanding a button tap.
func TestPairingStatusReportsPINLength(t *testing.T) {
	srv, mux := newSecuredTestServer(t)
	_ = srv

	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/pairing", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("expected OK pairing status, got %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), `"pinLength":6`) {
		t.Fatalf("expected pinLength for a set PIN, got %s", res.Body.String())
	}
	if strings.Contains(res.Body.String(), testPIN) {
		t.Fatalf("pairing status leaked the PIN itself: %s", res.Body.String())
	}
}

// Two halls on one network look identical from an unpaired phone. The PIN
// prompt names the hall, so a phone that opened the wrong one can tell.
func TestPairingStatusNamesTheHall(t *testing.T) {
	srv, mux := newSecuredTestServer(t)
	srv.mu.Lock()
	srv.config.DeviceName = "East Hall"
	srv.mu.Unlock()

	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/pairing", nil))
	var status struct {
		DeviceName string `json:"deviceName"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &status); err != nil || status.DeviceName != "East Hall" {
		t.Fatalf("expected the hall's name in pairing status, got %s (%v)", res.Body.String(), err)
	}
}

// Before a PIN exists there is no length to advertise — the field would only
// invite the dialog to auto-claim against a PIN nobody can type.
func TestPairingStatusOmitsPINLengthWhenUnset(t *testing.T) {
	_, mux := newPairingTestServer(t)

	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/pairing", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("expected OK pairing status, got %d", res.Code)
	}
	if strings.Contains(res.Body.String(), "pinLength") {
		t.Fatalf("expected no pinLength before a PIN is set, got %s", res.Body.String())
	}
}

// A paired controller can read the PIN back — that is the point of storing it —
// but an unpaired caller must not.
func TestShowPINRequiresAToken(t *testing.T) {
	srv, mux := newSecuredTestServer(t)

	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/pairing/pin", nil))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized without a token, got %d", res.Code)
	}
	if strings.Contains(res.Body.String(), testPIN) {
		t.Fatalf("unauthorized response leaked the PIN: %s", res.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/pairing/pin", nil)
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	authed := httptest.NewRecorder()
	mux.ServeHTTP(authed, req)
	if authed.Code != http.StatusOK {
		t.Fatalf("expected a paired controller to read the PIN, got %d", authed.Code)
	}
	var body struct {
		PIN           string `json:"pin"`
		PINConfigured bool   `json:"pinConfigured"`
	}
	if err := json.Unmarshal(authed.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.PIN != testPIN || !body.PINConfigured {
		t.Fatalf("expected the current PIN back, got %+v", body)
	}
}

func TestPINPairsAndUnlocksProtectedRoutes(t *testing.T) {
	srv, mux := newSecuredTestServer(t)

	// A phone with no token is shut out.
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/api/control/start", nil))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized before pairing, got %d", res.Code)
	}
	if got := claimPIN(mux, "000000"); got.Code != http.StatusUnauthorized {
		t.Fatalf("expected a wrong PIN to be refused, got %d", got.Code)
	}

	claim := claimPIN(mux, testPIN)
	if claim.Code != http.StatusOK {
		t.Fatalf("expected the PIN to pair, got %d: %s", claim.Code, claim.Body.String())
	}
	var paired struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(claim.Body.Bytes(), &paired); err != nil {
		t.Fatal(err)
	}
	if paired.Token != srv.config.ControlToken {
		t.Fatalf("expected the control token back, got %q", paired.Token)
	}

	start := httptest.NewRequest(http.MethodPost, "/api/control/start", nil)
	start.Header.Set("X-Wall-Clock-Token", paired.Token)
	startRes := httptest.NewRecorder()
	mux.ServeHTTP(startRes, start)
	if startRes.Code != http.StatusOK {
		t.Fatalf("expected the paired token to work, got %d: %s", startRes.Code, startRes.Body.String())
	}
}

// A long-lived PIN has to survive somebody grinding at it, so wrong guesses buy
// a lockout rather than another turn.
func TestWrongPINsLockOutGuessing(t *testing.T) {
	srv, mux := newSecuredTestServer(t)
	now := time.Now()
	srv.clock = func() time.Time { return now }

	for i := 1; i < maxPINFailures; i++ {
		if res := claimPIN(mux, "000000"); res.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i, res.Code)
		}
	}
	if res := claimPIN(mux, "000000"); res.Code != http.StatusUnauthorized {
		t.Fatalf("expected the last wrong attempt to be a 401, got %d", res.Code)
	}
	// Locked now: even the right PIN waits.
	if res := claimPIN(mux, testPIN); res.Code != http.StatusTooManyRequests {
		t.Fatalf("expected a lockout, got %d: %s", res.Code, res.Body.String())
	}

	now = now.Add(pinLockout + time.Second)
	if res := claimPIN(mux, testPIN); res.Code != http.StatusOK {
		t.Fatalf("expected pairing to work once the lockout lapsed, got %d", res.Code)
	}
}

// Guessing in parallel must not buy more attempts than guessing in series.
// Counting a failure only after the (deliberately slow) derivation finished let
// a whole burst read an untripped counter and slip through together, turning a
// five-per-lockout cap into as many tries as the caller could open sockets.
func TestConcurrentWrongPINsCannotOutrunTheLockout(t *testing.T) {
	_, mux := newSecuredTestServer(t)

	const burst = 40
	codes := make(chan int, burst)
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- claimPIN(mux, "000000").Code
		}()
	}
	wg.Wait()
	close(codes)

	tried := 0
	for code := range codes {
		if code == http.StatusUnauthorized {
			tried++
		}
	}
	if tried > maxPINFailures {
		t.Fatalf("a burst of %d got %d guesses past a %d-attempt cap", burst, tried, maxPINFailures)
	}
	// And the lockout really did engage, rather than the burst simply failing.
	if res := claimPIN(mux, testPIN); res.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the burst to trip the lockout, got %d", res.Code)
	}
}

func TestSuccessfulPairingClearsFailureCount(t *testing.T) {
	_, mux := newSecuredTestServer(t)

	for i := 0; i < maxPINFailures-1; i++ {
		claimPIN(mux, "000000")
	}
	if res := claimPIN(mux, testPIN); res.Code != http.StatusOK {
		t.Fatalf("expected the right PIN to pair, got %d", res.Code)
	}
	// The counter reset, so a fresh run of wrong guesses gets the full budget
	// rather than tripping the lockout immediately.
	for i := 1; i < maxPINFailures; i++ {
		if res := claimPIN(mux, "000000"); res.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d after a success: expected 401, got %d", i, res.Code)
		}
	}
}

// First boot with no PIN has to let somebody in, or the appliance could never
// be set up without SSH.
func TestFirstBootGraceWindowPairsWithoutAPIN(t *testing.T) {
	_, mux := newPairingTestServer(t)

	status := pairingBody(t, mux)
	if status["pairingOpen"] != true || status["pinConfigured"] != false {
		t.Fatalf("expected an open bootstrap window with no PIN set, got %v", status)
	}
	if res := claimPIN(mux, ""); res.Code != http.StatusOK {
		t.Fatalf("expected the grace window to pair without a PIN, got %d: %s", res.Code, res.Body.String())
	}
}

// The bootstrap window must survive the first phone pairing. ensurePaired
// claims silently on page load, so a one-shot window was consumed by whichever
// origin was opened first — leaving the setup page on another origin (or
// another device) prompting for a PIN that had never been set, with no way to
// set one. Time and setting a PIN already bound this window.
func TestGraceWindowSurvivesPairingAPhone(t *testing.T) {
	srv, mux := newPairingTestServer(t)

	// Browser A opens /control and pairs itself with no UI at all.
	if res := claimPIN(mux, ""); res.Code != http.StatusOK {
		t.Fatalf("expected the first phone to pair, got %d", res.Code)
	}
	// Browser B — a different origin, so a different localStorage — opens /setup.
	if res := claimPIN(mux, ""); res.Code != http.StatusOK {
		t.Fatalf("expected the bootstrap window to still be open, got %d: %s", res.Code, res.Body.String())
	}
	if !srv.pairingOpenLocked(srv.clock()) {
		t.Fatal("expected the bootstrap window to stay open for its full term")
	}
}

func TestGraceWindowExpires(t *testing.T) {
	srv, mux := newPairingTestServer(t)
	now := time.Now()
	srv.clock = func() time.Time { return now }

	now = now.Add(pairingGrace + time.Second)
	if res := claimPIN(mux, ""); res.Code != http.StatusForbidden {
		t.Fatalf("expected the lapsed grace window to refuse, got %d: %s", res.Code, res.Body.String())
	}
}

// Setting a PIN is the moment the appliance becomes secured, so the wide-open
// bootstrap window must not outlive it.
func TestSettingAPINClosesTheGraceWindow(t *testing.T) {
	srv, mux := newPairingTestServer(t)
	if !srv.pairingOpenLocked(srv.clock()) {
		t.Fatal("expected the bootstrap window to start open")
	}

	if res := setPIN(t, srv, mux, testPIN); res.Code != http.StatusOK {
		t.Fatalf("expected the PIN to save, got %d", res.Code)
	}
	if srv.pairingOpenLocked(srv.clock()) {
		t.Fatal("expected setting a PIN to close the bootstrap window")
	}
	if res := claimPIN(mux, ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("expected an empty PIN to be refused once one is set, got %d", res.Code)
	}
}

// A restart with a PIN configured must not reopen the door.
func TestGraceWindowDoesNotReopenOnceAPINIsSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	srv, err := newServer(path)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := srv.routes("")
	if err != nil {
		t.Fatal(err)
	}
	if res := setPIN(t, srv, mux, testPIN); res.Code != http.StatusOK {
		t.Fatalf("expected the PIN to save, got %d", res.Code)
	}

	restarted, err := newServer(path)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.pairingOpenLocked(time.Now()) {
		t.Fatal("expected a restart with a PIN set to come up closed")
	}
}

// Adding a second phone should not require telling anyone the PIN.
func TestPairedControllerCanOpenAWindowForAnotherPhone(t *testing.T) {
	srv, mux := newSecuredTestServer(t)

	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/api/pairing/enable", nil))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized without a token, got %d", res.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/pairing/enable", nil)
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	authed := httptest.NewRecorder()
	mux.ServeHTTP(authed, req)
	if authed.Code != http.StatusOK {
		t.Fatalf("expected a paired controller to open pairing, got %d", authed.Code)
	}

	if res := claimPIN(mux, ""); res.Code != http.StatusOK {
		t.Fatalf("expected the open window to pair without a PIN, got %d", res.Code)
	}
	// One window, one phone.
	if res := claimPIN(mux, ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("expected the window to close after a phone joined, got %d", res.Code)
	}
}

func TestSetPINRejectsWeakInput(t *testing.T) {
	srv, mux := newSecuredTestServer(t)

	for _, pin := range []string{"", "1", "abc", strings.Repeat("9", maxPINLength+1)} {
		if res := setPIN(t, srv, mux, pin); res.Code != http.StatusBadRequest {
			t.Fatalf("expected %q to be refused, got %d", pin, res.Code)
		}
	}
	// The old PIN still works after a rejected change.
	if res := claimPIN(mux, testPIN); res.Code != http.StatusOK {
		t.Fatalf("expected the existing PIN to survive a rejected change, got %d", res.Code)
	}
}

func TestSetPINRequiresAnExistingToken(t *testing.T) {
	_, mux := newSecuredTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/pairing/pin", strings.NewReader(`{"pin":"999999"}`))
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized without a token, got %d", res.Code)
	}
	if got := claimPIN(mux, "999999"); got.Code != http.StatusUnauthorized {
		t.Fatalf("expected the unauthorized PIN change to have no effect, got %d", got.Code)
	}
}

// Changing the PIN must not evict the phones already in use — an operator
// mid-meeting should never be logged out by somebody tidying up settings.
func TestChangingThePINKeepsPairedPhonesWorking(t *testing.T) {
	srv, mux := newSecuredTestServer(t)
	token := srv.config.ControlToken

	if res := setPIN(t, srv, mux, "775533"); res.Code != http.StatusOK {
		t.Fatalf("expected the PIN change to save, got %d", res.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/control/start", nil)
	req.Header.Set("X-Wall-Clock-Token", token)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected an already-paired phone to keep working, got %d", res.Code)
	}
	if got := claimPIN(mux, "775533"); got.Code != http.StatusOK {
		t.Fatalf("expected the new PIN to pair, got %d", got.Code)
	}
	if got := claimPIN(mux, testPIN); got.Code != http.StatusUnauthorized {
		t.Fatalf("expected the old PIN to stop working, got %d", got.Code)
	}
}

// A config reset mints a fresh ControlToken, so every browser is left holding a
// dead one. The client checks with this endpoint before trusting what it stored;
// without it, ensurePaired returned the stale token and every write 401'd
// silently, forever.
func TestVerifyTokenRejectsAStaleToken(t *testing.T) {
	srv, mux := newSecuredTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/pairing/verify", nil)
	req.Header.Set("X-Wall-Clock-Token", srv.config.ControlToken)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("expected a live token to verify, got %d", res.Code)
	}

	stale := httptest.NewRequest(http.MethodGet, "/api/pairing/verify", nil)
	stale.Header.Set("X-Wall-Clock-Token", "a-token-from-a-config-that-was-reset")
	staleRes := httptest.NewRecorder()
	mux.ServeHTTP(staleRes, stale)
	if staleRes.Code != http.StatusUnauthorized {
		t.Fatalf("expected a stale token to be rejected, got %d", staleRes.Code)
	}
}
