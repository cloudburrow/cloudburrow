# ssm-templates: Secure Source Manager's repository initialization templates

Secure Source Manager's `CreateRepository` takes an `initial_config` whose `gitignores` and
`license` name templates (#2071): the `InitialConfig` reference lists 230 .gitignore names and
459 licence names, and Google's "Create a repository" page offers them in the web interface's
drop-down menus. Google publishes neither the templates' text nor its default README, so
CloudBurrow's are an UNVERIFIED stand-in built from two openly licensed sources:

| Input | Version | Source (official) | SHA-256 of the archive | Licence |
|---|---|---|---|---|
| github/gitignore | commit `b71db924d35085e481ad7c7ad8a7435b8d8b3016` | https://github.com/github/gitignore (`/archive/b71db924d35085e481ad7c7ad8a7435b8d8b3016.tar.gz`) | `0a4bb4abd684b961c0dd0da0b4615f4986eabf2399217fd1a3c56512f3d4d667` | CC0-1.0 (`LICENSE-gitignore`, copied here from the archive's `LICENSE`) |
| spdx/license-list-data | v3.29.0, commit `ab9b0904b43db6f4b67431efd8821feb821f1c8b` | https://github.com/spdx/license-list-data (`/archive/ab9b0904b43db6f4b67431efd8821feb821f1c8b.tar.gz`), its `text/` folder | `98380b0a6813ce1f792dbd790f2e8ae06f77701e98d9c3504791b94c3cd48a8f` | The SPDX License List is CC-BY-3.0 (https://spdx.org/licenses/, "SPDX License List" by the Linux Foundation and its contributors); each licence text is the licence document itself, which its own terms let anyone copy verbatim |

Only data derived from them is in the repository, no code:
`internal/adapter/run/ssmtemplates.tar.gz` (SHA-256
`95eaa76dc8e9df2929055ca074df6e619ec593494c0c852ffe9b2bd9233b404b`, which
`TestSourceManagerTemplatesDigest` checks), the output of `tools/ssmtemplates/generate.py`
(CloudBurrow's own script) run on the two archives above, extracted:

    python3 -I tools/ssmtemplates/generate.py \
        $(go env GOMODCACHE)/cloud.google.com/go/securesourcemanager@v1.12.0/apiv1/securesourcemanagerpb/secure_source_manager.pb.go \
        <scratch>/gitignore-b71db924d35085e481ad7c7ad8a7435b8d8b3016 \
        <scratch>/license-list-data-ab9b0904b43db6f4b67431efd8821feb821f1c8b/text \
        internal/adapter/run/ssmtemplates.tar.gz

The archive holds `gitignore/NAME` and `license/NAME` for every name the reference lists. The
licence texts are licence documents, GPL and AGPL among them, written into a user's repository
when the user picks them: they are text, and no code under those licences is in CloudBurrow.
How each name maps to its file (kebab case, the exceptions for names github/gitignore spells
otherwise, SPDX's deprecated IDs) is documented in the script.
