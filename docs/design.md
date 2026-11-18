# gh-sudo design

Status: Mode A implemented (MVP). Mode B is future work.

## 1. Background

Some `gh` operations need strong OAuth scopes such as `admin:enterprise`,
`admin:org` or `delete_repo`. The usual workflow is:

1. `gh auth refresh -s admin:enterprise` (approve a device code in the browser)
2. do the work
3. `gh auth refresh -r admin:enterprise` (approve again in the browser)

This costs two browser round trips, and step 3 gives almost no security
benefit, as the experiments below show.

### 1.1 Observed facts (gh 2.104.0, 2026-11-18)

- **Every refresh issues a new token, and old tokens are not revoked.**
  The tokens before adding a scope (T0), after adding it (T1) and after
  removing it (T2) were all different, and all three still returned 200 for
  `GET /user` afterwards. T1 kept `admin:enterprise` in `X-OAuth-Scopes`.
  `refresh -r` only changes which token is stored in the local keyring.
- **gh OAuth tokens (`gho_`) do not expire.** They are revoked automatically
  after one year without use, or when more than ten tokens exist for the same
  user/app/scope combination.
  ([token expiration](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/token-expiration-and-revocation),
  [authorizing OAuth apps](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps))
- **`POST /credentials/revoke` cannot revoke a single `gho_` token.**
  Submitting one revokes the whole GitHub CLI OAuth authorization, including
  the token in everyday use. Recovering requires a fresh `gh auth login`.
  The notification email lists the cumulative scopes of the authorization.
  This behavior is not documented.
- **The original token can be written back without a browser** with
  `gh auth login -h <host> --with-token` (token on stdin).
- **A separate `GH_CONFIG_DIR` does not isolate credentials.** Keyring entries
  are keyed by `gh:<host>` and user name, not by config dir
  (`keyringServiceName` in cli/cli `internal/config/config.go`), so logging in
  from another config dir overwrites the everyday token.
- **Fine-grained PATs and GitHub Apps cannot change enterprise-level Actions
  policies**; classic `admin:enterprise` is required.
- **No existing tool** (gh extension or cli/cli feature) provides temporary
  scope elevation (searched 2026-11-18).

### 1.2 Facts from reading the gh source (cli/cli v2.104.0)

- `gh auth refresh` only refreshes the **active** account of a host. It
  refuses credentials approved in the browser by a different user
  ("received credentials for ...").
- `gh auth login --with-token` validates the token over the network
  (minimum scopes and `GET /user`), stores it for that user and **makes that
  user active**.
- `gh auth login --with-token` removes any refreshable (expiring) credential
  of that user. Writing back an expiring token this way would lose its
  refresh token.
- When it can prompt, `gh auth refresh` asks "Authenticate Git with your GitHub credentials?" if the git credential
  helper is not gh itself, and on "yes" hands the new token to that helper with `git credential approve` (for example
  storing it in osxkeychain). With `GH_PROMPT_DISABLED` set, it skips that step; the device flow still works, but gh
  prints the URL instead of opening the browser.
- When gh runs an extension it does not catch `SIGINT`, so on Ctrl-C the outer `gh` exits at once while the
  extension keeps running.
- `gh auth status --json hosts --no-refresh` reports, per account, `active`,
  `login`, `tokenSource` and `refreshable`, without printing the token.

## 2. Goals

- Use a strong scope for **exactly one command**, with **one browser
  approval**.
- Afterwards the local gh **always** holds the original token again, also
  when the command fails, on Ctrl-C/SIGTERM/SIGHUP, and on panic.
- Never write a token to stdout, stderr, files, argv or logs.
- Ideally, also revoke the strong token on GitHub's side (Mode B).

## 3. Non-goals

- Long-lived PAT storage (that is a password manager's job).
- Keeping an interactive shell open with elevated scopes.
- GitHub Enterprise Server specifics. `--hostname` is accepted, but only
  github.com is verified.

## 4. Interface

```
gh sudo -s admin:enterprise -- api -X PUT /enterprises/x/actions/permissions/selected-actions --input body.json
gh sudo -s admin:org,delete_repo -- repo delete owner/repo
gh sudo -s admin:org --hostname github.com --user octocat -- api user
```

| Flag | Meaning |
| --- | --- |
| `-s, --scopes` | Required. Comma separated scopes to add for this command. |
| `-h, --hostname` | Host. Defaults to the default host (normally `github.com`). |
| `-u, --user` | Optional. Must be the **active** account of the host (see 5.3). |
| `-c, --clipboard` | Passed to `gh auth refresh`: copy the device code to the clipboard. |

Everything after `--` is passed to `gh` unchanged. The exit status is the
exit status of the wrapped command (see 5.5 for restore failures).

## 5. Mode A: refresh and restore (implemented)

### 5.1 Flow

1. **Preflight.** Fail if `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`
   or `GITHUB_ENTERPRISE_TOKEN` is set: gh would use the environment value
   instead of the keyring, so neither the refresh nor the restore would do
   what we expect.
2. **Inspect the account** with `gh auth status --json hosts -h H --no-refresh`.
   Fail unless the active account
   - matches `--user` (when given),
   - has `tokenSource` `keyring`, and
   - is not `refreshable`.
3. **Take a lock** for the host (see 5.4).
4. **Save T0** with `gh auth token -h H -u U --no-refresh`, kept only in
   memory. From this point on, the restore is armed.
5. **Skip the elevation** if the account already has every requested scope.
   Refreshing anyway would mint another token with the same scope set as T0,
   and GitHub keeps only the ten newest tokens per user/app/scope set, so
   repeated runs could get T0 revoked.
6. **Elevate** with `GH_PROMPT_DISABLED=1 gh auth refresh -h H -s SCOPES`.
   The user approves once in the browser. The keyring now holds T1. Prompts
   are disabled so that gh does not copy T1 into a non-gh git credential
   helper, where the restore could not remove it (1.2).
7. **Run** `gh <args>` with the terminal attached. It reads T1 from the
   keyring.
8. **Restore** (always, see 5.2).
9. **Verify** that `gh auth token -h H -u U --no-refresh` equals T0 and that
   U is still the active account.

### 5.2 Restore guarantees

- The restore is deferred right after T0 is read, so it also runs when the
  refresh fails or is cancelled.
- If the keyring already holds T0 and U is still active, nothing is written.
  This avoids a needless network round trip, for example when the refresh was
  cancelled before the browser approval.
- Otherwise T0 is written back with `gh auth login -h H --with-token`, with
  T0 on stdin. This needs the network, so it is retried a few times with
  backoff.
- The restore uses its own context, not the one that may have been cancelled,
  so cancellation cannot kill the restore itself.
- `SIGINT`, `SIGTERM` and `SIGHUP` are caught for the whole run. A second
  Ctrl-C cannot interrupt the restore. `SIGTERM` and `SIGHUP` are forwarded to
  the running child; `SIGINT` from a terminal already reaches the child
  through the process group.
- A panic is recovered, the restore runs, and the panic is re-raised.
- `main` calls `os.Exit(run())`, and `run` owns all deferred work, so
  `os.Exit` never skips the restore.
- `SIGKILL` and power loss cannot be handled. The recovery steps in 5.5 apply.
- When run as `gh sudo`, Ctrl-C ends the outer `gh` immediately (1.2), so the
  shell prompt may return before the restore finishes. The restore still
  completes and prints a final line to stderr.

### 5.3 Why only the active account

`gh auth refresh` cannot target an inactive account, and
`gh auth login --with-token` makes the restored account active. Supporting
inactive accounts would require `gh sudo` to switch accounts and switch back,
which adds a second piece of global state to restore and changes the active
account for every other process on the machine while the command runs. So
`--user` only selects among accounts as a safety check: it must name the
active account. To elevate another account, switch to it yourself first.

### 5.4 Concurrency

If a second `gh sudo` starts while the first is running, it would read T1 as
its "original" token and restore T1 at the end, leaving the elevated token
in the keyring. To prevent this, `gh sudo` takes an exclusive, non-blocking
file lock per host in the user cache directory
(`<UserCacheDir>/gh-sudo/<host>.lock`). The file contains no secret. A second
instance fails immediately.

The lock does not protect against other tools running `gh auth refresh` or
`gh auth login` at the same time.

### 5.5 Restore failure

If the restore or the verification fails, `gh sudo` prints a prominent
warning to stderr that the keyring may still hold the elevated token, and the
recovery steps:

```
gh auth login -h <host>
```

The exit status is the wrapped command's status if it was non-zero, and `1`
otherwise, so a failed restore is never reported as success.

### 5.6 Limitations of Mode A

- **T1 stays valid on GitHub.** Mode A changes only the local keyring, just
  like `gh auth refresh -r`. The elevated token does not expire and is not
  revoked. It must not be revoked with `POST /credentials/revoke`, because
  that would revoke the whole GitHub CLI authorization (1.1).
- **Other processes see T1 while the command runs.** Any `gh` invoked on the
  same machine during step 6 (for example by another agent) uses T1.
- **The GitHub CLI authorization keeps the added scope.** Later
  `gh auth refresh` runs may carry it forward.

### 5.7 Secret handling

- Tokens are held in a type whose `String`, `GoString` and `Format` methods
  redact the value, so they cannot leak through `fmt` or panic messages.
- Tokens are passed to child processes only on stdin (`--with-token`) and
  read only from stdout (`gh auth token`). They never appear in argv.
- Tests assert that the token never appears in recorded argv or output.

## 6. Mode B: dedicated OAuth App with device flow (future work)

Not implemented. No OAuth App is registered yet; this was decided on
2026-11-18.

1. Use the client ID of a dedicated gh-sudo OAuth App with device flow
   enabled (no client secret is needed). Run the device flow ourselves to get
   T1, and never write it to the keyring.
2. Run `GH_TOKEN=T1 GH_HOST=H gh <args>`. T1 is passed only through the child
   environment. The everyday keyring is never touched.
3. Always revoke T1 with an unauthenticated
   `POST /credentials/revoke` (`{"credentials":["T1"]}`). This revokes the
   gh-sudo App authorization, not the GitHub CLI one.
4. Poll `GET /user` until it returns 401, because revocation is
   asynchronous.

Advantages: the everyday gh is never touched, the strong token is revoked on
GitHub, and no other process can use it. Disadvantages: an OAuth App must be
registered, every revocation sends a notification email, and orgs with SAML
SSO or OAuth App access restrictions may need extra approval.

Open questions for Mode B:

- **B-1**: Revoking a gh-sudo App token must not revoke the GitHub CLI
  authorization. To be tested on a personal account only.
- **B-2**: Whether OAuth App tokens work against SAML SSO orgs and enterprise
  endpoints, given OAuth App access restrictions.
- **B-3**: How closely to mimic gh's device flow UX (clipboard, opening the
  browser).
- **B-4**: Because each revoke removes the authorization, every run shows the
  App authorization screen again. Is that acceptable?
- Whether to ship a default client ID in the binary or require
  `--client-id` / an environment variable.

## 7. Testing

- Unit tests replace the gh invocations with a fake that models the keyring
  and the active account, and verify that the restore runs when the command
  fails, when the refresh fails, on signals and on panic, that it retries
  transient failures, that verification catches a wrong token, and that the
  token never appears in argv or output.
- The device flow cannot be automated. The manual test procedure is in
  [manual-test.md](manual-test.md).

## 8. CI and release

- GitHub Actions runs `go test`, `go vet` and golangci-lint, plus actionlint
  and zizmor for the workflows.
- Releases use `cli/gh-extension-precompile`.
- Actions are pinned by commit SHA with the version in a comment, and
  `permissions` are minimal.
- License: MIT.
