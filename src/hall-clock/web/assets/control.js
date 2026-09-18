(function () {
  const tokenWarning = document.getElementById("tokenWarning");
  const offlineNotice = document.getElementById("offlineNotice");
  const commandNotice = document.getElementById("commandNotice");
  const meetingType = document.getElementById("meetingType");
  const hallName = document.getElementById("hallName");
  const timeValue = document.getElementById("timeValue");
  const partPickerBtn = document.getElementById("partPickerBtn");
  const partPickerList = document.getElementById("partPickerList");
  const adhocPartBtn = document.getElementById("adhocPartBtn");
  const adhocPartPanel = document.getElementById("adhocPartPanel");
  const adhocPartTitleInput = document.getElementById("adhocPartTitleInput");
  const adhocPartAfterSelect = document.getElementById("adhocPartAfterSelect");
  const adhocPartMinutesInput = document.getElementById("adhocPartMinutesInput");
  const cancelAdhocPartBtn = document.getElementById("cancelAdhocPartBtn");
  const startBtn = document.getElementById("startBtn");
  const nextBtn = document.getElementById("nextBtn");
  const endBtn = document.getElementById("endBtn");
  const resetBtn = document.getElementById("resetBtn");
  const nowPartTitle = document.getElementById("nowPartTitle");
  const meetingOvertime = document.getElementById("meetingOvertime");
  const nextPart = document.getElementById("nextPart");
  const coToggle = document.getElementById("coToggle");
  const coHint = document.getElementById("coHint");
  const languageSelect = document.getElementById("languageSelect");
  const languageStatus = document.getElementById("languageStatus");
  let scheduleKey = "";
  let nextArmTimeout = null;
  let startArmTimeout = null;
  let endArmTimeout = null;
  let resetArmTimeout = null;
  let partArmTimeout = null;
  let coArmTimeout = null;
  let latestStatus = "idle";
  let latestState = null;
  let timerCommandPending = false;
  let advancePending = false;
  let languageCommandPending = false;
  let lastAppliedLanguage = "en";
  let commandNoticeTimeout = null;
  // How long a confirmation stays armed. Three seconds was long enough to miss:
  // an operator who taps, glances at the platform, then taps again has re-armed
  // rather than confirmed, twice over, which reads as a dead button. The armed
  // state also drains a bar (see .armed::after) so the window is visible rather
  // than something you find out about by failing it.
  const ARM_TIMEOUT_MS = 5000;

  // A 401 means the token this browser held is dead — shared.js has already
  // dropped it. Raise the PIN prompt in place rather than a banner: the banner
  // used to point at /pair, which cannot pair anything and loops the operator
  // straight back here still unpaired, mid-meeting. The banner is only for a
  // pairing that failed outright.
  async function repairPairing() {
    const paired = await WallClock.repairPairing();
    tokenWarning.classList.toggle("hidden", paired);
  }

  // Most commands fail silently into the console; the operator deserves at
  // least a transient on-screen cue that a tap went nowhere.
  function flashCommandNotice() {
    commandNotice.classList.remove("hidden");
    clearTimeout(commandNoticeTimeout);
    commandNoticeTimeout = setTimeout(() => commandNotice.classList.add("hidden"), 4000);
  }

  function render(state) {
    latestState = state;
    latestStatus = state.status;
    document.title = `${state.deviceName || "Hall Clock"} Control`;
    // The title alone is not enough: the Android shell runs the page full
    // screen with no title bar, so there the name is invisible.
    //
    // An unnamed hall shows nothing rather than the generic default. The server
    // coerces a blank name to "Hall Clock" (server.go, handlers.go), so "unset"
    // reaches us as that string, not "" — and a label identical in every hall
    // looks like identity while answering the question wrongly, which is worse
    // than staying quiet and letting the operator ask. Naming both Pis on the
    // setup page is what makes this pill worth anything.
    const named = state.deviceName && state.deviceName !== "Hall Clock";
    hallName.textContent = named ? state.deviceName : "";
    hallName.classList.toggle("hidden", !named);
    const meetingLabel = state.meetingType === "weekend" ? "Weekend meeting" : "Midweek meeting";
    meetingType.textContent = state.circuitOverseer ? `${meetingLabel} · CO visit` : meetingLabel;
    meetingType.classList.toggle("co-active", Boolean(state.circuitOverseer));
    const prestart = state.prestartActive;
    timeValue.textContent = WallClock.formatTime(prestart ? state.prestartRemainingSeconds : state.remainingSeconds);

    const timing = state.status === "running" || state.status === "paused";
    timeValue.classList.toggle("warning", timing && !prestart && state.remainingSeconds <= state.closingSeconds && state.remainingSeconds >= 0);
    timeValue.classList.toggle("overtime", timing && !prestart && state.remainingSeconds < 0);
    startBtn.disabled = timerCommandPending || advancePending;
    startBtn.dataset.status = state.status;
    // Start only starts. Operators here never pause a part — they let it run
    // over and move on — so while the clock is running the button has no job
    // and its row collapses (see the CSS): Next moves up into the space, and a
    // stray second tap lands on a control that arms before it acts.
    // "Resume" survives for a paused state reached through the API, which the
    // UI can no longer produce but must not strand anybody in.
    startBtn.classList.toggle("slot-hidden", state.status === "running");
    // Start only needs confirming while the countdown runs (see its click
    // handler); once the countdown ends, or the clock moves, one tap is right.
    if (!startNeedsConfirm() && startBtn.classList.contains("armed")) {
      disarmStart();
    }
    if (!timerCommandPending && !startBtn.classList.contains("armed")) {
      startBtn.textContent = state.status === "paused" ? "Resume" : "Start";
    }
    if (state.status === "idle") {
      disarmPartButtons();
      // No live timer left to protect, so the confirmation is moot.
      disarmNext();
      disarmReset();
    }
    coToggle.setAttribute("aria-checked", state.circuitOverseer ? "true" : "false");
    const appliedLanguage = state.midweekLanguage || languageFromSchedule(state.schedule) || "en";
    lastAppliedLanguage = appliedLanguage;
    if (languageSelect && !languageCommandPending) {
      languageSelect.value = appliedLanguage;
    }
    if (languageSelect) {
      languageSelect.disabled = languageCommandPending || state.status !== "idle";
    }
    if (languageStatus && !languageCommandPending && !languageStatus.classList.contains("error")) {
      languageStatus.textContent = state.status === "idle"
        ? ""
        : "Language can be changed while the timer is idle.";
      languageStatus.classList.toggle("hidden", languageStatus.textContent === "");
    }
    // CO mode reshapes the schedule, so it's editable only while idle.
    coToggle.disabled = state.status !== "idle";
    coToggle.title = coToggle.disabled
      ? "Circuit overseer visit — reset the timer to idle to change"
      : "Circuit overseer visit schedule";
    // A confirmation armed on a switch that just locked, or whose flip already
    // happened from another phone, would apply on a later stray tap.
    if ((coToggle.disabled || state.circuitOverseer) && coToggle.classList.contains("armed")) {
      disarmCo();
    }
    if (coToggle.classList.contains("armed")) {
      // The armed hint is the confirmation prompt; a broadcast must not
      // overwrite it mid-decision.
    } else if (state.circuitOverseer && state.circuitOverseerExpiresAt) {
      coHint.textContent = `On — turns off automatically around ${WallClock.formatClock(state.circuitOverseerExpiresAt)}`;
      coHint.classList.remove("hidden");
    } else if (coToggle.disabled) {
      // The title-attribute explanation never shows on a phone; say it on-screen.
      coHint.textContent = "Available while the timer is idle.";
      coHint.classList.remove("hidden");
    } else {
      coHint.textContent = "";
      coHint.classList.add("hidden");
    }

    const schedule = state.schedule || [];
    const index = schedule.findIndex((talk) => talk.id === state.currentTalkId);
    // The clock's time belongs to this title; naming it here saves the operator
    // a glance down at the picker card to learn what is being timed. During the
    // pre-meeting countdown the big number is not timing an item at all, so say
    // what it *is* counting down to -- that label used to live in the item
    // counter above, which is gone.
    const nowTitle = prestart
      ? state.prestartLabel || "Meeting starts soon"
      : index >= 0
        ? schedule[index].title
        : "";
    nowPartTitle.textContent = nowTitle;
    nowPartTitle.classList.toggle("hidden", nowTitle === "");
    const next = index >= 0 ? schedule[index + 1] : undefined;
    // During the pre-meeting countdown nothing is on the clock, so what comes
    // next is the staged item itself rather than the one after it. Naming the
    // second item while the first has not started is wrong at the one moment
    // the operator is checking they are set up right -- and it reads as though
    // the first item has already been missed.
    //
    // It reads "First up", not "Next": the Next part button sits right below,
    // and during the countdown it would skip this very item. "Next: X" above
    // it reads as "this button goes to X", which is the opposite of true.
    //
    // Only the label moves. `next` still means "the item Start would advance
    // to", which is what decides whether Next and End meeting are on screen; a
    // one-item schedule must not grow a Next button just because the countdown
    // is running.
    const upNext = prestart ? (index >= 0 ? schedule[index] : schedule[0]) : next;
    nextPart.textContent = upNext
      ? `${prestart ? "First up" : "Next"}: ${upNext.title}`
      : prestart
        ? ""
        : "Last item of the meeting";

    // How far the whole meeting is behind, not just this part. Absent until it
    // exists: a meeting running to time should show nothing at all.
    const behind = state.meetingOvertimeSeconds || 0;
    const liveOvertime = Math.max(0, -(state.remainingSeconds || 0));
    const showMeetingOvertime = behind > 0 && behind !== liveOvertime;
    meetingOvertime.textContent = showMeetingOvertime ? `Meeting ${WallClock.formatTime(behind)} behind` : "";
    meetingOvertime.classList.toggle("hidden", !showMeetingOvertime);

    // End meeting belongs to the end of the meeting, not to all of it: it
    // appears on the final item rather than sitting under the operator's thumb
    // for the whole programme. Not gated on the clock reaching zero -- a last
    // item that finishes early would leave no button on screen at all, since
    // Start is hidden while running and Next has nothing left to advance to.
    //
    // Only there. Showing it in the idle gaps between parts as well put a way
    // to end the meeting under the operator's thumb all evening, and next to
    // the countdown after a false start. A meeting left idle closes itself: the
    // server lets it go after half an hour, or when the next one's countdown
    // opens.
    const onFinalItem = !next && !prestart && timing;
    endBtn.classList.toggle("slot-hidden", !onFinalItem);
    if (!onFinalItem && endBtn.classList.contains("armed")) {
      disarmEnd();
    }

    // Nothing follows the last item, so Next goes away entirely rather than
    // greying out under a "Meeting complete" label -- a control that looks
    // tappable and does nothing is worse than no control. End meeting takes its
    // place. Leave an armed label alone: overwriting it mid-confirmation would
    // drop the operator's first tap on the next state broadcast.
    const atEnd = !next;
    const busy = timerCommandPending || advancePending;
    nextBtn.classList.toggle("slot-hidden", atEnd);
    nextBtn.disabled = busy || atEnd;
    endBtn.disabled = busy;

    // Reset only exists while a part is actually on the clock: resetting an
    // idle part puts it back where it already is, so the button would be a
    // no-op wearing the look of an action. Sits below Next rather than in the
    // slot Start vacates, so the stray second tap after Start still lands on
    // Next -- the control the operator was aiming at.
    resetBtn.classList.toggle("slot-hidden", !timing);
    resetBtn.disabled = busy;
    if ((!timing || busy) && resetBtn.classList.contains("armed")) {
      disarmReset();
    }
    if (!nextBtn.classList.contains("armed")) {
      nextBtn.textContent = "Next part";
    }
    // A confirmation left armed on a button that just went away would fire the
    // next time it reappears.
    if (nextBtn.disabled && nextBtn.classList.contains("armed")) {
      disarmNext();
    }

    renderPartPicker(schedule, state.currentTalkId);

  }

  async function command(path, body, options) {
    try {
      const state = await WallClock.postJSON(path, body);
      if (state && state.status) {
        render(state);
      }
      return state;
    } catch (error) {
      // Only an auth failure means the token is wrong. The server also refuses
      // legitimate requests -- advancing past the last part, changing CO mode
      // mid-meeting -- and telling the operator to re-pair for those sends them
      // chasing a problem that does not exist.
      if (error.status === 401) {
        repairPairing();
      } else if (error.status === 403) {
        tokenWarning.classList.remove("hidden");
      } else if (error.status === 412 && options && options.quietConflict) {
        // 412: the clock had already moved on from the item this tap named,
        // so the latest state is the answer. Any other refusal, 409 included,
        // is a real one and still gets the notice.
        if (latestState) render(latestState);
      } else {
        flashCommandNotice();
      }
      console.error(error);
      return null;
    }
  }

  // A confirmed Next leaves the clock idle on the following item, where Next
  // takes a single tap -- so a nervous second tap just after the reply skipped
  // a whole part. Stay busy for a moment once the clock has moved, so that tap
  // lands on a disabled button. It holds Start too: Start has just come back
  // into the slot Next was in, and a stray tap there would start a part.
  const ADVANCE_COOLDOWN_MS = 1000;

  // The body a Next tap sends: the item on the operator's screen when they
  // tapped. Left out until a state has arrived, which is the old behaviour.
  function advanceFrom() {
    const talkId = latestState && latestState.currentTalkId;
    return Number.isInteger(talkId) ? { fromTalkId: talkId } : undefined;
  }

  // Whether the latest state from the stream has the clock on some other item
  // than the one a failed tap named: the tap landed, or somebody else moved it.
  function clockLeft(body) {
    return Boolean(body && latestState) &&
      Number.isInteger(body.fromTalkId) &&
      latestState.currentTalkId !== body.fromTalkId;
  }

  function finishAdvance() {
    advancePending = false;
    if (latestState) render(latestState);
  }

  // Next/Stop/End retire schedule items and are not idempotent on the server
  // (both /next and /reset advance the schedule), so a double-tap or a tap
  // queued behind a hung request must never send twice.
  //
  // Next also names the item the operator was looking at (advanceFrom). If the
  // clock has already left it -- another phone got there first, or an earlier
  // tap that timed out here landed after all -- the server answers 412 and
  // changes nothing, so a retry can never skip a part nobody meant to skip.
  // 409 is a different refusal (Next on the last item, say) and is reported
  // like any other failure.
  async function advanceCommand(path, body) {
    if (advancePending) return;
    advancePending = true;
    // A Start armed for the item being left must not carry over to the next.
    disarmStart();
    if (latestState) render(latestState);
    // Whether the clock ended up off the item this tap was about, however the
    // reply went. It decides both the failure notice and the cooldown.
    let moved = false;
    try {
      const state = await WallClock.postJSON(path, body, { timeoutMs: TIMER_COMMAND_TIMEOUT_MS });
      render(state);
      moved = true;
    } catch (error) {
      if (error.status === 401) {
        repairPairing();
      } else if (error.status === 403) {
        tokenWarning.classList.remove("hidden");
      } else if (error.status === 412 || clockLeft(body)) {
        // Already handled. The notice says "try again", and once the clock is
        // off the item there is nothing to retry: the server says (412) it had
        // already moved on, or the tap timed out here but the stream shows it
        // landed. The latest state rendered below is the answer.
        moved = true;
      } else {
        flashCommandNotice();
      }
      console.error(error);
    } finally {
      if (moved) {
        if (latestState) render(latestState);
        setTimeout(finishAdvance, ADVANCE_COOLDOWN_MS);
      } else {
        finishAdvance();
      }
    }
  }

  function openAdhocPartPanel() {
    closePartPicker();
    adhocPartTitleInput.value = "";
    adhocPartMinutesInput.value = "5";
    // The operator says where the item goes before it exists — the old flow
    // dropped it after the current item and left them shuffling it with
    // arrow buttons afterwards. Defaults to after the current item, which
    // is where an announcement usually lands.
    const schedule = (latestState && latestState.schedule) || [];
    const currentId = latestState && latestState.currentTalkId;
    adhocPartAfterSelect.innerHTML = "";
    const startOption = document.createElement("option");
    startOption.value = "0";
    startOption.textContent = "Start of meeting";
    adhocPartAfterSelect.appendChild(startOption);
    schedule.forEach((talk, index) => {
      const option = document.createElement("option");
      option.value = String(talk.id);
      option.textContent = `${index + 1}. ${talk.title}`;
      option.selected = talk.id === currentId;
      adhocPartAfterSelect.appendChild(option);
    });
    adhocPartPanel.classList.remove("hidden");
    adhocPartBtn.setAttribute("aria-expanded", "true");
    // No auto-focus: on a phone that throws the keyboard over half the
    // controls, and the defaults ("Additional item", 5 min) mean many
    // operators only want to tap Add. The keyboard should arrive when the
    // title is tapped, not before.
  }

  function closeAdhocPartPanel() {
    adhocPartPanel.classList.add("hidden");
    adhocPartBtn.setAttribute("aria-expanded", "false");
  }

  // A person is standing at the front waiting for this, so fail fast rather than
  // hang: the click handler only clears its pending flag once this settles, and
  // render() keeps the button disabled while it is set.
  const TIMER_COMMAND_TIMEOUT_MS = 8000;

  async function timerCommand(path) {
    try {
      const state = await WallClock.postJSON(path, undefined, { timeoutMs: TIMER_COMMAND_TIMEOUT_MS });
      timerCommandPending = false;
      render(state);
    } catch (error) {
      // Only an auth failure means the token is wrong. A timeout or a refusal
      // must not send the operator off to re-pair the device.
      if (error.status === 401) {
        repairPairing();
      } else if (error.status === 403) {
        tokenWarning.classList.remove("hidden");
      }
      console.error(error);
      throw error;
    }
  }

  function renderPartPicker(schedule, currentId) {
    const key = JSON.stringify(schedule.map((talk) => [talk.id, talk.title, talk.durationSeconds, talk.temporary ? 1 : 0]));
    if (key !== scheduleKey) {
      scheduleKey = key;
      const armedTalkId = partPickerList.querySelector(".part-picker-option.armed")?.dataset.talkId;
      partPickerList.innerHTML = "";
      schedule.forEach((talk, index) => {
        const row = document.createElement("div");
        row.className = "part-picker-row";
        row.dataset.talkId = String(talk.id);

        const button = document.createElement("button");
        button.className = "part-picker-option";
        button.type = "button";
        button.dataset.talkId = String(talk.id);
        button.innerHTML = `
          <span class="part-picker-label">
            <span>${index + 1}. ${escapeHTML(talk.title)}</span>
          </span>
          <strong>${Math.round(talk.durationSeconds / 60)} min</strong>
        `;
        row.appendChild(button);

        // Only the ad-hoc additions are disposable from here; the saved
        // programme is edited in /setup.
        if (talk.temporary) {
          const removeButton = document.createElement("button");
          removeButton.className = "part-picker-remove";
          removeButton.type = "button";
          removeButton.dataset.removeTalkId = String(talk.id);
          removeButton.textContent = "Remove";
          removeButton.setAttribute("aria-label", `Remove ${talk.title}`);
          row.appendChild(removeButton);
        }

        partPickerList.appendChild(row);
      });
      if (armedTalkId !== undefined) {
        // Re-arm the pending two-tap confirmation so an SSE schedule update
        // doesn't silently swallow the operator's first tap.
        const rearmed = partPickerList.querySelector(`.part-picker-option[data-talk-id="${armedTalkId}"]`);
        if (rearmed) {
          rearmed.dataset.originalHtml = rearmed.innerHTML;
          rearmed.classList.add("armed");
          rearmed.textContent = "Confirm item";
        }
      }
    }
    // The button stays a plain "Select item" affordance rather than echoing
    // the current item: the timer card right above already names it, and the
    // opened list marks it as selected — a third copy is noise.
    // aria-current, not just the class: with the button no longer naming the
    // current item, the highlight in the list is the only thing left saying
    // which one it is, and a highlight alone says nothing out loud.
    partPickerList.querySelectorAll(".part-picker-option").forEach((button) => {
      const selected = button.dataset.talkId === String(currentId);
      button.classList.toggle("selected", selected);
      if (selected) button.setAttribute("aria-current", "true");
      else button.removeAttribute("aria-current");
    });
  }

  function languageFromSchedule(schedule) {
    const title = (schedule || []).find((talk) => talk.title)?.title || "";
    if (/[áéíóúñü¿¡]/i.test(title)) return "es";
    if (/[ɔɛŋ]/i.test(title)) return "tw";
    return "";
  }

  function escapeHTML(value) {
    return String(value).replace(/[&<>"']/g, (char) => ({
      "&": "&amp;",
      "<": "&lt;",
      ">": "&gt;",
      '"': "&quot;",
      "'": "&#039;",
    })[char]);
  }

  function escapeAttr(value) {
    return escapeHTML(value).replace(/"/g, "&quot;");
  }

  function closePartPicker() {
    partPickerList.classList.add("hidden");
    partPickerBtn.setAttribute("aria-expanded", "false");
  }

  function togglePartPicker() {
    const opening = partPickerList.classList.contains("hidden");
    if (opening) {
      closeAdhocPartPanel();
    }
    partPickerList.classList.toggle("hidden", !opening);
    partPickerBtn.setAttribute("aria-expanded", opening ? "true" : "false");
    if (opening) {
      partPickerList.querySelector(".selected")?.scrollIntoView({ block: "nearest" });
    }
  }



  function disarmCo() {
    clearTimeout(coArmTimeout);
    coArmTimeout = null;
    coToggle.classList.remove("armed");
    // The hint text is left for the next state broadcast to restore: render
    // rewrites it four times a second whenever the toggle is not armed.
  }

  function disarmEnd() {
    clearTimeout(endArmTimeout);
    endArmTimeout = null;
    endBtn.classList.remove("armed");
    endBtn.textContent = "End meeting";
  }


  function disarmNext() {
    clearTimeout(nextArmTimeout);
    nextArmTimeout = null;
    nextBtn.classList.remove("armed");
    nextBtn.textContent = "Next part";
  }

  // Whether Start takes two taps: only while the pre-meeting countdown is on
  // screen and nothing is on the clock yet.
  function startNeedsConfirm() {
    return Boolean(latestState && latestState.prestartActive) && latestStatus === "idle";
  }

  function disarmStart() {
    clearTimeout(startArmTimeout);
    startArmTimeout = null;
    if (!startBtn.classList.contains("armed")) return;
    startBtn.classList.remove("armed");
    startBtn.textContent = latestStatus === "paused" ? "Resume" : "Start";
  }

  function disarmReset() {
    clearTimeout(resetArmTimeout);
    resetArmTimeout = null;
    resetBtn.classList.remove("armed");
    resetBtn.textContent = "Reset part";
  }

  function disarmPartButtons() {
    clearTimeout(partArmTimeout);
    partArmTimeout = null;
    partPickerList.querySelectorAll(".armed").forEach((button) => {
      button.classList.remove("armed");
      button.innerHTML = button.dataset.originalHtml || button.innerHTML;
    });
  }

  function guardedPartCommand(button, confirmLabel, action) {
    if (latestStatus === "idle") {
      disarmPartButtons();
      action();
      return;
    }
    if (!button.classList.contains("armed")) {
      disarmPartButtons();
      button.classList.add("armed");
      if (!button.dataset.originalHtml) {
        button.dataset.originalHtml = button.innerHTML;
      }
      button.textContent = confirmLabel;
      partArmTimeout = setTimeout(disarmPartButtons, ARM_TIMEOUT_MS);
      return;
    }
    disarmPartButtons();
    action();
  }

  startBtn.addEventListener("click", async () => {
    if (timerCommandPending || advancePending) return;
    // During the pre-meeting countdown one stray tap started the first part
    // early and took the countdown off the TV. Start still works then -- the
    // countdown is only as right as the start time saved in /setup, and a wrong
    // one must never lock the operator out -- but it takes a second tap.
    if (startNeedsConfirm() && !startBtn.classList.contains("armed")) {
      startBtn.classList.add("armed");
      startBtn.textContent = "Confirm start";
      startArmTimeout = setTimeout(disarmStart, ARM_TIMEOUT_MS);
      return;
    }
    disarmStart();
    timerCommandPending = true;
    const status = latestStatus;
    startBtn.disabled = true;
    startBtn.textContent = status === "paused" ? "Resuming..." : "Starting...";
    try {
      await timerCommand("/api/control/start");
    } catch {
      startBtn.disabled = false;
      startBtn.textContent = status === "paused" ? "Resume" : "Start";
    } finally {
      timerCommandPending = false;
    }
  });
  partPickerBtn.addEventListener("click", togglePartPicker);
  partPickerList.addEventListener("click", (event) => {
    const removeButton = event.target.closest("[data-remove-talk-id]");
    if (removeButton) {
      guardedPartCommand(removeButton, "Sure?", () => {
        command("/api/control/remove-part", { talkId: Number(removeButton.dataset.removeTalkId) });
      });
      return;
    }
    const button = event.target.closest("[data-talk-id]");
    if (!button) return;
    guardedPartCommand(button, "Confirm item", () => {
      closePartPicker();
      command("/api/control/select", { talkId: Number(button.dataset.talkId) });
    });
  });
  adhocPartBtn.addEventListener("click", () => {
    if (adhocPartPanel.classList.contains("hidden")) {
      openAdhocPartPanel();
    } else {
      closeAdhocPartPanel();
    }
  });
  adhocPartPanel.addEventListener("submit", (event) => {
    event.preventDefault();
    const title = adhocPartTitleInput.value.trim() || "Additional item";
    const minutes = Math.max(1, Math.min(120, Number(adhocPartMinutesInput.value || 5)));
    closeAdhocPartPanel();
    command("/api/control/adhoc-part", {
      title,
      seconds: minutes * 60,
      afterTalkId: Number(adhocPartAfterSelect.value) || 0,
    });
  });
  cancelAdhocPartBtn.addEventListener("click", closeAdhocPartPanel);
  document.addEventListener("click", (event) => {
    if (
      partPickerBtn.contains(event.target) ||
      partPickerList.contains(event.target) ||
      adhocPartBtn.contains(event.target) ||
      adhocPartPanel.contains(event.target)
    ) {
      return;
    }
    closePartPicker();
    closeAdhocPartPanel();
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      closePartPicker();
      closeAdhocPartPanel();
    }
  });
  // Ending a meeting stops the clock and closes the meeting, so it always takes
  // two taps. Unlike Next it names no item: ending twice ends once, so a retry
  // after a timeout cannot do any harm.
  endBtn.addEventListener("click", () => {
    if (!endBtn.classList.contains("armed")) {
      endBtn.classList.add("armed");
      endBtn.textContent = "Confirm end";
      endArmTimeout = setTimeout(disarmEnd, ARM_TIMEOUT_MS);
      return;
    }
    disarmEnd();
    advanceCommand("/api/control/end");
  });
  // Advancing discards a live timer's elapsed time with no way back, so while a
  // part is running or paused it takes two taps. Idle is the ordinary case
  // (the part just ended) and moves straight on -- which is why advanceCommand
  // holds a cooldown and names the item it is leaving.
  nextBtn.addEventListener("click", () => {
    if (latestStatus === "idle") {
      disarmNext();
      advanceCommand("/api/control/next", advanceFrom());
      return;
    }
    if (!nextBtn.classList.contains("armed")) {
      disarmPartButtons();
      nextBtn.classList.add("armed");
      nextBtn.textContent = "Confirm next";
      nextArmTimeout = setTimeout(disarmNext, ARM_TIMEOUT_MS);
      return;
    }
    disarmNext();
    advanceCommand("/api/control/next", advanceFrom());
  });
  // Selecting the part that is already current, not /api/control/reset: that
  // route is a documented alias for Next (see handleReset) and would advance
  // the meeting. handleSelect is the one that treats re-picking the current
  // item as "a restart, not a departure", so the seconds a false start burned
  // never reach the meeting's overtime tally.
  //
  // Always two taps, with no idle shortcut: unlike Next, Reset is only on
  // screen while a part is running or paused, so every tap of it is throwing
  // away elapsed time somebody is watching on the hall display.
  resetBtn.addEventListener("click", () => {
    if (!resetBtn.classList.contains("armed")) {
      disarmPartButtons();
      disarmNext();
      resetBtn.classList.add("armed");
      resetBtn.textContent = "Confirm reset";
      resetArmTimeout = setTimeout(disarmReset, ARM_TIMEOUT_MS);
      return;
    }
    disarmReset();
    const talkId = latestState && latestState.currentTalkId;
    if (!talkId) return;
    // Names the item as well as selecting it: on a phone whose stream has
    // fallen behind, "the current item" can be one the clock has already left,
    // and restarting it would drag the meeting back a part. The server refuses
    // that with a 412, which here just means there is nothing to reset.
    command("/api/control/select", { talkId, fromTalkId: talkId }, { quietConflict: true });
  });
  // Turning CO mode on replaces the whole programme, so it arms like End
  // does rather than flipping on a single tap. Turning it off stays one tap:
  // undoing a mistaken enable should be quicker than making one.
  coToggle.addEventListener("click", () => {
    const next = !(latestState && latestState.circuitOverseer);
    if (next && !coToggle.classList.contains("armed")) {
      coToggle.classList.add("armed");
      coHint.textContent = "Tap again to apply the circuit overseer schedule.";
      coHint.classList.remove("hidden");
      coArmTimeout = setTimeout(disarmCo, ARM_TIMEOUT_MS);
      return;
    }
    disarmCo();
    command("/api/control/circuit-overseer", { on: next });
  });
  if (languageSelect) {
    languageSelect.addEventListener("change", async () => {
      const language = languageSelect.value;
      languageCommandPending = true;
      languageSelect.disabled = true;
      if (languageStatus) {
        languageStatus.classList.remove("error");
        languageStatus.classList.remove("hidden");
        languageStatus.textContent = `Switching to ${languageName(language)} items...`;
      }
      try {
        // postJSON's 30s deadline matters here: a switch does two WOL fetches
        // server-side, and a request that never settles would leave
        // languageCommandPending set and the select disabled until reload.
        const state = await WallClock.postJSON("/api/control/midweek-language", { language });
        render(state);
        if (languageStatus) {
          languageStatus.classList.remove("error");
          languageStatus.classList.remove("hidden");
          languageStatus.textContent = `${languageName(language)} items applied.`;
        }
      } catch (error) {
        console.error(error);
        if (languageStatus) {
          if (error.status === 401 || error.status === 403) {
            if (error.status === 401) repairPairing();
            languageStatus.classList.add("error");
            languageStatus.classList.remove("hidden");
            languageStatus.textContent = `Pair this phone before changing languages.`;
          } else {
            languageStatus.classList.add("error");
            languageStatus.classList.remove("hidden");
            languageStatus.textContent = `Could not switch to ${languageName(language)}. ${cleanError(error.message)}`;
          }
        }
      } finally {
        languageCommandPending = false;
        if (latestState) {
          render(latestState);
        }
      }
    });
  }

  function languageName(language) {
    if (language === "es") return "Spanish";
    if (language === "tw") return "Twi";
    return "English";
  }

  function cleanError(message) {
    return String(message || "").trim().replace(/\s+/g, " ");
  }
  document.querySelectorAll("[data-adjust]").forEach((button) => {
    button.addEventListener("click", () => {
      command("/api/control/adjust", { deltaSeconds: Number(button.dataset.adjust) });
    });
  });
  // No page can add itself to a home screen — iOS has no install API at all,
  // and Android's only works over HTTPS — so the closest thing to auto-install
  // is saying the right one-tap instruction to the right phone. Shown once,
  // after pairing: before the PIN it would compete with the prompt that
  // matters, and a phone already running from its icon needs nothing.
  function showInstallHint() {
    const hint = document.getElementById("installHint");
    const text = document.getElementById("installHintText");
    const dismiss = document.getElementById("installHintDismiss");
    if (!hint || !text || !dismiss) return;
    const DISMISSED_KEY = "wallClockInstallHintDismissed";
    // "; wv)" is Android's WebView marker: the page is already inside an app
    // shell (the android/ wrapper), which is as installed as it gets — telling
    // it to open a browser menu it doesn't have would just confuse.
    const standalone = window.matchMedia("(display-mode: standalone)").matches ||
      window.navigator.standalone === true ||
      /; wv\)/.test(navigator.userAgent);
    let dismissed = false;
    try {
      dismissed = localStorage.getItem(DISMISSED_KEY) === "1";
    } catch {
      // Storage denied means the dismissal could never stick either; showing
      // the hint every visit would nag, so treat it as dismissed.
      dismissed = true;
    }
    if (standalone || dismissed) return;

    // iPadOS 13+ masquerades as a Mac, but a Mac with a touch screen isn't one.
    const ios = /iPhone|iPad|iPod/.test(navigator.userAgent) ||
      (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
    let message;
    if (ios) {
      // Safari never brands its iOS user agent; every other browser does.
      // Only Safari can add to the home screen, so anywhere else the useful
      // instruction is the detour, not the destination.
      const notSafari = /CriOS|FxiOS|EdgiOS|DuckDuckGo|GSA|OPR\/|OPT\//.test(navigator.userAgent);
      message = notSafari
        ? `To keep this controller on your home screen, open http://${location.host}/control in Safari, then tap Share and “Add to Home Screen”.`
        : "Keep this controller one tap away: tap the Share button, then “Add to Home Screen”.";
    } else if (/SamsungBrowser/.test(navigator.userAgent)) {
      // Also matches /Android/, so it must be asked first. Samsung Internet
      // puts its menu at the bottom and names the entry differently.
      message = "Keep this controller one tap away: tap the ☰ menu, then “Add page to” and “Home screen”.";
    } else if (/Android/.test(navigator.userAgent)) {
      message = "Keep this controller one tap away: open the browser menu (⋮), then “Add to Home Screen”.";
    } else {
      // A laptop has no home screen worth pitching.
      return;
    }
    text.textContent = message;
    hint.classList.remove("hidden");
    dismiss.addEventListener("click", () => {
      try {
        localStorage.setItem(DISMISSED_KEY, "1");
      } catch {
        // Hiding for this visit is all that's possible without storage.
      }
      hint.classList.add("hidden");
    });
  }

  async function init() {
    // A printed, tokenless QR (http://hallclock.local/control) lands here with
    // no token, so ask for the hall PIN. This blocks until the operator pairs —
    // a controller whose every button returns 401 is worse than one that says
    // plainly what it needs.
    await WallClock.ensurePaired();
    showInstallHint();
    WallClock.subscribe(render, (online) => {
      offlineNotice.classList.toggle("hidden", online);
    });
  }
  init();
})();
