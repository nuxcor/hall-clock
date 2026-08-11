# Hall Clock — Android wrapper

A thin WebView shell around the controller at `http://hallclock.local`. It
exists for one reason: Android browsers refuse a standalone home-screen install
from a plain-HTTP origin, and the hall serves plain HTTP by design (see
`../deploy/raspberry-pi/README.md` "Why not HTTPS?"). iOS needs no equivalent —
Safari's "Add to Home Screen" already installs standalone over HTTP via the
`apple-*` meta tags.

The controller UI itself stays on the Pi and updates server-side, so this app
should almost never need a release. Resist adding features here; they belong in
the web app, where every phone — iOS, Android, plain browser — gets them. The
exceptions are the handful of things a plain-HTTP page provably cannot do for
itself, which is why the shell holds the screen awake (see below).

## What the shell does

- Loads the controller full screen, dark-themed to match it, with a spinner
  from the moment a load starts — a phone slow to resolve `.local` shows
  nothing at all until DNS gives up, and a black rectangle reads as a crash.
- **No setup screen.** The clock's address is built in (`host_default` in
  `strings.xml` — the one value to change when pointing a build at a different
  hall) and the app opens it straight away. Every phone in a hall talks to the
  same Pi, so asking each operator to type an address is friction for all of
  them and a typo waiting to happen. It can still be changed at runtime, from
  the error screen or the back menu, and that value is stored in app
  preferences. Anything reasonable is accepted — a bare name, an IP, a
  `host:port`, or a pasted `http://`/`https://` URL, scheme preserved so a hall
  behind a real certificate is not downgraded.
- **One hall per build, deliberately — not a picker.** A second hall
  (`hallclock-2.local`) does not get its own entry in a list: operators belong
  to one hall, so a phone is pointed once and never again, and a saved-address
  picker would be persistent machinery — with its own eviction rules — serving
  a one-time act. Hall two's phones enter that address once from the error
  screen and keep it. Revisit only if people start working both halls.
- **Keeps the screen on.** A meeting outlasts any screen timeout, and the page
  cannot ask for this itself: the Screen Wake Lock API requires a secure
  context, which a plain-HTTP hall will never have. The shell is the only
  place this can be fixed.
- Keeps localStorage (the pairing token), so pairing survives relaunches.
- Back walks WebView history; at the root it asks before closing, and that
  dialog is also where **Change address** lives. It has to be reachable from a
  working page: an address that loads *something* (a router's admin page, say)
  never trips the error screen, and there would otherwise be no way out short
  of clearing app data.
- External links open in the real browser; a link no app can handle is
  swallowed rather than crashing the shell.
- A dead or erroring main frame (including a 502 from Caddy with the Go app
  down) shows a retry screen naming the usual cause (wrong Wi-Fi) and the one
  thing to try next: enter the Pi's IP address instead. It stays deliberately
  short — an operator reading it has a meeting waiting, and the reasons a
  `.local` name fails (an AP that will not bridge mDNS between wireless and the
  wired Pi; Android 11 and older, which cannot resolve `.local` at all) are the
  installer's problem, not theirs. See `strings.xml` for both.

Cleartext HTTP is allowed app-wide in `network_security_config.xml`; a
per-domain config cannot express "any `.local` name or a user-entered LAN IP",
and the app only ever loads the configured clock host.

## Build

Always `./gradlew`, never a `gradle` off your PATH:

```sh
cd android
./gradlew assembleDebug
```

The wrapper is committed on purpose. AGP 8.13 uses a Gradle internal API that
**Gradle 9.6 removed**, so a Homebrew `gradle` (9.6.1 at time of writing) fails
at plugin application with `InternalProblems ... removed in Gradle 9.6.0`. The
wrapper pins 9.3.1 and downloads it on first run, so the build does not depend
on what anyone happens to have installed. That is worth more than keeping a
46KB jar out of the tree.

Debug APK lands in `app/build/outputs/apk/debug/`. Release builds use your
existing Play signing setup (`./gradlew bundleRelease` for an AAB). CI runs the
same wrapper and assembles debug **and** release plus lint on every PR — release
because that is the only build exercising R8 and `lintVitalRelease`, which
otherwise fail for the first time at Play upload.

Two toolchain pins, both load-bearing:

- **AGP 8.13, not 8.7** — Android Studio now bundles a JDK 25 JBR, and 8.7's
  lint throws `IllegalArgumentException: 25.0.2` under it, taking
  `bundleRelease` with it.
- **Gradle 9.3.1, not 9.6+** — see above. Moving to 9.6 means moving off
  AGP 8.x.

Any JDK from 17 up works for the build itself; CI uses 17.

## Play distribution

Publish to a **closed testing track** (or internal testing) rather than a
public listing:

- The app is useless outside a hall running this clock, so a public listing
  serves no one.
- Google Play's "minimum functionality" policy frowns on thin web wrappers in
  public review; closed tracks with an invited tester list avoid that
  friction entirely.

Invite member Google accounts (or a Google Group) as testers; they install
from the Play link like any app and get updates automatically on the rare
occasion the shell changes.

**Play forces a release roughly once a year even when nothing here changes.**
`targetSdk` has to stay within one year of the latest Android release or the
listing stops accepting updates — the deadline lands each August, and missing
it is not fatal but means no fix can ship until the bump does. So the bump is
its own release: raise `targetSdk` and `compileSdk` to the new API level, raise
`versionCode`, read Google's behaviour-changes page for *apps targeting* that
level, and check the app still runs. The two that have bitten this shell are
edge-to-edge (the inset padding in `MainActivity` is what keeps the controller
out from under the status bar) and back handling (`OnBackPressedDispatcher`,
because `onBackPressed()` stopped being called at 36). An emulator running the
new API level is enough to check both: point the app at a dev server with
`adb shell run-as com.nuxcor.hallclock` to write `base` into `shared_prefs`,
then look at the status bar, the keyboard, and the back menu.

## APK distribution

For anyone who cannot use Play, `./gradlew assembleRelease` produces a signed
APK at `app/build/outputs/apk/release/app-release.apk`. Rename it to
`hall-clock.apk` and attach it to a GitHub release.

**Attach it to an existing release. Never publish a release for the APK
alone.** `hall-clock-update.sh` resolves updates through
`/repos/<repo>/releases/latest`, which is whichever release published most
recently, regardless of its tag. An APK-only release therefore becomes what
every Pi checks, and they would look in it for a Linux binary that is not
there — an outage across every hall, caused by shipping a phone app.

Hand out the **pinned** asset URL, not the `latest` alias:

```text
https://github.com/nuxcor/hall-clock/releases/download/<tag>/hall-clock.apk
```

`releases/latest/download/hall-clock.apk` looks tidier and breaks almost
immediately: CI cuts a release on every merge to `main`, none of those builds
carry an APK, and the alias starts 404ing on the next server change. A pinned
URL keeps working forever, and the shell changes rarely enough that reissuing
the link is cheaper than teaching CI to sign Android builds — which would mean
putting the upload keystore into Actions secrets.

A sideloaded APK and a Play install **cannot upgrade to one another.** Play App
Signing is mandatory for new apps, so Google re-signs the AAB with a key that
is not the upload key this APK is signed with, and Android refuses to swap one
for the other. A phone moving between the two has to uninstall first, losing
its saved clock address and pairing token. Pick one channel per phone.
