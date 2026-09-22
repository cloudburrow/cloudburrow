# Google Cloud product icons

These are **Google's own published product icons**, not CloudBurrow artwork.

## Where they came from

Downloaded on 2026-09-22 from the sets Google publishes at
<https://cloud.google.com/icons>:

| Set | URL |
|---|---|
| Legacy console icons | `https://services.google.com/fh/files/misc/google-cloud-legacy-icons.zip` |
| Core product icons | `https://services.google.com/fh/files/misc/core-products-icons.zip` |
| Product category icons | `https://services.google.com/fh/files/misc/category-icons.zip` |

`categories/` holds the second set. Those are Google's **product categories**,
and the archive is also where the category *names* the navigation groups under
come from — Serverless computing, Containers, Databases and the rest are
Google's taxonomy rather than headings invented here.

The files are copied unmodified and renamed to the console screen that shows
them, so `run.svg` is Google's `cloud_run.svg`. The mapping is one product to
possibly several screens: every Kubernetes Engine page carries the GKE icon and
both Vertex AI screens carry the Vertex AI icon, as the real console does.

## Terms

**Neither download contains a licence or terms file**, and the icons page does
not state redistribution terms inline. That was checked rather than assumed:
both archives were listed and no `LICENSE`, `NOTICE` or `TERMS` entry exists in
either.

So the position here is stated plainly rather than implied:

- The icons are **Google's trademarks and artwork**. CloudBurrow claims no
  ownership of them and applies no licence of its own to them.
- They are used to **identify the Google Cloud product each screen emulates**,
  which is the whole purpose of this console. Nothing here is Google software,
  Google-published, or endorsed by Google.
- The console is branded **CloudBurrow** and carries a permanent **LOCAL**
  badge at every viewport, which exists so that nobody can mistake it for the
  Google Cloud console.
- They are **unmodified**. A modified logo would misrepresent a mark that is
  not ours to change.

If Google's terms for these assets turn out to forbid this use, the fix is one
directory: delete `internal/console/assets/icons/` and the navigation falls
back to the line drawings in `console.js`, which are CloudBurrow's own work.
That fallback is deliberate and is kept working for exactly this reason.
