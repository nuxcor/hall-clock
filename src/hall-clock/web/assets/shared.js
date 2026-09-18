(function () {
  const TOKEN_KEY = "wallClockControlToken";

  // Safari with storage blocked throws on every localStorage call, and an
  // unguarded throw in getToken stopped the controller before it subscribed,
  // leaving a dead 00:00. Keep a copy in memory as well: without storage the
  // phone asks for the PIN on each visit, which beats not working at all.
  // Read storage first, so a token another tab re-paired with still wins.
  let memoryToken = "";

  function storeToken(token) {
    memoryToken = token;
    try {
      localStorage.setItem(TOKEN_KEY, token);
    } catch {
      // Storage denied: memoryToken carries it for this page's lifetime.
    }
  }

  function getToken() {
    const params = new URLSearchParams(window.location.search);
    const token = params.get("token");
    if (token) {
      storeToken(token);
      params.delete("token");
      const query = params.toString();
      const clean = window.location.pathname + (query ? `?${query}` : "");
      window.history.replaceState({}, "", clean);
    }
    try {
      return localStorage.getItem(TOKEN_KEY) || memoryToken;
    } catch {
      return memoryToken;
    }
  }

  // A request that never settles leaves its caller's "pending" flag set forever,
  // which is how a dropped wifi link used to permanently disable the Start
  // button. Always give fetch a deadline. Imports and updates talk to the
  // network and are legitimately slow, so the default is generous; the control
  // buttons pass something short because a person is waiting on them.
  const DEFAULT_TIMEOUT_MS = 30000;

  async function postJSON(path, body, options) {
    const timeoutMs = (options && options.timeoutMs) || DEFAULT_TIMEOUT_MS;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    let response;
    try {
      response = await fetch(path, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-Wall-Clock-Token": getToken(),
        },
        body: JSON.stringify(body || {}),
        signal: controller.signal,
      });
    } catch (cause) {
      const error = new Error(cause && cause.name === "AbortError" ? `${path} timed out` : String(cause));
      error.timedOut = cause && cause.name === "AbortError";
      throw error;
    } finally {
      clearTimeout(timer);
    }
    if (!response.ok) {
      // A 401 means this browser's token is dead — usually the config was reset
      // and the server minted a new one. Drop it so the next ensurePaired()
      // pairs again instead of retrying a credential that can never work.
      if (response.status === 401 && path !== "/api/pairing/claim") {
        clearToken();
      }
      // Carry the status: callers must tell "your token is bad" apart from an
      // ordinary refusal like advancing past the last part of the meeting.
      const error = new Error(await response.text());
      error.status = response.status;
      throw error;
    }
    return response.json();
  }

  // Pairing asks for the hall's PIN, which the congregation sets in /setup.
  // Nothing about pairing ever touches the display: the TV shows the clock and
  // only the clock. Before prompting, check whether a no-PIN window is open —
  // the first-boot grace period, or "add a phone" started from a paired
  // controller — and if so pair silently.

  function clearToken() {
    memoryToken = "";
    try {
      localStorage.removeItem(TOKEN_KEY);
    } catch {
      // Storage denied: there was nothing stored to remove.
    }
  }

  // tokenWorks asks the server whether the stored token is still valid. Trusting
  // localStorage blindly is what turned a config reset into a browser that
  // silently 401s every write and never re-pairs.
  async function tokenWorks(token) {
    if (!token) return false;
    try {
      const response = await fetch("/api/pairing/verify", {
        headers: { "X-Wall-Clock-Token": token },
      });
      // Only a 401 says the token is bad. Anything else -- a 502 from Caddy
      // while the app restarts, say -- is the clock being unreachable, and
      // wiping a good token for it threw the operator into a PIN prompt
      // mid-meeting. Treat it like the offline case below.
      if (response.status === 401) return false;
      if (!response.ok) console.error(`pairing check answered ${response.status}`);
      return true;
    } catch (error) {
      // Offline is not the same as unpaired: keep the token and let the page
      // retry rather than dumping the operator into a PIN prompt mid-meeting.
      console.error(error);
      return true;
    }
  }

  async function pairingStatus() {
    const response = await fetch("/api/pairing");
    if (!response.ok) throw new Error("pairing status unavailable");
    return response.json();
  }

  async function claimPairing(pin) {
    const result = await postJSON("/api/pairing/claim", { pin: pin || "" }, { timeoutMs: 15000 });
    const token = (result && result.token) || "";
    if (token) {
      storeToken(token);
    }
    return token;
  }

  // A clock with no PIN set and no window open cannot be paired by anybody:
  // there is no PIN to type. Asking for one is a dead end that also hides the
  // settings page behind a modal, so say what actually unblocks it instead.
  function buildNoPinPrompt() {
    const backdrop = document.createElement("div");
    backdrop.className = "pairing-backdrop";
    backdrop.innerHTML = `
      <section class="pairing-card" role="dialog" aria-modal="true" aria-labelledby="pairingTitle">
        <p class="eyebrow">Controller pairing</p>
        <h2 id="pairingTitle">Pairing is closed</h2>
        <p class="pairing-lead">
          No PIN has been set on this clock, and the window for setting one has
          closed. Restart the hall clock to reopen it for 15 minutes, then set a
          PIN from this page.
        </p>
        <div class="pairing-actions">
          <button type="button" class="action action-primary pairing-recheck">Check again</button>
        </div>
      </section>`;
    return backdrop;
  }

  function showNoPinPrompt() {
    return new Promise((resolve) => {
      const backdrop = buildNoPinPrompt();
      document.body.appendChild(backdrop);
      backdrop.querySelector(".pairing-recheck").addEventListener("click", async () => {
        backdrop.remove();
        resolve(await ensurePaired());
      });
    });
  }

  function buildPinPrompt() {
    const backdrop = document.createElement("div");
    backdrop.className = "pairing-backdrop";
    backdrop.innerHTML = `
      <section class="pairing-card" role="dialog" aria-modal="true" aria-labelledby="pairingTitle">
        <p class="eyebrow">Controller pairing</p>
        <h2 id="pairingTitle">Enter the hall PIN</h2>
        <p class="notice pairing-error hidden" role="alert"></p>
        <input class="pairing-input" type="password" inputmode="numeric" autocomplete="current-password"
               maxlength="32" placeholder="••••" aria-label="Hall PIN">
        <div class="pairing-actions">
          <button type="button" class="action action-primary pairing-submit">Pair this device</button>
        </div>
      </section>`;
    return backdrop;
  }

  // showPinPrompt resolves with a token once the operator enters the PIN. It
  // never rejects: a page that cannot pair should sit on the prompt rather than
  // fall through to a controller whose every button would fail with a 401.
  // When the PIN's length is known (from /api/pairing), the final digit claims
  // by itself; the button stays for the cases where it is not.
  function showPinPrompt(pinLength) {
    return new Promise((resolve) => {
      const backdrop = buildPinPrompt();
      const input = backdrop.querySelector(".pairing-input");
      const submit = backdrop.querySelector(".pairing-submit");
      const errorEl = backdrop.querySelector(".pairing-error");
      document.body.appendChild(backdrop);
      input.focus();

      function showError(message) {
        errorEl.textContent = message;
        errorEl.classList.toggle("hidden", !message);
      }

      let attempting = false;
      async function attempt() {
        if (attempting) return;
        if (!input.value) {
          showError("Type the PIN first.");
          return;
        }
        attempting = true;
        submit.disabled = true;
        try {
          const token = await claimPairing(input.value);
          if (!token) throw new Error("no token returned");
          backdrop.remove();
          resolve(token);
          return;
        } catch (error) {
          input.value = "";
          if (error.status === 429) {
            // Repeated wrong PINs lock guessing for a few minutes.
            showError("Too many wrong tries. Wait five minutes, then try again.");
          } else if (error.status === 401) {
            showError("That PIN was not right.");
          } else if (error.status === 403) {
            showError("No PIN has been set on this clock yet. Set one in Setup from a phone that is already paired.");
          } else {
            console.error(error);
            showError("Could not reach the clock. Check the wifi and try again.");
          }
        } finally {
          attempting = false;
          submit.disabled = false;
          input.focus();
        }
      }

      input.addEventListener("keydown", (event) => {
        if (event.key === "Enter") {
          event.preventDefault();
          attempt();
        }
      });
      // Claim on the last digit. A failed try clears the field (see attempt),
      // so retyping re-arms this without ever firing on a partial PIN — and
      // the lockout still backstops deliberate guessing.
      if (Number.isInteger(pinLength) && pinLength > 0) {
        input.addEventListener("input", () => {
          if ([...input.value].length === pinLength) attempt();
        });
      }
      submit.addEventListener("click", attempt);
    });
  }

  // ensurePaired resolves with a control token, prompting for the hall PIN when
  // this browser has none and no open window can pair it silently.
  async function ensurePaired() {
    const existing = getToken();
    if (existing && (await tokenWorks(existing))) {
      return existing;
    }
    if (existing) {
      clearToken();
    }
    let pinLength = 0;
    try {
      const status = await pairingStatus();
      if (status.pairingOpen) {
        const token = await claimPairing("");
        if (token) return token;
      }
      if (!status.pinConfigured) {
        return showNoPinPrompt();
      }
      pinLength = status.pinLength;
    } catch (error) {
      // Fall through to the prompt: an unreachable status endpoint should not
      // stop somebody who knows the PIN from typing it.
      console.error(error);
    }
    return showPinPrompt(pinLength);
  }

  // repairPairing is what a page calls after a 401: the token it held is dead
  // and postJSON has already dropped it, so ask for the PIN in place rather
  // than leave every later write going out with no token until a reload.
  // Every page used to carry its own copy of this. One run at a time, shared
  // by all callers: a few commands failing together must raise one PIN
  // prompt, not a stack of them. Resolves true once paired, false if pairing
  // failed; never rejects, so a caller only has to decide what to show.
  let repairing = null;
  function repairPairing() {
    if (!repairing) {
      repairing = ensurePaired()
        .then(() => true, (error) => {
          console.error(error);
          return false;
        })
        .finally(() => {
          repairing = null;
        });
    }
    return repairing;
  }

  // showPairingPIN reads the PIN in force. Needs a token, so only an
  // already-paired controller can see it.
  async function showPairingPIN() {
    const response = await fetch("/api/pairing/pin", {
      headers: { "X-Wall-Clock-Token": getToken() },
    });
    if (!response.ok) throw new Error("could not read the PIN");
    return response.json();
  }

  // setPairingPIN changes the hall PIN. Setup calls this; it needs an existing
  // token, so only an already-paired controller can rotate it.
  async function setPairingPIN(pin) {
    return postJSON("/api/pairing/pin", { pin }, { timeoutMs: 15000 });
  }

  // A backgrounded phone cannot hold a connection open: iOS and Android suspend
  // a hidden tab's timers and sockets, so locking the screen always drops the
  // stream. That part is not ours to fix. The recovery is:
  //
  //   - EventSource retries by itself only while it is CONNECTING. Once it
  //     reaches CLOSED it is finished, and this used to sit on "reconnecting"
  //     forever until somebody reloaded the page.
  //   - Coming back to the foreground should reconnect immediately rather than
  //     wait out the browser's retry backoff.
  //   - A blip should not flash the banner. Normal resumes settle in about a
  //     second, and a notice that cries wolf is one people learn to ignore.
  const OFFLINE_GRACE_MS = 2000;
  const RECONNECT_MIN_MS = 1000;
  // Doubles up to this cap while the server stays away, so a Pi mid-update is
  // not hammered at 1Hz by every paired phone in the hall.
  const RECONNECT_MAX_MS = 5000;
  // The server pushes state every 250ms, so a stream that has been quiet for
  // this long is dead no matter what readyState claims.
  const STALE_MS = 1500;
  // A screen that never goes dark -- the Android wrapper keeps it on, the hall
  // display never sleeps -- never fires the visibility events that check
  // STALE_MS, so a stream that dies without an error (a wifi handover, a
  // router dropping the idle socket) used to freeze the countdown with no
  // offline notice at all. A watchdog checks for silence on a timer instead.
  // Its limit is twenty missed pushes rather than six: it runs whether or not
  // anybody is looking, so it must never tear down a stream that is only slow
  // for a moment.
  const WATCHDOG_EVERY_MS = 1000;
  const WATCHDOG_SILENT_MS = 5000;

  function subscribe(onState, onConnection) {
    let source = null;
    let offlineTimer = null;
    let reconnectTimer = null;
    let watchdogTimer = null;
    let reconnectDelay = RECONNECT_MIN_MS;
    let lastConnectAt = 0;
    let lastEventAt = 0;
    let stopped = false;

    function report(online) {
      if (!onConnection) return;
      if (online) {
        clearTimeout(offlineTimer);
        offlineTimer = null;
        onConnection(true);
        return;
      }
      if (offlineTimer) return;
      offlineTimer = setTimeout(() => {
        offlineTimer = null;
        onConnection(false);
      }, OFFLINE_GRACE_MS);
    }

    function connect() {
      if (stopped) return;
      lastConnectAt = Date.now();
      // Counts as activity: a stream that never opens is caught by its own
      // error events, and one that opens gets the initial snapshot at once.
      lastEventAt = lastConnectAt;
      if (source) source.close();
      source = new EventSource("/events");
      source.addEventListener("open", () => {
        reconnectDelay = RECONNECT_MIN_MS;
        report(true);
      });
      source.addEventListener("state", (event) => {
        lastEventAt = Date.now();
        onState(JSON.parse(event.data));
      });
      source.addEventListener("error", () => {
        report(false);
        // CLOSED means the browser has given up; nothing will retry but us.
        if (source && source.readyState === EventSource.CLOSED) {
          scheduleReconnect();
        }
      });
    }

    function scheduleReconnect() {
      if (stopped || reconnectTimer) return;
      reconnectTimer = setTimeout(() => {
        reconnectTimer = null;
        connect();
      }, reconnectDelay);
      reconnectDelay = Math.min(reconnectDelay * 2, RECONNECT_MAX_MS);
    }

    // Rebuilding the stream the moment the operator looks at their phone again
    // beats waiting for a socket the OS already killed to notice it is dead.
    // readyState is no help here: a killed socket can report OPEN until the
    // browser finally notices, which is exactly the frozen-clock case this
    // exists to fix — so trust silence, not readyState. A healthy stream has
    // always spoken within the last STALE_MS.
    function resume() {
      if (stopped) return;
      const fresh = source && source.readyState === EventSource.OPEN && Date.now() - lastEventAt < STALE_MS;
      if (fresh || Date.now() - lastConnectAt < RECONNECT_MIN_MS) return;
      reconnect();
    }

    // resume's question, asked on a timer -- but judged on silence alone.
    // resume also rebuilds a stream that is still CONNECTING, so a returning
    // operator does not wait out the browser's retry backoff. Asked every
    // second, that would rebuild a hanging connection every second, and every
    // phone in the hall would hammer a Pi mid-update at 1Hz. connect() counts
    // as activity, so this replaces a stream at most once per
    // WATCHDOG_SILENT_MS, whatever state it is stuck in.
    function watchdog() {
      if (stopped || document.hidden) return;
      if (Date.now() - lastEventAt < WATCHDOG_SILENT_MS) return;
      reconnect();
    }

    function reconnect() {
      // A stream that died without an error never reported itself offline.
      // Start the grace period now: a reconnect that opens inside it clears
      // it unseen, and one that hangs gets the banner it deserves.
      report(false);
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
      connect();
    }

    function onVisible() {
      if (!document.hidden) resume();
    }

    document.addEventListener("visibilitychange", onVisible);
    window.addEventListener("pageshow", resume);
    window.addEventListener("online", resume);
    // One interval for the life of the subscription, never one per connect(),
    // so reconnects cannot stack watchdogs. A hidden page is left alone: the
    // OS has suspended its socket on purpose, and onVisible deals with it on
    // the way back.
    watchdogTimer = setInterval(watchdog, WATCHDOG_EVERY_MS);

    connect();
    return {
      close() {
        stopped = true;
        document.removeEventListener("visibilitychange", onVisible);
        window.removeEventListener("pageshow", resume);
        window.removeEventListener("online", resume);
        clearTimeout(offlineTimer);
        clearTimeout(reconnectTimer);
        clearInterval(watchdogTimer);
        if (source) source.close();
      },
    };
  }

  function formatTime(seconds) {
    const negative = seconds < 0;
    const total = Math.abs(seconds);
    const minutes = Math.floor(total / 60);
    const secs = total % 60;
    return `${negative ? "+" : ""}${String(minutes).padStart(2, "0")}:${String(secs).padStart(2, "0")}`;
  }

  function formatClock(isoDate) {
    const date = isoDate ? new Date(isoDate) : new Date();
    return date.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  }

  function formatStartTime(value) {
    const match = /^(\d{1,2}):(\d{2})$/.exec(String(value || ""));
    if (!match) return value || "";
    const date = new Date();
    date.setHours(Number(match[1]), Number(match[2]), 0, 0);
    return date.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  }

  function statusLabel(status) {
    if (status === "running") return "Running";
    if (status === "paused") return "Paused";
    return "Idle";
  }


  window.WallClock = {
    getToken,
    clearToken,
    ensurePaired,
    repairPairing,
    pairingStatus,
    showPairingPIN,
    setPairingPIN,
    postJSON,
    subscribe,
    formatTime,
    formatClock,
    formatStartTime,
    statusLabel,
  };
})();
