# Console verification

The result of walking the [parity checklist](console-parity.md) against a **fresh stack**,
driven through a real browser.

Run on **2026-09-21**, against a cluster created from nothing for this purpose
(`cloudburrow delete && cloudburrow up`), Chromium via Playwright.

---

## 1. What passed

### Browser-driven resource creation

A bucket created entirely through the UI — empty state, its create button, the form, submit —
and the resulting row:

```
emptyStateShown: true      emptyTitle: "No buckets yet"
emptyOffersCreate: true    createLabel: "Create"
noFakeRows: 0              ← no seeded data, which is what the criterion forbids

after submit:
dialogClosed: true
rows: [["console-e2e-bucket", "US-CENTRAL1", "STANDARD", "2026-09-21 16:58", "Delete"]]
liveRegion: "1 buckets loaded"
```

A topic the same way:

```
dialogClosed: true
rows: ["projects/console-e2e-demo/topics/console-e2e-topic"]
```

### Cross-checked outside the console

Both are real resources, read back through the services the SDKs use:

```
$ curl .../storage/v1/b/console-e2e-bucket
{"kind":"storage#bucket","id":"console-e2e-bucket","name":"console-e2e-bucket",...}

$ curl .../v1/projects/console-e2e-demo/topics
{"topics": [{"name": "projects/console-e2e-demo/topics/console-e2e-topic"}]}
```

The other direction — SDK-created resources appearing in the UI — is covered by
`TestSDKCreatedTopicAppearsInTheConsole` in the compatibility suite.

### The #19 acceptance workflow, on the same fresh stack

```
1. uploaded input.txt via the official Cloud Storage SDK
2. worker deployed and ready at http://e2e-worker.default.svc.cluster.local
3. published message id=3 to a push subscription targeting the worker
4. worker wrote result.txt = "CLOUDBURROW END TO END"
ACCEPTANCE WORKFLOW: PASS
```

The exact resulting bytes are asserted, not merely the object's existence.

### The four states

| State | Evidence |
|---|---|
| Empty | Above: named, offers create, **zero rows** |
| Loading | Skeleton rendered before data arrives |
| Error | The pubsub tunnel was killed mid-session; the console reported `{"unavailable":"list topics: context deadline exceeded"}` rather than an empty table |
| Operating | Every mutation appears in the notifications panel and carries its terminal state; while it is outstanding the submit button, the affected row and a bar under the toolbar all say so ([#132](https://github.com/cloudburrow/cloudburrow/issues/132)) |

The error case is the one that matters: an empty table says *"you have none"* and sends a
developer to debug their own code.

**Extended by [#129](https://github.com/cloudburrow/cloudburrow/issues/129),
[#130](https://github.com/cloudburrow/cloudburrow/issues/130) and
[#152](https://github.com/cloudburrow/cloudburrow/issues/152).** Loading is now a state that
ends. Every read the server serves is bounded, every call the browser makes carries a
deadline, a skeleton that is still there after eight seconds says what it is waiting for and
offers a way out, the log stream states its own health rather than going quiet, and a failure
in the console's own script renders an error card instead of leaving the skeleton up forever.
Measured against a backend stubbed to never answer:

```
t=2s   skeleton, nothing said
t=9s   "Still loading cloud storage… [Cancel]"
Cancel "Cloud Storage not loaded — you stopped this request before it finished. [Try again]"

log stream, idle 25s   keepalive observed, status stayed "streaming" (green)
filter with no matches "No entries match these filters"
render throws          "Dashboard stopped — the console hit an error in its own code"
stray rejection        a banner above the page, with Reload
```

### Keyboard and accessibility

```
focusableCount: 25          firstFocusReceived: true
landmarks: { banner: true, navigation: true, main: true }
liveRegion: true            skipLink: "Skip to main content"
panelOpened: true           focusMovedIntoPanel: true
panelClosedOnEscape: true   focusReturned: true
themeApplied: "dark"        noReload: true
```

The theme change without a page reload is the documented console behaviour, and it holds.

### Project isolation

A second project shows its own resources. Cloud Storage is the exception and **says so**: the
backend accepts the project parameter and returns every bucket, so the screen carries the
caveat rather than presenting rows under a heading it did not honour.

### Offline

```
/            remote refs: 0
/console.css remote refs: 0
/console.js  remote refs: 0
```

No font, CDN or Google endpoint is referenced. The assets are embedded in the binary, and a
unit test greps for every such host so this cannot regress.

---

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

## 2. What this found

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
constraint. Reverting the fix makes it fail.

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

## 4. Reproducing this

```sh
# a genuinely fresh stack
cloudburrow delete --name p49 && rm -rf ./state
cloudburrow up --name p49 --state-dir ./state --port-console 9091 &

# the acceptance workflow, unchanged
export CLOUDBURROW_TEST_STORAGE=http://127.0.0.1:<storage>
export CLOUDBURROW_TEST_PUBSUB=127.0.0.1:<pubsub>
export CLOUDBURROW_TEST_RUN=127.0.0.1:<run>
export CLOUDBURROW_TEST_CLUSTER=cloudburrow-p49
export CLOUDBURROW_TEST_KUBECONFIG=./state/p49/kubeconfig
export CLOUDBURROW_TEST_STORAGE_INCLUSTER=storage-internal.cloudburrow.svc.cluster.local:4443
make test-e2e

# the console, in a browser
open http://127.0.0.1:9091
```

No live Google endpoint is contacted at any point. The compatibility harness refuses to run
if cloud credentials are present in the environment, and the console's assets reference
nothing remote.

---

## 5. Verdict

The console is usable for what it claims: create and delete buckets, topics and queues;
deploy and delete Cloud Run services; read cluster state; follow live logs; track operations
with their causes. Each of those was driven from a browser and cross-checked outside it.

**It is not verified as visually matching the GCP console**, and this document does not claim
it is. That claim needs reference screens that do not exist.
