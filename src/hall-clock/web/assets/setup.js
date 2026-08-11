(function () {
  const tokenWarning = document.getElementById("tokenWarning");
  const form = document.getElementById("setupForm");
  const deviceNameInput = document.getElementById("deviceNameInput");
  const advertisedBaseUrlInput = document.getElementById("advertisedBaseUrlInput");
  const meetingTypeInput = document.getElementById("meetingTypeInput");
  const prestartMinutesInput = document.getElementById("prestartMinutesInput");
  const midweekUrlInput = document.getElementById("midweekUrlInput");
  const autoImportInput = document.getElementById("autoImportInput");
  const autoImportStatus = document.getElementById("autoImportStatus");
  const scheduleModeText = document.getElementById("scheduleModeText");
  const todayStrip = document.getElementById("todayStrip");
  const startsList = document.getElementById("startsList");
  const partsList = document.getElementById("partsList");
  const saveStatus = document.getElementById("saveStatus");
  const tabButtons = Array.from(document.querySelectorAll("[data-settings-tab]"));
  const tabPanels = Array.from(document.querySelectorAll("[data-settings-panel]"));
  let parts = [];
  let meetingStarts = [];
  let defaultScheduleLanguage = "en";
  const dayLabels = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

  async function load() {
    const config = await fetchConfig();
    deviceNameInput.value = config.deviceName || "Hall Clock";
    advertisedBaseUrlInput.value = config.advertisedBaseUrl || "";
    meetingTypeInput.value = config.meetingType || "midweek";
    renderMeetingType(config.meetingType || "midweek");
    prestartMinutesInput.value = Math.round((config.prestartSeconds || 300) / 60);
    midweekUrlInput.value = config.midweekUrl || "";
    defaultScheduleLanguage = normalizeLanguage(config.midweekLanguage) || languageFromURL(config.midweekUrl) || "en";
    autoImportInput.checked = Boolean(config.autoImportMidweek);
    renderAutoStatus(config);
    meetingStarts = config.meetingStarts || defaultMeetingStarts(config.meetingStartTime || "19:30");
    parts = config.schedule || [];
    renderStarts();
    renderParts();
  }

  async function fetchConfig() {
    const response = await fetch("/api/config");
    return response.json();
  }

  async function refreshMeetingType() {
    const config = await fetchConfig();
    meetingTypeInput.value = config.meetingType || "midweek";
    renderMeetingType(meetingTypeInput.value);
  }

  function renderAutoStatus(config) {
    if (config.midweekImportedWeek) {
      const match = /^(\d{4})-W(\d{2})$/.exec(config.midweekImportedWeek);
      const week = match ? `week ${Number(match[2])} of ${match[1]}` : config.midweekImportedWeek;
      autoImportStatus.textContent = `Last imported ${week}${config.midweekUrl ? ` from ${config.midweekUrl}` : ""}.`;
    } else {
      autoImportStatus.textContent = "Nothing imported yet.";
    }
  }

  // The strip starts neutral and only turns green once the clock has actually
  // told us which schedule is live — it sits at the top of the page, where a
  // guess would read as confirmed state.
  function renderMeetingType(meetingType) {
    const title = meetingType === "weekend" ? "Weekend meeting is active today" : "Midweek meeting is active today";
    if (scheduleModeText) scheduleModeText.textContent = title;
    if (todayStrip) todayStrip.classList.remove("pending");
  }

  // The save bar exists only while there is something to save or something to
  // report. Nothing is written to the clock until Save is pressed, so a bar
  // that is always there is an instruction the operator can never satisfy —
  // and one that appears is a signal worth reading.
  let dirty = false;
  let statusTimer = null;

  function setSaveStatus(message, isError, transient) {
    clearTimeout(statusTimer);
    saveStatus.textContent = message;
    saveStatus.classList.toggle("error", Boolean(isError));
    // "Saved" would otherwise keep the bar on screen long after the changes it
    // refers to are gone. Retire it — and fall back to the pending state rather
    // than to nothing, in case edits are still outstanding.
    if (transient) {
      statusTimer = setTimeout(() => {
        setSaveStatus(dirty ? "Unsaved changes" : "");
      }, 4000);
    }
  }

  function setDirty(value) {
    if (dirty === value) return;
    dirty = value;
    form.classList.toggle("is-dirty", value);
    if (value) setSaveStatus("Unsaved changes");
    else if (saveStatus.textContent === "Unsaved changes") setSaveStatus("");
  }

  // Typing counts as an edit; typing a PIN does not. The PIN field sits in this
  // form for layout only — its own button writes it, and Save settings never
  // carries it.
  ["input", "change"].forEach((type) => {
    form.addEventListener(type, (event) => {
      if (event.target.id === "pinInput") return;
      // An edit is also the answer to whatever the last complaint was.
      if (dirty && saveStatus.classList.contains("error")) setSaveStatus("Unsaved changes");
      setDirty(true);
    });
  });

  function activateTab(name, focus) {
    tabButtons.forEach((button) => {
      const active = button.dataset.settingsTab === name;
      button.classList.toggle("active", active);
      button.setAttribute("aria-selected", String(active));
      button.tabIndex = active ? 0 : -1;
      if (active && focus) button.focus();
    });
    tabPanels.forEach((panel) => {
      const active = panel.dataset.settingsPanel === name;
      panel.classList.toggle("active", active);
      panel.hidden = !active;
    });
  }

  function watchAutoImport(attempts) {
    setTimeout(async () => {
      try {
        const config = await fetchConfig();
        renderAutoStatus(config);
        renderMeetingType(config.meetingType || "midweek");
        if (config.midweekUrl) midweekUrlInput.value = config.midweekUrl;
        if (config.midweekImportedWeek) {
          parts = config.schedule || parts;
          renderParts();
        } else if (attempts > 1) {
          watchAutoImport(attempts - 1);
        }
      } catch (error) {
        console.error(error);
      }
    }, 3000);
  }

  function defaultMeetingStarts(time) {
    const starts = [1, 2, 3, 4, 5].map((day, index) => ({
      id: index + 1,
      day,
      time,
      congregation: "",
      language: "en",
      midweekUrl: "",
    }));
    starts.push({ id: starts.length + 1, day: 0, time: "10:00", congregation: "", language: "en", midweekUrl: "" });
    return starts;
  }

  function renderStarts() {
    startsList.innerHTML = "";
    meetingStarts.forEach((start, index) => {
      const row = document.createElement("div");
      row.className = "start-row";
      row.innerHTML = `
        <label class="field">
          <span>Day</span>
          <select data-start-field="day" data-index="${index}">
            ${dayLabels.map((label, day) => `<option value="${day}" ${Number(start.day) === day ? "selected" : ""}>${label}</option>`).join("")}
          </select>
        </label>
        <label class="field">
          <span>Time</span>
          <input data-start-field="time" data-index="${index}" type="time" value="${escapeAttr(start.time || "19:30")}">
        </label>
        <label class="field">
          <span>Schedule language</span>
          <select data-start-field="language" data-index="${index}">
            ${languageOptions(start.language || languageFromURL(start.midweekUrl) || defaultScheduleLanguage)}
          </select>
        </label>
        <button data-remove-start="${index}" class="row-remove" type="button" aria-label="Remove this start time">Remove</button>
      `;
      startsList.appendChild(row);
    });
  }

  function readStartsFromForm() {
    startsList.querySelectorAll("[data-start-field]").forEach((input) => {
      const index = Number(input.dataset.index);
      const field = input.dataset.startField;
      if (field === "day") meetingStarts[index].day = Number(input.value);
      if (field === "time") meetingStarts[index].time = input.value;
      if (field === "language") {
        meetingStarts[index].language = input.value;
        meetingStarts[index].congregation = "";
        meetingStarts[index].midweekUrl = "";
        meetingStarts[index].midweekImportedWeek = "";
      }
    });
  }

  function languageOptions(selected) {
    return [
      ["en", "English"],
      ["es", "Spanish"],
      ["tw", "Twi"],
    ].map(([value, label]) => `<option value="${value}" ${selected === value ? "selected" : ""}>${label}</option>`).join("");
  }

  function languageFromURL(value) {
    const match = String(value || "").match(/^https?:\/\/wol\.jw\.org\/([^/]+)\//);
    const language = match ? match[1] : "";
    if (language === "es") return "es";
    if (language === "tw") return "tw";
    return language === "en" ? "en" : "";
  }

  function normalizeLanguage(value) {
    const language = String(value || "").trim().toLowerCase();
    if (language === "en" || language === "english") return "en";
    if (language === "es" || language === "spanish") return "es";
    if (language === "tw" || language === "twi") return "tw";
    return "";
  }

  function renderParts() {
    partsList.innerHTML = "";
    parts.forEach((part, index) => {
      const row = document.createElement("div");
      row.className = "part-row";
      row.innerHTML = `
        <input
          class="part-input part-title"
          data-field="title"
          data-index="${index}"
          type="text"
          value="${escapeAttr(part.title)}"
          aria-label="Item title"
          placeholder="Item title"
        >
        <span class="part-caption" aria-hidden="true">Minutes</span>
        <input
          class="part-input part-minutes"
          data-field="minutes"
          data-index="${index}"
          type="number"
          min="1"
          max="120"
          inputmode="numeric"
          value="${Math.round(part.durationSeconds / 60)}"
          aria-label="Minutes"
        >
        <span class="part-caption" aria-hidden="true">Closing bell</span>
        <span class="part-readonly" title="Set by the WOL import: seconds of amber warning before time is up">
          ${Number(part.closingSeconds) || 0}s
        </span>
        <button data-remove="${index}" class="row-remove" type="button" aria-label="Remove ${escapeAttr(part.title)}">Remove</button>
      `;
      partsList.appendChild(row);
    });
  }

  // The closing bell is displayed but not editable: the WOL import defines it,
  // and the server restores it on every save (applyImportedClosingSeconds), so
  // there is nothing to read back out of the form for it.
  function readPartsFromForm() {
    partsList.querySelectorAll("input").forEach((input) => {
      const index = Number(input.dataset.index);
      const field = input.dataset.field;
      if (field === "title") parts[index].title = input.value;
      if (field === "minutes") parts[index].durationSeconds = Number(input.value) * 60;
    });
  }

  function escapeAttr(value) {
    return String(value || "").replace(/[&<>"']/g, (char) => ({
      "&": "&amp;",
      "<": "&lt;",
      ">": "&gt;",
      '"': "&quot;",
      "'": "&#039;",
    }[char]));
  }

  document.getElementById("addPartBtn").addEventListener("click", () => {
    readPartsFromForm();
    parts.push({ title: `Item ${parts.length + 1}`, durationSeconds: 300, closingSeconds: 120 });
    renderParts();
    setDirty(true);
  });

  tabButtons.forEach((button, index) => {
    button.addEventListener("click", () => activateTab(button.dataset.settingsTab, false));
    button.addEventListener("keydown", (event) => {
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
      event.preventDefault();
      let nextIndex = index;
      if (event.key === "ArrowLeft") nextIndex = index === 0 ? tabButtons.length - 1 : index - 1;
      if (event.key === "ArrowRight") nextIndex = index === tabButtons.length - 1 ? 0 : index + 1;
      if (event.key === "Home") nextIndex = 0;
      if (event.key === "End") nextIndex = tabButtons.length - 1;
      activateTab(tabButtons[nextIndex].dataset.settingsTab, true);
    });
  });

  document.getElementById("addStartBtn").addEventListener("click", () => {
    readStartsFromForm();
    const last = meetingStarts[meetingStarts.length - 1];
    meetingStarts.push({
      id: meetingStarts.length + 1,
      day: last ? Number(last.day) : 1,
      time: last ? last.time : "19:30",
      congregation: "",
      language: "en",
      midweekUrl: "",
    });
    renderStarts();
    setDirty(true);
  });

  startsList.addEventListener("click", (event) => {
    const index = event.target.dataset.removeStart;
    if (index === undefined) return;
    readStartsFromForm();
    meetingStarts.splice(Number(index), 1);
    if (meetingStarts.length === 0) {
      meetingStarts = defaultMeetingStarts("19:30");
    }
    renderStarts();
    setDirty(true);
  });

  document.getElementById("parseMidweekBtn").addEventListener("click", () => {
    importMidweekText(false);
  });

  document.getElementById("previewMidweekUrlBtn").addEventListener("click", async () => {
    await importMidweekUrl(false);
  });

  document.getElementById("applyMidweekUrlBtn").addEventListener("click", async () => {
    await importMidweekUrl(true);
  });

  async function importMidweekUrl(apply) {
    setSaveStatus(apply ? "Importing..." : "Fetching preview...");
    try {
      const result = await WallClock.postJSON("/api/import/midweek", {
        url: midweekUrlInput.value,
        apply,
      });
      parts = result.schedule || [];
      renderParts();
      if (apply) {
        await refreshMeetingType();
      }
      tokenWarning.classList.add("hidden");
      if (!apply) setDirty(true);
      setSaveStatus(apply ? "Imported and saved" : `Previewed ${parts.length} items`, false, true);
    } catch (error) {
      tokenWarning.classList.remove("hidden");
      setSaveStatus("Could not import URL", true);
      console.error(error);
    }
  }

  async function importMidweekText(apply) {
    setSaveStatus(apply ? "Importing pasted timings..." : "Parsing pasted timings...");
    try {
      const result = await WallClock.postJSON("/api/import/midweek-text", {
        text: document.getElementById("midweekTextInput").value,
        apply,
      });
      parts = result.schedule || [];
      renderParts();
      if (apply) {
        await refreshMeetingType();
      }
      tokenWarning.classList.add("hidden");
      if (!apply) setDirty(true);
      setSaveStatus(apply ? "Imported and saved" : `Parsed ${parts.length} items`, false, true);
    } catch (error) {
      tokenWarning.classList.remove("hidden");
      setSaveStatus("Could not parse pasted timings", true);
      console.error(error);
    }
  }

  partsList.addEventListener("click", (event) => {
    const index = event.target.dataset.remove;
    if (index === undefined) return;
    readPartsFromForm();
    parts.splice(Number(index), 1);
    if (parts.length === 0) {
      parts.push({ title: "Item 1", durationSeconds: 300, closingSeconds: 120 });
    }
    renderParts();
    setDirty(true);
  });

  // The form carries `novalidate`, so these two are checked here instead. Left
  // to the browser, a bad value in either one blocks submission outright — and
  // both can be sitting inside a collapsed <details> or a hidden tab panel,
  // where nothing can be focused and no bubble can be drawn. The operator would
  // press Save and watch nothing at all happen, on every press.
  function fieldProblem() {
    const url = advertisedBaseUrlInput.value.trim();
    if (url && !/^https?:\/\/[^\s/]+/i.test(url)) {
      return { input: advertisedBaseUrlInput, message: "Controller URL must start with http:// or https://" };
    }
    const minutes = Number(prestartMinutesInput.value);
    if (!Number.isInteger(minutes) || minutes < 1 || minutes > 30) {
      return { input: prestartMinutesInput, message: "Pre-meeting countdown must be a whole number of minutes, 1 to 30." };
    }
    return null;
  }

  // Saying which field is wrong is no use if the field is out of sight: open
  // its tab and its disclosure, then put the cursor in it.
  function revealField(input) {
    const panel = input.closest("[data-settings-panel]");
    if (panel) activateTab(panel.dataset.settingsPanel, false);
    let details = input.closest("details");
    while (details) {
      details.open = true;
      details = details.parentElement ? details.parentElement.closest("details") : null;
    }
    input.focus();
  }

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    readPartsFromForm();
    readStartsFromForm();
    const problem = fieldProblem();
    if (problem) {
      revealField(problem.input);
      setSaveStatus(problem.message, true);
      return;
    }
    setSaveStatus("Saving...");
    try {
      await WallClock.postJSON("/api/config", {
        deviceName: deviceNameInput.value,
        advertisedBaseUrl: advertisedBaseUrlInput.value,
        meetingType: meetingTypeInput.value,
        meetingStartTime: meetingStarts[0]?.time || "19:30",
        meetingStarts,
        prestartSeconds: Number(prestartMinutesInput.value || 5) * 60,
        midweekUrl: midweekUrlInput.value,
        autoImportMidweek: autoImportInput.checked,
        schedule: parts,
      });
      // The server owns the closing bell and may re-derive it, so redraw from
      // the saved midweek program rather than leave a stale number on screen.
      // The POST response carries the runtime state (on a weekend, the weekend
      // template), which must never be loaded into this editor.
      const savedConfig = await fetchConfig();
      parts = savedConfig.schedule || parts;
      renderParts();
      setDirty(false);
      setSaveStatus("Saved", false, true);
      tokenWarning.classList.add("hidden");
      if (autoImportInput.checked) {
        watchAutoImport(15);
      }
    } catch (error) {
      tokenWarning.classList.remove("hidden");
      setSaveStatus("Could not save", true);
      console.error(error);
    }
  });

  // Controller PIN. An unset PIN is the one setting worth nagging about: until
  // it exists the clock reopens a pairing window on every boot, so anyone on
  // the wifi can take it over.
  const pinInput = document.getElementById("pinInput");
  const setPinBtn = document.getElementById("setPinBtn");
  const pinMessage = document.getElementById("pinMessage");
  const pinStatus = document.getElementById("pinStatus");
  const pinCurrentRow = document.getElementById("pinCurrentRow");
  const pinCurrentValue = document.getElementById("pinCurrentValue");
  const pinRevealBtn = document.getElementById("pinRevealBtn");
  let currentPin = "";
  let pinRevealed = false;

  function renderCurrentPin() {
    pinCurrentRow.classList.toggle("hidden", !currentPin);
    if (!currentPin) return;
    pinCurrentValue.textContent = pinRevealed ? currentPin : "•".repeat(currentPin.length);
    pinCurrentValue.classList.toggle("revealed", pinRevealed);
    pinRevealBtn.textContent = pinRevealed ? "Hide" : "Show";
  }

  async function refreshPinStatus() {
    try {
      const status = await WallClock.pairingStatus();
      const missing = !status.pinConfigured;
      pinStatus.textContent = missing
        ? "No PIN is set yet, so any phone on the network can pair with this clock. Set one below."
        : "";
      pinStatus.classList.toggle("hidden", !missing);

      currentPin = "";
      if (!missing) {
        const current = await WallClock.showPairingPIN();
        currentPin = current.pin || "";
      }
      // Never leave a PIN on screen across a refresh — /setup can be open on a
      // laptop plugged into the projector.
      pinRevealed = false;
      renderCurrentPin();
    } catch (error) {
      console.error(error);
    }
  }

  pinRevealBtn.addEventListener("click", () => {
    pinRevealed = !pinRevealed;
    renderCurrentPin();
  });

  setPinBtn.addEventListener("click", async () => {
    if (!pinInput.value) {
      pinMessage.textContent = "Type a PIN first.";
      return;
    }
    setPinBtn.disabled = true;
    pinMessage.textContent = "Saving PIN...";
    try {
      await WallClock.setPairingPIN(pinInput.value);
      pinInput.value = "";
      pinMessage.textContent = "PIN saved. Phones already paired keep working.";
      await refreshPinStatus();
    } catch (error) {
      console.error(error);
      pinMessage.textContent = error.message || "Could not save the PIN.";
    } finally {
      setPinBtn.disabled = false;
    }
  });

  // The PIN field lives inside the settings form, so the phone keyboard's Go key
  // would otherwise submit the whole form — which posts every setting except the
  // PIN, then reports "Saved". An elder would walk away from an unprotected
  // clock believing they had just locked it.
  pinInput.addEventListener("keydown", (event) => {
    if (event.key !== "Enter") return;
    event.preventDefault();
    setPinBtn.click();
  });

  (async () => {
    // Setup pairs the same way control does. It matters more here: a browser
    // that never visited /control (or lost its per-origin token to a hostname
    // change) would otherwise have every save and update fail with a token
    // error, and setup's writes are the persistent ones.
    await WallClock.ensurePaired();
    refreshPinStatus();
    load().catch((error) => {
      console.error(error);
      setSaveStatus("Could not load settings — check the connection and reload.", true);
      // Leave the strip neutral and say so: the schedule was never read, and
      // guessing one here is worse than admitting the clock did not answer.
      if (scheduleModeText) scheduleModeText.textContent = "Could not read today’s schedule";
    });
  })();
})();
