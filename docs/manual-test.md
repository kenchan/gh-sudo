# Manual test

The browser approval of the device flow cannot be automated, so this test is run by hand. It only uses a harmless
read-only command and a low-risk scope (`read:user`).

Never run this test against an organization or enterprise you do not own.

## Setup

Build the extension from the working tree and install it as a local extension:

```sh
cd path/to/gh-sudo
go build -o gh-sudo .
gh extension install .
```

Make sure no token environment variable is set:

```sh
env | grep -E '^(GH|GITHUB)(_ENTERPRISE)?_TOKEN=' || echo ok
```

Record the current state without printing the token:

```sh
gh auth status
gh auth token | shasum -a 256 > /tmp/gh-sudo-before.sha256
```

## 1. Normal run

```sh
gh sudo -s read:user -- api -i user | grep -i '^x-oauth-scopes'
```

Approve the device code in the browser with the **active** account.

Expected:

- `X-OAuth-Scopes` includes `read:user`.
- `gh sudo` prints `restored the original token for <user> on github.com`.

## 2. Cancel before approving

```sh
gh sudo -s read:user -- api user
```

Press Ctrl-C while gh waits for the browser approval.

Expected: `gh sudo` exits with a non-zero status and prints `restored the original token ...`. `api user` does not run.

## 3. Failing command

```sh
gh sudo -s read:user -- api /this/does/not/exist; echo "exit=$?"
```

Approve in the browser.

Expected: gh prints a 404 error, `exit=1`, and `gh sudo` prints `restored the original token ...`.

## Verify after each step

```sh
gh auth token | shasum -a 256 | diff - /tmp/gh-sudo-before.sha256 && echo "token restored"
gh auth status
```

Expected:

- `token restored`.
- `gh auth status` shows the same active account, token source (`keyring`) and scopes as before.

## Cleanup

```sh
gh extension remove sudo
rm /tmp/gh-sudo-before.sha256
```
