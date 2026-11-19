# gh-sudo

A [gh](https://cli.github.com/) extension that runs **one** gh command with temporarily elevated OAuth scopes,
and always puts your original token back afterwards.

```sh
gh sudo -s admin:org -- api -X PUT /orgs/my-org/actions/permissions --input body.json
gh sudo -s delete_repo -- repo delete owner/repo
```

## Why

Some operations need strong classic OAuth scopes such as `admin:enterprise`, `admin:org` or `delete_repo`. Fine-grained
tokens and GitHub Apps cannot do everything; for example, enterprise-level Actions policies need `admin:enterprise`.

The usual workaround is:

```sh
gh auth refresh -s admin:enterprise   # browser approval
gh api ...
gh auth refresh -r admin:enterprise   # browser approval again
```

That is two browser round trips, and it is easy to forget the second one and keep working with the strong token. Worse,
the second step does not do what it looks like; see [Important facts](#important-facts).

`gh sudo` does the same thing with one approval and no cleanup to forget:

1. it saves your current token (in memory only),
2. adds the scopes with `gh auth refresh` (one browser approval; gh prints the URL and the one-time code, open the URL
   yourself),
3. runs your command, and
4. writes the original token back with `gh auth login --with-token` and verifies it, also when the command fails, on
   Ctrl-C, `SIGTERM` or `SIGHUP`, and on a panic.

## Important facts

These are easy to get wrong and they shape what `gh sudo` can and cannot do. Each item says whether it is documented by
GitHub or was observed in our own experiment (gh 2.104.0, github.com, 2026-11-18). The experiment is described in more
detail in the [design doc](docs/design.md#11-observed-facts-gh-21040-2026-11-18).

1. **`gh auth refresh` issues a new token every time and does not revoke the old one** (observed).
   This holds both when adding scopes with `-s` and when removing them with `-r`. In the experiment, after removing
   `admin:enterprise` with `-r`, the token that had `admin:enterprise` still returned 200 for `GET /user` and still
   listed the scope in `X-OAuth-Scopes`. `-r` only changes which token your local keyring holds.
2. **gh's OAuth tokens (`gho_`) do not expire** (documented). They are revoked only when
   - they have not been used for one year,
   - more than ten tokens exist for the same user, app and scope combination (the oldest are revoked first), or
   - the authorization is revoked.

   Sources: [Token expiration and revocation](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/token-expiration-and-revocation),
   [Authorizing OAuth apps](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps)
3. **Submitting a single `gho_` token to
   [`POST /credentials/revoke`](https://docs.github.com/en/rest/credentials/revoke) revokes the whole GitHub CLI OAuth
   authorization** (observed; not documented). Not only the submitted token but also the token you use every day stopped
   working, and `gh auth login` was needed again. The notification email listed the scopes accumulated by the
   authorization, not just those of the submitted token.
4. **Revoking the GitHub CLI authorization also deletes deploy keys created with its tokens** (documented). This
   applies to keys added through the API, for example with `gh repo deploy-key add`, and to any revocation of the
   authorization, including sending a `gho_` token to `POST /credentials/revoke`. In the experiment, a deploy key
   created this way disappeared, and a deployment that depended on it failed the next day. Register long-lived deploy
   keys from the web UI instead.

   Source: [REST API endpoints for deploy keys](https://docs.github.com/en/rest/deploy-keys/deploy-keys)
5. **Therefore `gh sudo` removes the elevated token from your machine, but not from GitHub**, just like
   `gh auth refresh -r`. If you need it gone on GitHub for certain, revoke GitHub CLI under GitHub Settings →
   Applications → Authorized OAuth Apps, then run `gh auth login` again. This also revokes every other gh token for that
   account and deletes the deploy keys described in item 4.

## Install

```sh
gh extension install kenchan/gh-sudo
```

## Usage

```
gh sudo -s <scopes> [flags] -- <gh command>

  -s, --scopes     Comma separated scopes to add for this command (required)
  -h, --hostname   The GitHub host (default: the default host of gh)
  -u, --user       Expected active account; gh sudo fails if it is not active
  -c, --clipboard  Copy the one-time device code to the clipboard
      --help       Show help
```

Everything after `--` is passed to gh unchanged, with `GH_HOST` set to the elevated host. The exit status is the exit
status of the command, except that a failed restore never exits with 0, and a command killed by a signal exits with
`128 + signal`.

`gh sudo` refuses to run when:

- `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN` or `GITHUB_ENTERPRISE_TOKEN` is set, because gh would use it
  instead of the keyring;
- `--user` is not the active account. `gh auth refresh` only works on the active account; switch first with
  `gh auth switch`;
- the token is not stored in the system keyring, or is an expiring token (restoring it would drop its refresh token);
- the command is a `gh auth` command (aliases and extensions that run `gh auth` are not detected; do not wrap them);
- another `gh sudo` is already running for the same host.

## Limitations

- **The elevated token stays valid on GitHub** (see [Important facts](#important-facts)). Do **not** try to revoke it
  with `POST /credentials/revoke`.
- **Other processes see the elevated token while the command runs.** Any gh invoked on the same machine during that
  time (for example by another terminal or an agent) uses it.
- **The GitHub CLI authorization keeps the added scope**, so a later `gh auth refresh` may carry it forward.
- **After Ctrl-C, wait for the `restored the original token` line.** gh does not wait for extensions on Ctrl-C, so
  your shell prompt may come back while `gh sudo` is still restoring.
- If the account already has every requested scope, `gh sudo` runs the command without refreshing.
- Signal handling is verified on macOS and Linux only. On Windows, signals are not forwarded to the command.
- `SIGKILL` and power loss cannot be handled. If `gh sudo` ever reports that it failed to restore the token, run
  `gh auth login -h <host>`.

A mode that uses a dedicated OAuth App and revokes the elevated token on GitHub is described as future work in the
[design doc](docs/design.md#6-mode-b-dedicated-oauth-app-with-device-flow-future-work).

## Development

```sh
go test ./...
```

See [docs/design.md](docs/design.md) for the design and [docs/manual-test.md](docs/manual-test.md) for the manual test
that covers the browser approval.

Pull requests other than the release pull request are squash-merged automatically once CI passes.

## Releasing

Releases are automated with [tagpr](https://github.com/Songmu/tagpr):

1. Every push to `main`, and a daily run at 12:07 JST, creates or updates a release pull request that adds a
   `CHANGELOG.md` entry generated from the merged pull requests, and runs CI on it. The daily run picks up pull requests
   merged by auto-merge, which do not start the workflow by themselves. Run the `tagpr` workflow manually with no tag
   to update it right away. Label it `tagpr:minor` or `tagpr:major` for a bigger bump than a patch
   release.
2. Merging the release pull request tags the commit.
3. The same workflow then builds the binaries for that tag and creates the GitHub release with them.

If building fails after the tag was created, run the `tagpr` workflow manually with the tag to publish it again.

The release flow depends on settings that live outside the repository. Recreate them if the repository changes hands:

- Settings → Actions → General → "Allow GitHub Actions to create and approve pull requests" is enabled.
- Settings → General → "Allow auto-merge" and "Automatically delete head branches" are enabled.
- A ruleset on the default branch requires the CI checks, with a bypass for repository admins.

Pull requests that change only the development setup (workflows, docs including this README, tests, lint and tagpr
configuration) are labeled `skip-changelog` automatically and left out of the release notes. Add the label by hand to
leave out any other pull request.

Release notes are generated from merged pull requests, so land changes through pull requests rather than direct pushes
to `main`.

## License

[MIT](LICENSE)
