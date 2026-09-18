package com.nuxcor.hallclock

import android.annotation.SuppressLint
import android.app.AlertDialog
import android.content.ActivityNotFoundException
import android.content.Intent
import android.graphics.Bitmap
import android.os.Bundle
import android.text.InputType
import android.view.View
import android.view.inputmethod.EditorInfo
import android.webkit.WebChromeClient
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.TextView
import androidx.activity.ComponentActivity
import androidx.activity.addCallback
import androidx.core.content.edit
import androidx.core.net.toUri
import androidx.core.view.ViewCompat
import androidx.core.view.WindowInsetsCompat
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import kotlin.concurrent.thread

// A deliberately thin shell: the whole controller UI lives on the Pi and keeps
// updating server-side, exactly as it does for browser users. This app exists
// only because Android browsers refuse a standalone home-screen install from a
// plain-HTTP origin, and the hall serves plain HTTP by design (see
// deploy/raspberry-pi/README.md "Why not HTTPS?"). Anything beyond "open the
// controller full screen" belongs in the web app, not here.
class MainActivity : ComponentActivity() {

    private lateinit var web: WebView
    private lateinit var errorView: View
    private lateinit var errorAddress: TextView
    private lateinit var spinner: View
    private lateinit var searchView: View

    // A hall the search found: the origin to load, and what to call it.
    private data class Hall(val base: String, val label: String)

    private val prefs by lazy { getSharedPreferences("hall-clock", MODE_PRIVATE) }

    // The full origin ("http://hallclock.local", "https://clock.example.org"),
    // not just the authority: an operator who pastes the URL shown on the setup
    // page must not have their scheme silently rewritten to http.
    private var base: String
        get() = prefs.getString("base", "") ?: ""
        set(value) = prefs.edit { putString("base", value) }

    private val baseAuthority: String
        get() = base.toUri().authority ?: ""

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        // Android 15+ draws every app edge-to-edge, which would slide the page
        // under the status bar; from targetSdk 36 there is not even an opt-out
        // attribute to fall back on. The controller's CSS has no safe-area
        // handling — it expects what iOS Safari's standalone mode gives it: a
        // solid dark bar above the page. Pad the (dark) root by the bar and
        // keyboard insets to reproduce exactly that.
        val root = findViewById<View>(android.R.id.content)
        ViewCompat.setOnApplyWindowInsetsListener(root) { v, insets ->
            val bars = insets.getInsets(
                WindowInsetsCompat.Type.systemBars() or WindowInsetsCompat.Type.ime(),
            )
            v.setPadding(bars.left, bars.top, bars.right, bars.bottom)
            WindowInsetsCompat.CONSUMED
        }

        web = findViewById(R.id.web)
        errorView = findViewById(R.id.errorView)
        errorAddress = findViewById(R.id.errorAddress)
        spinner = findViewById(R.id.spinner)
        searchView = findViewById(R.id.searchView)
        findViewById<View>(R.id.retryButton).setOnClickListener { load() }
        findViewById<View>(R.id.changeAddressButton).setOnClickListener { searchForHall(firstLaunch = false) }

        // The controller keeps its pairing token in localStorage, so DOM storage
        // is load-bearing, not an optimization: without it every launch would
        // demand re-pairing.
        web.settings.javaScriptEnabled = true
        web.settings.domStorageEnabled = true
        // A meeting runs longer than any screen timeout, and the page cannot ask
        // for this itself: the Screen Wake Lock API needs a secure context, which
        // a plain-HTTP hall will never be. So the shell is the only place a
        // running timer can stay visible without somebody poking the phone.
        web.keepScreenOn = true

        // Without a WebChromeClient the WebView silently swallows alert/confirm
        // and window.open. Nothing in the controller uses them today; the plain
        // base class restores the platform defaults so that stays a non-event
        // rather than a mystery for whoever adds the first one.
        web.webChromeClient = WebChromeClient()

        web.webViewClient = object : WebViewClient() {
            override fun shouldOverrideUrlLoading(
                view: WebView,
                request: WebResourceRequest,
            ): Boolean {
                // Keep the clock inside the shell; hand anything else (an
                // external link on a page, some future footer) to the browser
                // so this app never grows into one. Compare authorities so a
                // host saved with an explicit port (a dev server on :8480)
                // still counts as the clock, and so an http→https redirect on
                // the same host stays in the app rather than bouncing out.
                if (request.url.authority.equals(baseAuthority, ignoreCase = true)) return false
                return try {
                    startActivity(Intent(Intent.ACTION_VIEW, request.url))
                    true
                } catch (_: ActivityNotFoundException) {
                    // A mailto:/tel:/intent: URL on a phone with no handler must
                    // not take the controller down mid-meeting. Swallowing the
                    // tap is the least surprising outcome.
                    true
                }
            }

            override fun onPageStarted(view: WebView, url: String, favicon: Bitmap?) {
                errorView.visibility = View.GONE
                spinner.visibility = View.VISIBLE
            }

            override fun onPageFinished(view: WebView, url: String) {
                spinner.visibility = View.GONE
            }

            override fun onReceivedError(
                view: WebView,
                request: WebResourceRequest,
                error: WebResourceError,
            ) {
                // Subresource hiccups (a slow icon fetch) must not blank a
                // working controller; only a dead main frame is worth the
                // error screen.
                if (request.isForMainFrame) showError()
            }

            override fun onReceivedHttpError(
                view: WebView,
                request: WebResourceRequest,
                errorResponse: WebResourceResponse,
            ) {
                // A reachable-but-broken clock (Caddy up, Go app down: 502)
                // otherwise lands on the WebView's own bare error page, which
                // says nothing about wifi or .local names.
                if (request.isForMainFrame) showError()
            }
        }

        onBackPressedDispatcher.addCallback(this) {
            if (web.canGoBack()) web.goBack() else promptForLeave()
        }

        // A phone that has been pointed at its hall goes straight there. The
        // first launch looks for the halls on this network instead of opening a
        // built-in address: two halls sharing one Wi-Fi both answer, and opening
        // the first one's controller put the second one's operators in front of
        // the wrong hall's PIN prompt with nothing to say so.
        if (base.isNotEmpty()) load() else searchForHall(firstLaunch = true)
    }

    // Finds the halls on this network and lets the operator pick theirs by
    // name. On the first launch a single hall opens straight away — most
    // congregations have one, and asking them anything would be friction — and
    // finding none falls back to the built-in address, whose error screen
    // explains what to check. From Change address the list is always shown,
    // with the current hall ticked and a way to type an address.
    private fun searchForHall(firstLaunch: Boolean) {
        errorView.visibility = View.GONE
        searchView.visibility = View.VISIBLE
        findHalls { halls ->
            searchView.visibility = View.GONE
            when {
                firstLaunch && halls.size == 1 -> {
                    base = halls[0].base
                    load()
                }
                halls.isEmpty() && firstLaunch -> {
                    val fallback = normalizeBase(getString(R.string.host_default))
                    if (fallback.isEmpty()) {
                        promptForHost()
                    } else {
                        base = fallback
                        load()
                    }
                }
                halls.isEmpty() -> promptForHost()
                else -> chooseHall(halls, firstLaunch)
            }
        }
    }

    private fun chooseHall(halls: List<Hall>, firstLaunch: Boolean) {
        val current = halls.indexOfFirst { it.base == base }
        val builder = AlertDialog.Builder(this)
            .setTitle(R.string.hall_picker_title)
            .setSingleChoiceItems(halls.map { it.label }.toTypedArray(), current) { dialog, which ->
                dialog.dismiss()
                base = halls[which].base
                load()
            }
            .setNeutralButton(R.string.hall_picker_other) { _, _ -> promptForHost() }
        if (firstLaunch) {
            // Nothing is loaded yet, so there is nothing to go back to: the
            // choice is the way in.
            builder.setCancelable(false)
        } else {
            builder.setNegativeButton(R.string.menu_cancel, null)
        }
        builder.show()
    }

    // Asks every name in hall_candidates at once and hands back the ones that
    // answered as a hall clock, in list order. A .local name that does not
    // exist can take the whole mDNS timeout to fail, so this waits for all of
    // them only up to SEARCH_WAIT_MS and keeps whatever answered. `done` runs
    // on the main thread, and not at all once the activity is gone.
    private fun findHalls(done: (List<Hall>) -> Unit) {
        val candidates = resources.getStringArray(R.array.hall_candidates)
            .map { normalizeBase(it) }
            .filter { it.isNotEmpty() }
            .distinct()
        val found = ConcurrentHashMap<Int, Hall>()
        val finished = CountDownLatch(candidates.size)
        candidates.forEachIndexed { index, candidate ->
            thread(isDaemon = true) {
                try {
                    probeHall(candidate)?.let { found[index] = it }
                } finally {
                    finished.countDown()
                }
            }
        }
        thread(isDaemon = true) {
            finished.await(SEARCH_WAIT_MS, TimeUnit.MILLISECONDS)
            val halls = found.toSortedMap().values.toList()
            runOnUiThread {
                if (!isFinishing && !isDestroyed) done(halls)
            }
        }
    }

    // A hall clock answers /api/state without pairing, with the name set on its
    // setup page. Anything else at that name — a printer, a router — is not one.
    private fun probeHall(candidate: String): Hall? = try {
        val connection = URL("$candidate/api/state").openConnection() as HttpURLConnection
        connection.connectTimeout = PROBE_TIMEOUT_MS
        connection.readTimeout = PROBE_TIMEOUT_MS
        try {
            if (connection.responseCode != HttpURLConnection.HTTP_OK) {
                null
            } else {
                // Bounded: whatever answers at a guessed name is not trusted to
                // be small.
                val body = connection.inputStream.bufferedReader().use { reader ->
                    val chars = CharArray(MAX_STATE_CHARS)
                    var length = 0
                    while (length < chars.size) {
                        val read = reader.read(chars, length, chars.size - length)
                        if (read < 0) break
                        length += read
                    }
                    String(chars, 0, length)
                }
                val state = JSONObject(body)
                if (state.has("deviceName") && state.has("schedule")) {
                    Hall(candidate, hallLabel(state.optString("deviceName"), candidate))
                } else {
                    null
                }
            }
        } finally {
            connection.disconnect()
        }
    } catch (_: Exception) {
        null
    }

    // The name from the hall's setup page with its address beneath, so two
    // halls read differently even to someone who has never seen either. A hall
    // still called the default "Hall Clock" is shown by its address alone.
    private fun hallLabel(deviceName: String, candidate: String): String {
        val address = candidate.toUri().authority ?: candidate
        val name = deviceName.trim()
        return if (name.isEmpty() || name == "Hall Clock") address else "$name\n$address"
    }

    private fun load() {
        errorView.visibility = View.GONE
        // Cold starts spend their whole first few seconds in mDNS resolution,
        // where no page event fires at all — without this the app is a black
        // rectangle for exactly as long as the phone is slowest to resolve.
        spinner.visibility = View.VISIBLE
        web.loadUrl("$base/")
    }

    private fun showError() {
        spinner.visibility = View.GONE
        errorAddress.text = getString(R.string.error_address, base)
        errorView.visibility = View.VISIBLE
    }

    // Back at the root of the history stack is the app's only menu: every other
    // affordance (a floating button, a long-press) would either put chrome over
    // the controller or be undiscoverable, and back is the one control everybody
    // tries. Ordered safest to most destructive, so Close is not where a thumb
    // lands by habit, and dismissing means stay — there is no "stay" entry to
    // mis-tap.
    private fun promptForLeave() {
        val labels = mutableListOf<String>()
        val actions = mutableListOf<() -> Unit>()

        // reload(), not load(): it re-requests whatever page is showing, so an
        // operator who walked to /setup stays there. Assets are served no-store,
        // so this genuinely re-fetches. The meeting is unaffected either way —
        // the clock runs on the Pi, not in this WebView.
        labels += getString(R.string.menu_reload)
        actions += { web.reload() }

        // Suppressed when the error screen is already offering it: this dialog
        // does not cover that button, so both would be on screen at once.
        if (errorView.visibility != View.VISIBLE) {
            labels += getString(R.string.menu_change_address)
            actions += { searchForHall(firstLaunch = false) }
        }

        // The reassurance rides on the item itself rather than sitting in a
        // message above it: this is the moment somebody mid-meeting needs to
        // read it, and a message block is what gets scanned past.
        labels += getString(R.string.menu_close)
        actions += { finish() }

        AlertDialog.Builder(this)
            .setTitle(R.string.app_name)
            .setItems(labels.toTypedArray()) { _, which -> actions[which]() }
            .setNegativeButton(R.string.menu_cancel, null)
            .show()
    }

    // Typing an address is the fallback now, reached from the hall list's
    // "Another address" or when the search finds nothing: a hall behind an IP,
    // a custom name, or a network whose Wi-Fi does not carry mDNS.
    private fun promptForHost(
        error: String? = null,
        // Re-prompting after a rejected address keeps what was typed: the fix is
        // usually one character, and clearing it back to the default would throw
        // away a hand-typed IP.
        prefill: String? = null,
    ) {
        // Built against the builder's themed context, not the activity's, or the
        // field keeps the platform's default styling inside a restyled dialog.
        val builder = AlertDialog.Builder(this)
        val themed = builder.context
        val input = EditText(themed).apply {
            inputType = InputType.TYPE_TEXT_VARIATION_URI
            imeOptions = EditorInfo.IME_ACTION_GO
            setSingleLine()
            hint = getString(R.string.host_dialog_hint)
            setText(prefill ?: base)
            setSelection(text.length)
            if (error != null) this.error = error
        }
        // A bare EditText in an AlertDialog sits flush against the edges.
        val frame = FrameLayout(themed).apply {
            val pad = (24 * resources.displayMetrics.density).toInt()
            setPadding(pad, pad / 2, pad, 0)
            addView(input)
        }
        val dialog = builder
            .setTitle(R.string.host_dialog_title)
            .setView(frame)
            .setPositiveButton(R.string.host_dialog_ok) { _, _ -> applyHost(input.text.toString()) }
            .setNegativeButton(R.string.host_dialog_cancel, null)
            // Backing out on the first launch, before any hall was chosen, would
            // leave an empty app with nothing to go back to: search again.
            .setOnCancelListener { if (base.isEmpty()) searchForHall(firstLaunch = true) }
            .show()
        dialog.getButton(AlertDialog.BUTTON_NEGATIVE).setOnClickListener {
            dialog.dismiss()
            if (base.isEmpty()) searchForHall(firstLaunch = true)
        }
        input.setOnEditorActionListener { _, actionId, _ ->
            if (actionId != EditorInfo.IME_ACTION_GO) return@setOnEditorActionListener false
            dialog.dismiss()
            applyHost(input.text.toString())
            true
        }
    }

    private fun applyHost(raw: String) {
        val entered = normalizeBase(raw)
        if (entered.isNotEmpty()) {
            base = entered
            load()
        } else {
            // Dismissing on unparseable input reads as "accepted" and leaves the
            // app pointed wherever it already was. Say what was wrong instead.
            promptForHost(getString(R.string.host_dialog_error), raw)
        }
    }

    // People paste whatever they have — a bare name, an IP, a host:port, or a
    // full URL copied from a browser or the setup page. Reduce it all to the
    // origin the app loads, keeping any explicit port (the appliance is
    // portless, but a dev server is not) and any explicit https (a hall that
    // put the clock behind a real certificate must not be downgraded).
    private fun normalizeBase(raw: String): String {
        val trimmed = raw.trim()
        if (trimmed.isEmpty()) return ""
        val withScheme = if (trimmed.contains("://")) trimmed else "http://$trimmed"
        val parsed = withScheme.toUri()
        val scheme = parsed.scheme?.lowercase() ?: return ""
        if (scheme != "http" && scheme != "https") return ""
        val authority = parsed.authority?.takeIf { it.isNotBlank() } ?: return ""
        return "$scheme://$authority"
    }

    private companion object {
        // Long enough for a phone slow to resolve .local on a cold start, short
        // enough to sit through once: it only ever runs on the first launch and
        // from Change address.
        const val SEARCH_WAIT_MS = 6000L
        const val PROBE_TIMEOUT_MS = 3000
        // A hall's state is a few kilobytes; this leaves room for a long
        // programme without reading an unbounded body.
        const val MAX_STATE_CHARS = 256 * 1024
    }
}
