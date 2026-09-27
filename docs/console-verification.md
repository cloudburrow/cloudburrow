# Console verification

The [parity checklist](console-parity.md) walked in a real browser against a running instance.

It was first walked by hand on 2026-09-21, with Chromium driven through Playwright against a
stack created for the purpose. Since [#594](https://github.com/cloudburrow/cloudburrow/issues/594)
the browser walk is a test suite instead of a transcript: `test/browser` drives headless
Chrome through [chromedp](https://github.com/chromedp/chromedp) and the DevTools protocol, and
CI runs it against the compat job's storage shard instance. The step fails `ci-green` unless
every test in the package passed; a skipped test counts as a failure.

---

## 1. The automated run

[`test/browser/console_test.go`](../test/browser/console_test.go), build tag `browser`:

| What | Test | Asserted in the browser |
|---|---|---|
| Shell landmarks and the LOCAL badge | `TestConsoleShellLandmarksAndLocalBadge` | At `/`, Chrome's accessibility tree has the `banner`, `navigation` and `main` landmarks, and the LOCAL badge is rendered, with a size and not hidden, inside the banner. |
| Theme switch with no navigation | `TestThemeSwitchesWithoutNavigation` | Dark, Light and Same as device, each picked from the settings panel: `data-theme` follows, the choice is stored, the body background changes between dark and light, and Same as device matches dark under an emulated dark preference. The main frame never navigates after the first load, and a value set on `window` survives every switch. |
| Create a bucket through the real form | `TestCreateBucketThroughTheForm` | From the empty Cloud Storage screen of a new project, **Create** opens the form. A name the pattern rejects is refused under the field with the constraint in words and the dialog stays open, with **no POST sent**. A valid name is then created through the same form: the dialog closes, its row appears, exactly one POST was made, and the console API lists the bucket. |
| Error state under an injected fault | `TestInjectedFaultShowsTheServicesMessage` | A rule on Cloud Tasks' `ListQueues`, scoped to the test's project, is installed through `POST /admin/faults` with the instance's admin token. The Cloud Tasks screen shows its error card (`role="alert"`, "Cloud Tasks unavailable") carrying the service's own message, `Unavailable: injected fault (rule fault-N): UNAVAILABLE`, rather than an empty table. With the rule removed, **Retry** brings the screen back. |
| Keyboard row activation | `TestKeyboardOpensAListRow` | With no pointer: Tab reaches **Show info panel** and Enter shows the panel; Tab reaches the queue's row and Enter opens it in the panel, which is headed by the queue's name, and the row is `aria-pressed`. |
| No request leaves loopback | every test, and `TestLoopbackGuardCatchesAnOffLoopbackRequest` | See below. |

Every test also fails on any exception, `console.error` or error-level log entry the page
reports. That is how the invalid pattern in section 2 first showed itself, and it is still one
of the two ways this suite catches it.

Each test starts its own browser with a fresh profile and registers a project of its own
through the console, so its resources and its fault rule cannot touch another test's, and
removes them when it ends. A failing test writes a screenshot of the page to
`CLOUDBURROW_TEST_SCREENSHOTS`, which CI uploads as the `browser-screenshots` artifact.

### Loopback only

Two mechanisms, one to prevent and one to detect:

- Chrome is started with its proxy set to a sinkhole the test runs on loopback. Loopback is
  never proxied, so the console is reached directly, and a request for any other host reaches
  the sinkhole, which records it and answers 502. Nothing the browser asks for leaves the
  machine, whether the page or Chrome itself asked.
- Every request the page makes is read from the DevTools Network domain, and the test fails
  on any whose host is not loopback.

Chrome's own services (component updates, sign-in, autofill) call home from the browser
process rather than from the page. They land on the sinkhole, go nowhere, and are logged
rather than failed, since they are not the console's. So that the check cannot pass
vacuously, `TestLoopbackGuardCatchesAnOffLoopbackRequest` has a page fetch a TEST-NET-1
address (192.0.2.1, never routed) with the console's CSP bypassed for that page only, since
the CSP would otherwise refuse it before the network, and asserts that the Network domain
reported it and the sinkhole received it.

### The pattern regression, proved

With the bucket pattern's escaped `-` reverted to `[a-z0-9._-]`, which is the bug in section
2, the run fails, for both of the reasons it should:

```
--- FAIL: TestCreateBucketThroughTheForm
    an invalid bucket name was not refused by its pattern: {Message: RowClass:form-row DialogUp:true Validates:false}
    an invalid bucket name was posted: [http://127.0.0.1:<console>/api/resources/storage?project=cb-browser-…]
    the page reported an error: log (rendering): Pattern attribute value ^[a-z0-9][a-z0-9._-]{1,61}[a-z0-9]$
      is not a valid regular expression: … Invalid regular expression: /^[a-z0-9][a-z0-9._-]{1,61}[a-z0-9]$/v:
      Invalid character in character class
```

`TestCreateFormPatternsAreValidInTheBrowser`, the Go unit test that compiles every shipped
pattern under the `v` rule, fails on the same change without a browser.

### Recorded run

2026-09-27, on macOS against an instance with `--services storage,pubsub,tasks,secretmanager,scheduler`
and every port OS-assigned, Google Chrome 153: all six tests passed, three runs in a row, in
about three seconds each.

### What the browser run does not cover, and where it is covered

- **Resources are real.** A console-created bucket and topic are read back through the official
  SDKs by `TestConsoleCreatedBucketIsVisibleToTheOfficialSDK` and
  `TestConsoleCreatedTopicIsVisibleToTheOfficialSDK`, and an SDK-created topic appears in the
  console in `TestSDKCreatedTopicAppearsInTheConsole`, in the compat suite.
- **The #19 acceptance workflow** runs in CI's acceptance shard (`test/e2e`).
- **Loading that ends.** Measured by hand, against a backend stubbed to never answer, when
  [#129](https://github.com/cloudburrow/cloudburrow/issues/129),
  [#130](https://github.com/cloudburrow/cloudburrow/issues/130) and
  [#152](https://github.com/cloudburrow/cloudburrow/issues/152) landed: a skeleton still there
  after eight seconds says what it is waiting for and offers Cancel, the log stream states its
  own health, and a failure in the console's own script renders an error card. Not automated.
- **Focus indicators and colour contrast** are asserted from the stylesheet by
  `TestOutlineSuppressionHasAFocusVisibleRule` and `TestTokensMeetContrast`.
- **Offline assets.** A unit test greps the embedded assets for every CDN and web-font host;
  the browser run adds that, at run time, nothing the page does leaves loopback.

---

## Also measured on the first walkthrough's stack

### The kubelet already answers per-pod, and was being counted and thrown away

Measured before building anything on it, because the alternative was assuming
it. On this project's kind node, `/stats/summary` through the API server's
node proxy:

```
node keys       cpu, fs, io, memory, network, nodeName, rlimit, runtime, startTime, swap, systemContainers
node.cpu        psi, time, usageCoreNanoSeconds, usageNanoCores
node.memory     availableBytes, majorPageFaults, pageFaults, psi, rssBytes, time, usageBytes, workingSetBytes

pods            23
pod.cpu         psi, time, usageCoreNanoSeconds, usageNanoCores
pod.memory      availableBytes, majorPageFaults, pageFaults, psi, rssBytes, time, usageBytes, workingSetBytes
container.cpu   psi, time, usageCoreNanoSeconds, usageNanoCores

pods with cpu reading    : 23/23
pods with memory reading : 23/23

  cloudburrow/pubsub-7f69ccc95f-f8nzp     cpu=1243126  mem=589336576
  cloudburrow/firestore-8657446c4d-k8sqm  cpu=447066   mem=512942080
```

So the per-pod half of [#182](https://github.com/cloudburrow/cloudburrow/issues/182)
is real rather than aspirational: every pod reports both, with the cumulative
`usageCoreNanoSeconds` and the kubelet's own `time`. The decoder was keeping
node CPU, node memory and the **length** of the pods array, and discarding the
rest of a response that had already been fetched and paid for.

A trimmed capture of this response is committed at
`cmd/cloudburrow/testdata/kubelet-summary.json` so the decoder is tested
against the real shape with no cluster required.

## 2. What the first walkthrough found

### The create forms never validated anything

Driving a real browser produced this in the console:

```
[ERROR] Pattern attribute value ^[a-z0-9][a-z0-9._-]{1,61}[a-z0-9]$ is not a valid
        regular expression: Invalid regular expression: /.../v:
        Invalid character in character class
```

An HTML `pattern` attribute is compiled with the RegExp **`v` flag**, where a literal `-`
inside a character class must be escaped even in trailing position. An unescaped one makes
the whole pattern invalid — and the browser then **silently skips validation** rather than
reporting it to the user. Every create form had shipped with a pattern the browser refused.

The form appeared to validate and did not. Nothing in the Go tests could see it, because the
patterns are valid Go regexps; only a browser rejects them.

Fixed, and after the fix:

```
patternValid: true             inputRejectedByBrowser: true
dialogStillOpen: true          postRequests: []      ← nothing was sent
```

**Superseded by [#133](https://github.com/cloudburrow/cloudburrow/issues/133).** The form
now carries `novalidate`, so the browser no longer rejects the input — the console does, and
says which field and why. `inputRejectedByBrowser` is therefore `false` by design; what the
row above was really evidencing, that **nothing is sent**, still holds, and the message is
now attached to the offending field rather than left to a bubble that vanishes on the next
keystroke:

```
emptySubmit: { row: "form-row is-invalid", message: "Service name is required.",
               ariaInvalid: "true", focused: "f-name", postRequests: [] }
afterCorrection: { row: "form-row", message: "", describedBy: "h-name" }
```

A regression test now compiles every shipped pattern's character classes under the `v` rule,
checks each default satisfies its own pattern, and checks every required field explains its
constraint. Reverting the fix makes it fail, and makes the browser suite fail too (section 1).

### The navigation was invisible below 1280px

*(Historical: the icon rail this describes was never reachable and has since been removed —
see the note under Viewports in `console-parity.md`. The defect below was real and its fix
still stands; only the rail it was written against is gone.)*

The rail was meant to hide labels and keep icons — that is the entire point of a rail. The
rule was written as:

```css
nav a span { display: none; }      /* at ≤1279px */
```

which also matched the `<span class="nav-icon">` wrapping each icon. Below 1280px the
navigation was an empty 72px column: no icons, no labels, nothing visible to click. That is
most laptop windows, and the structural checks above did not catch it because the DOM was
entirely correct — eight links, eight SVGs, every label present. Only the paint was missing.

Now `nav a .nav-label`, with an `aria-label` on each link so a link still announces a name
wherever its text is hidden.

### Every list screen rendered the word "null"

Above the filter row, on every page whose listing carried no note:

```js
const note = data.note ? el("p", ...) : null;
view.replaceChildren(...header, note, ...);
```

`Node.replaceChildren` stringifies anything that is not a Node, so the `null` became the
text `null`. The `el()` helper had always filtered absent children; the direct calls did
not. All twenty child-setting call sites now go through `setChildren`, which applies the
same rule, and a test forbids the direct form.

### A test that failed for the wrong reason

`TestUpFailsOnOccupiedControlPort` pinned only `--port-control` and left every other port at
its default, so an unrelated process holding one of them made the test fail **naming the
wrong port**. Every other port is now OS-assigned.

---

## 3. What was not verified, and why

### Visual regression against reference screenshots

**Not done, and not possible today.** [#43](https://github.com/cloudburrow/cloudburrow/issues/43)
established that no authorized read-only console session was available, so **no dated
reference screenshots exist**. Without a reference there is nothing to diff against, and a
threshold measured against a screenshot of our own output would only prove the console still
looks like itself.

What was verified is **structural**: page names, navigation paths, control labels, the four
states, table semantics and the documented theme behaviour. Pixel parity — spacing, type
scale, palette, row heights — remains **unverified and unclaimed**, exactly as the parity
specification says.

A later pass brought the visual treatment closer to the console's published design language:
control radii separated from surface radii (4px against 8px), an elevation shadow under the
toolbar instead of a hairline, 40px navigation rows with the selected pill hinged on the rail
edge, 12px column headers against 14px cells, and denser cards. The palette was already the
published one. **None of this is parity** and none of it was diffed against a reference,
because there is still no reference to diff against. It is a closer approximation, verified
only as "renders as intended in a browser".

This is the gap that keeps console visual fidelity at `Partial` in the support matrix.
Closing it needs reference screens, not more work on the build.

### Restart and reset

Not re-run as part of this walkthrough, with one incidental observation: restarting the stack
too quickly after stopping it was **correctly refused**, with an actionable message —

```
cloudburrow: start tasks: open Cloud Tasks state: data directory is in use by
another instance: .../p49/tasks (remove .../owner.lock if no instance is running)
```

which is the lock doing its job. Resource durability across a restart is covered by the
service-level tests and by the mode semantics in [configuration.md](configuration.md); the
console holds no state of its own, so there is nothing additional in it to survive one.

### Subscriptions in the UI

The acceptance criterion asks for a subscription created from the browser. The console does
not offer subscription creation — [#45](https://github.com/cloudburrow/cloudburrow/issues/45)
shipped topics only, and recorded that. A UI flow for it does not exist, so it was not walked.

**Partly closed by [#154](https://github.com/cloudburrow/cloudburrow/issues/154).** Create
topic now carries an **Add a default subscription** checkbox, and it is not decoration: the
subscription is created through the same API a client would use, and a failure to create it
is reported rather than swallowed. Verified against a live instance — the topic was created
from the browser, and the emulator was then asked directly:

```
GET /v1/projects/demo/subscriptions
  projects/demo/subscriptions/parity-batch3-topic-sub → projects/demo/topics/parity-batch3-topic
```

Creating a subscription **on its own**, with its own settings, is still not offered.

---

## 4. Running it

Against an instance of your own, never one you are using:

```sh
make build
./bin/cloudburrow up --detach --name browser --state-dir ./state \
  --port-control 0 --port-console 0 --port-storage 0 --port-pubsub 0 \
  --services storage,pubsub,tasks,secretmanager,scheduler

eval "$(scripts/compat-env.sh --only CONSOLE,CONTROL,ADMIN_TOKEN --name browser --state-dir ./state)"
go test -tags=browser -count=1 -v ./test/browser/

./bin/cloudburrow stop --name browser --state-dir ./state
```

chromedp starts the Chrome or Chromium already installed (`google-chrome`, `chromium`, or
Chrome.app on macOS); `CLOUDBURROW_TEST_CHROME` names another binary. Nothing is downloaded.
CI uses the runner image's Google Chrome and falls back to a pinned Chrome for Testing only if
the image ever drops it.

No live Google endpoint is contacted at any point. The suite refuses to run with cloud
credentials in the environment, refuses a console address that is not loopback, and sinks
anything the browser tries to send elsewhere.

---

## 5. Verdict

The console is usable for what it claims: create and delete buckets, topics and queues;
deploy and delete Cloud Run services; read cluster state; follow live logs; track operations
with their causes. Each of those was driven from a browser and cross-checked outside it, and
the flows listed in section 1 now run in a browser in CI.

**It is not verified as visually matching the GCP console**, and this document does not claim
it is. That claim needs reference screens that do not exist.
