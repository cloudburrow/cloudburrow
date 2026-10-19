# Release signing setup (macOS)

The release workflow's `sign-macos` job signs both darwin binaries with a Developer ID Application
certificate, notarizes them with Apple and re-packages the archives before `checksums.txt`, the
attestations and the Homebrew formula are computed (#605). It does this only once the five
repository secrets below exist. Until then every release ships unsigned darwin binaries, with a
notice in the run. What users see, and how they check a binary, is in
[Gatekeeper](install.md#gatekeeper).

The maintainer does every step here by hand. No automation creates, reads or handles the
certificate, the key or their passwords, and the workflow reads them only as `${{ secrets.* }}`,
inside a temporary keychain it deletes at the end of the job, even when the job fails.

## What you need

- A paid **Apple Developer Program** membership.
- The **Account Holder** or **Admin** role on that team. Other roles cannot create a Developer ID
  certificate.
- Xcode signed in to that team (Xcode › Settings › Accounts).
- The GitHub CLI, signed in with admin rights on `cloudburrow/cloudburrow`.

## 1. Create the Developer ID Application certificate

In Xcode: **Settings › Accounts**, then select the team and choose **Manage Certificates…**, then
**+ › Developer ID Application**. Xcode creates the private key in your login keychain and the
certificate with it.

Check that it is there. This prints identity names only, never a key:

```sh
security find-identity -v -p codesigning
# 1) <hash> "Developer ID Application: Your Name (TEAMID)"
```

An **Apple Development** identity is not enough: it signs for development only, and Gatekeeper
rejects binaries signed with it.

## 2. Export it as a .p12

In **Keychain Access**, open the **login** keychain's **My Certificates**, select
`Developer ID Application: Your Name (TEAMID)` and expand it to check that the private key is
under it. Then choose **File › Export Items…**, pick the **Personal Information Exchange (.p12)**
format and set a strong export password. Xcode's Manage Certificates sheet can do the same:
Control-click the certificate and choose **Export Certificate**.

The .p12 holds the private key. Keep it out of the repository and out of any synced folder, and
delete it after step 4 (keep a copy only in a password manager).

## 3. Create an App Store Connect API key for notarization

In [App Store Connect](https://appstoreconnect.apple.com/): **Users and Access › Integrations ›
App Store Connect API › Team Keys**, then **+**. Give it a name such as `cloudburrow-notary` and
the **Developer** access role. Then:

- download `AuthKey_<KEYID>.p8`. Apple lets you download it **once only**;
- note the **Key ID** shown in the key's row;
- note the **Issuer ID** shown above the table.

## 4. Add the repository secrets

Run these yourself, from the directory holding the two files. When a command has no piped
input, `gh secret set` prompts for the value without echoing it, so the value stays out of your
shell history.

```sh
R=cloudburrow/cloudburrow
base64 -i DeveloperID.p12 | gh secret set MACOS_CERTIFICATE_P12_BASE64 -R "$R"
gh secret set MACOS_CERTIFICATE_PASSWORD -R "$R"        # paste the .p12 export password
base64 -i AuthKey_KEYID.p8 | gh secret set NOTARY_KEY_P8_BASE64 -R "$R"
gh secret set NOTARY_KEY_ID -R "$R"                     # the Key ID
gh secret set NOTARY_ISSUER_ID -R "$R"                  # the Issuer ID
```

| Secret | Value |
|---|---|
| `MACOS_CERTIFICATE_P12_BASE64` | The Developer ID Application certificate and its private key, as `.p12`, base64-encoded |
| `MACOS_CERTIFICATE_PASSWORD` | The `.p12` export password |
| `NOTARY_KEY_P8_BASE64` | The App Store Connect API key (`AuthKey_<KEYID>.p8`), base64-encoded |
| `NOTARY_KEY_ID` | That key's Key ID |
| `NOTARY_ISSUER_ID` | The App Store Connect Issuer ID |

You need all five or none. The job fails, and names what is missing, if only some are set,
rather than silently shipping unsigned binaries.

**The identity name is not a secret.** The job signs with the one Developer ID Application
identity in the .p12. Only if the .p12 holds more than one, set which to use as a repository
*variable*:

```sh
gh variable set MACOS_SIGNING_IDENTITY -R "$R" --body "Developer ID Application: Your Name (TEAMID)"
```

**If you set the earlier names.** The first version of the job (#654) used
`MACOS_SIGNING_CERT_P12_BASE64`, `MACOS_SIGNING_CERT_PASSWORD`, `MACOS_SIGNING_IDENTITY`,
`APPLE_NOTARY_KEY_P8_BASE64`, `APPLE_NOTARY_KEY_ID` and `APPLE_NOTARY_ISSUER_ID`. The job now
fails while the old certificate or key secret is still set. Set the five above, then remove the old ones with
`gh secret delete <name> -R "$R"`.

Then delete the local `.p12` and `.p8` files.

## 5. Check it before a tag

A manual dispatch of the release workflow signs and notarizes but publishes nothing. It is held
to the same CI gate as a tag, so dispatch it on a commit where CI passed:

```sh
gh workflow run release.yml -R "$R" --ref main
```

In the run, the **sign and notarize darwin** job prints `Signing as: Developer ID Application: …`,
then `codesign -dv` output with `Authority=Developer ID Application`, a `Timestamp=` line and the
`runtime` flag, then the notary service's `Accepted`. The job summary has a **macOS signing**
section. The signed archives are the run's `cloudburrow-darwin-*` artifacts.

On the first tagged release, check a downloaded darwin binary as
[Gatekeeper](install.md#gatekeeper) describes (`codesign -dv --verbose=2`, `spctl -a -t exec -vv`,
`gh attestation verify`). That is the acceptance for #605.

## Optional: local notarization

To notarize by hand, or let a local check do it without the key going anywhere else, store the
key once in your login keychain under a profile name:

```sh
xcrun notarytool store-credentials cloudburrow-notary \
  --key AuthKey_KEYID.p8 --key-id <Key ID> --issuer <Issuer ID>
```

`xcrun notarytool history --keychain-profile cloudburrow-notary` then works without the key
being printed or passed on the command line again.

## Renewal

A Developer ID Application certificate is valid for five years. Binaries signed with a secure
timestamp stay valid after it expires, so only new releases need the renewed certificate: repeat
steps 1, 2 and 4. If you revoke the API key, repeat steps 3 and 4.
