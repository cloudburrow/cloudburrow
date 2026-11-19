# API Keys

CloudBurrow serves the API Keys API v2 (`apikeys.googleapis.com`, #1915): each project's API keys
and their restrictions. `gcloud services api-keys`, Terraform's `google_apikeys_key` and the
console's **APIs & Services > Credentials** call it.

It runs in the CLI process on `--port-apikeys` (default `9028`, moved by `--port-base`), always, as
Service Usage does, JSON only. `cloudburrow env` exports `CLOUDBURROW_APIKEYS_ENDPOINT` and
`CLOUDSDK_API_ENDPOINT_OVERRIDES_APIKEYS`, `cloudburrow gcloud-setup` writes the `apikeys` override,
and `cloudburrow terraform` sets the provider's `apikeys_custom_endpoint`. No client library reads
the variable: pass `http://<it>/` to `option.WithEndpoint` (`google.golang.org/api/apikeys/v2`, or
the REST client of `cloud.google.com/go/apikeys/apiv2`). The gRPC surface is not served.

## Keys

A key is `projects/{number}/locations/global/keys/{keyId}`; keys are global, and any other location
is refused. `keyId` follows Google's rules (RFC 1034: lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen, at most 63 characters, not UUID-like); without
one the ID is a UUID. The key string has Google's form, `AIza` and 35 more characters. It is in the
create's operation and in `getKeyString`'s answer, never in `get` or `list`.

Keys are stored in the project registry's store, so they persist exactly as the instance's
projects do (in memory for an ephemeral instance, in the state directory for a persistent one, and
in state snapshots). `/admin/reset` forgets them, and so does purging a project.

Every change happens before the call returns, so `create`, `patch`, `delete` and `undelete` return a
done operation (`operations/akmf.p7-…`), which `operations.get` answers for the life of the process.

`patch` changes `displayName`, `restrictions` and `annotations` under `updateMask` (`*` for all of
them; with no mask, every one of them set in the request); any other path is refused. An `etag` that
does not match the key's is refused (409 ABORTED).

`delete` sets `deleteTime`: the key stops authorizing calls and is listed only with `showDeleted`.
`undelete` restores it for 30 days, as Google documents; after that it is purged, `get` is 404 and
`keys.lookupKey` answers its project with an empty name.

## Restrictions

Validated as Google documents them:

- **One application restriction kind**: `browserKeyRestrictions`, `serverKeyRestrictions`,
  `androidKeyRestrictions` or `iosKeyRestrictions`, at most 1,200 entries.
- **Websites**: a URL with an optional scheme and port, where `*` can stand for a subdomain (a
  leading `*.`) or the rest of a path, not for part of the middle of a URL. `example.com/*` matches
  any page of `example.com`, `*.example.com/*` any page of its subdomains, and `www.example.com` only
  its root, so allowing a whole site takes both forms, as Google's guidance says.
- **IP addresses**: IPv4 or IPv6 addresses, or CIDR subnets.
- **Android apps**: a package name and a 20-byte SHA-1 certificate fingerprint, with or without
  colons (answered in plain hex). Requests carry them in `X-Android-Package` and `X-Android-Cert`.
- **iOS apps**: bundle IDs, carried in `X-Ios-Bundle-Identifier`.
- **API restrictions** (`apiTargets`): a service, and optionally methods, where `*` may only be the
  last character. Both are case-insensitive.

A project holds at most 300 keys, Google's system limit. Google documents that internal addresses
and `localhost` are not supported in IP restrictions; locally every caller is on loopback, so they
are accepted and matched, which is the only way to test a server key here.

## Using a key

A request to a served API that carries a key (`key=` or `x-goog-api-key`) is decided by
`Authorize` (`internal/service/apikeys/check.go`), with Google's errors and a `google.rpc.ErrorInfo`
(domain `googleapis.com`, metadata `service` and, for a found key, `consumer`), as
`google/api/error_reason.proto` documents them:

| Case | Status | Reason | Message |
|---|---|---|---|
| No live key has the string (malformed, never made, deleted or purged) | 400 INVALID_ARGUMENT | `API_KEY_INVALID` | `API key not valid. Please pass a valid API key.` |
| The website restriction refuses the request's `Referer` | 403 PERMISSION_DENIED | `API_KEY_HTTP_REFERRER_BLOCKED` | `Requests from referer <referrer> are blocked.` (`<empty>` for none) |
| The IP restriction refuses the caller | 403 PERMISSION_DENIED | `API_KEY_IP_ADDRESS_BLOCKED` | `The provided API key has an IP address restriction. The originating IP address of the call (<ip>) violates this restriction.` |
| The Android restriction refuses the app | 403 PERMISSION_DENIED | `API_KEY_ANDROID_APP_BLOCKED` | `Requests from this Android client application <package> are blocked.` |
| The iOS restriction refuses the app | 403 PERMISSION_DENIED | `API_KEY_IOS_APP_BLOCKED` | `Requests from this iOS client application <bundle> are blocked.` |
| The API restrictions do not name the API or method | 403 PERMISSION_DENIED | `API_KEY_SERVICE_BLOCKED` | `Requests to this API <service> method <method> are blocked.` |

Application restrictions are checked before API restrictions. Wiring the decision into each served
API's request path is the second half of #1915.

## Console

**APIs & Services > Credentials** lists the project's keys (Name, Creation date, Restrictions, and
Show key, Edit API key and Delete API key on each). **Create credentials > API key** makes a key and
shows it in Google's "API key created" dialog. A key's page (`/apis/credentials/key/<id>`) edits its
name, application restrictions and API restrictions (the enabled APIs), and offers **Show key**,
**Rotate key** and **Delete**. Rotate key is the console's alone, as on Google: it creates a new key
with the same restrictions, and the old key keeps working until **Delete the previous key** in the
new key's Previous key section. **Restore deleted credentials** (`/apis/credentials/restore`)
restores a key deleted in the last 30 days.

Per-method status is generated in [coverage/apikeys.md](coverage/apikeys.md); the claims are in
[compatibility.md](compatibility.md#api-keys--apikeysgoogleapiscom-v2).
