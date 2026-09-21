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

### The four states

| State | Evidence |
|---|---|
| Empty | Above: named, offers create, **zero rows** |
| Loading | Skeleton rendered before data arrives |
| Error | The pubsub tunnel was killed mid-session; the console reported `{"unavailable":"list topics: context deadline exceeded"}` rather than an empty table |
| Operating | Every mutation appears in the notifications panel and carries its terminal state |

The error case is the one that matters: an empty table says *"you have none"* and sends a
developer to debug their own code.

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

A regression test now compiles every shipped pattern's character classes under the `v` rule,
checks each default satisfies its own pattern, and checks every required field explains its
constraint. Reverting the fix makes it fail.

### A test that failed for the wrong reason

`TestUpFailsOnOccupiedControlPort` pinned only `--port-control` and left every other port at
its default, so an unrelated process holding one of them made the test fail **naming the
wrong port**. Every other port is now OS-assigned.

---

## 3. What was not verified, and why

### Visual regression against reference screenshots

**Not done, and not possible today.** [#43](https://github.com/identity-wael/cloudburrow/issues/43)
established that no authorized read-only console session was available, so **no dated
reference screenshots exist**. Without a reference there is nothing to diff against, and a
threshold measured against a screenshot of our own output would only prove the console still
looks like itself.

What was verified is **structural**: page names, navigation paths, control labels, the four
states, table semantics and the documented theme behaviour. Pixel parity — spacing, type
scale, palette, row heights — remains **unverified and unclaimed**, exactly as the parity
specification says.

This is the gap that keeps console visual fidelity at `Partial` in the support matrix.
Closing it needs reference screens, not more work on the build.

### Restart and reset

Not re-run as part of this walkthrough. Resource durability across restart is covered by the
service-level tests and by the mode semantics in [configuration.md](configuration.md); the
console holds no state of its own, so there is nothing additional in it to survive a restart.

### Subscriptions in the UI

The acceptance criterion asks for a subscription created from the browser. The console does
not offer subscription creation — [#45](https://github.com/identity-wael/cloudburrow/issues/45)
shipped topics only, and recorded that. A UI flow for it does not exist, so it was not walked.

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
