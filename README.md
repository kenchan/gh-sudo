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
the second step does not do what it looks like: `gh auth refresh` issues a new token every time and does **not**
revoke the old one, so the elevated token stays valid on GitHub either way. Removing the scope only changes which token
your keyring holds.

`gh sudo` does the same thing with one approval and no cleanup to forget:

1. it saves your current token (in memory only),
2. adds the scopes with `gh auth refresh` (one browser approval; gh prints the URL and the one-time code, open the URL
   yourself),
3. runs your command, and
4. writes the original token back with `gh auth login --with-token` and verifies it, also when the command fails, on
   Ctrl-C, `SIGTERM` or `SIGHUP`, and on a panic.

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

Everything after `--` is passed to gh unchanged. The exit status is the exit status of the command.

`gh sudo` refuses to run when:

- `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN` or `GITHUB_ENTERPRISE_TOKEN` is set, because gh would use it
  instead of the keyring;
- `--user` is not the active account. `gh auth refresh` only works on the active account; switch first with
  `gh auth switch`;
- the token is not stored in the system keyring, or is an expiring token (restoring it would drop its refresh token);
- the command is a `gh auth` command;
- another `gh sudo` is already running for the same host.

## Limitations

- **The elevated token stays valid on GitHub.** `gh sudo` only changes your local keyring, exactly like
  `gh auth refresh -r`. GitHub's OAuth tokens for gh do not expire; unused tokens are revoked after one year, and only
  the ten newest tokens per user/app/scope combination are kept. Do **not** try to revoke it with
  `POST /credentials/revoke`: that revokes the whole GitHub CLI authorization, including the token you use every day.
- **Other processes see the elevated token while the command runs.** Any gh invoked on the same machine during that
  time (for example by another terminal or an agent) uses it.
- **The GitHub CLI authorization keeps the added scope**, so a later `gh auth refresh` may carry it forward.
- **After Ctrl-C, wait for the `restored the original token` line.** gh does not wait for extensions on Ctrl-C, so
  your shell prompt may come back while `gh sudo` is still restoring.
- If the account already has every requested scope, `gh sudo` runs the command without refreshing.
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

## License

[MIT](LICENSE)
