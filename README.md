# isnipe

Retries `StartInstances` on a stopped EC2 instance until AWS frees up capacity
(`InsufficientInstanceCapacity`), then tells you the moment it's yours.

A single self-contained binary for macOS, Linux, and Windows. It talks to the
EC2 API directly, so there's **nothing to install at runtime** — not even the
AWS CLI. (If the AWS CLI happens to be installed, isnipe can use it to walk you
through first-time credential setup; it's never required.)

> **Note:** prebuilt binaries and `go install …@latest` become available once a
> `v2.x` release is tagged. From an unreleased checkout, use **Build from
> source** below (or `go install …@main`).

## Install

### `go install` (any OS with Go ≥ 1.21)

```bash
go install github.com/nmud/aws-instance-sniper/cmd/isnipe@latest
```

Installs `isnipe` into `$(go env GOPATH)/bin` — make sure that's on your `PATH`.
Go auto-downloads whatever toolchain the build needs.

### Prebuilt binary

Download the binary for your OS/arch from the
[Releases](https://github.com/nmud/aws-instance-sniper/releases) page, then:

**macOS / Linux**

```bash
chmod +x isnipe-*
sudo mv isnipe-<os>-<arch> /usr/local/bin/isnipe
# macOS only, if Gatekeeper blocks the unsigned binary:
xattr -d com.apple.quarantine /usr/local/bin/isnipe
```

**Windows** (PowerShell)

```powershell
New-Item -ItemType Directory -Force "$HOME\bin" | Out-Null
Move-Item .\isnipe-windows-amd64.exe "$HOME\bin\isnipe.exe"
# add ~\bin to your user PATH (new terminals pick it up):
[Environment]::SetEnvironmentVariable(
  'Path', [Environment]::GetEnvironmentVariable('Path','User') + ";$HOME\bin", 'User')
```

### Build from source

```bash
git clone https://github.com/nmud/aws-instance-sniper
cd aws-instance-sniper
go build -o isnipe ./cmd/isnipe      # Windows: -o isnipe.exe
```

On macOS / Linux `make build` does the same, and `make dist` cross-compiles a
binary for every OS/arch into `./dist`.

## Usage

```bash
isnipe
```

The interactive flow verifies your AWS credentials (using your existing
profiles / SSO sessions, and offering a guided `aws configure` / `aws sso
login` if the AWS CLI is present), asks for a region, then shows an arrow-key
picker of your stopped instances. Pick one, set the retry delay, and it starts
sniping.

While sniping: **Esc** cancels and returns to the instance picker,
**Ctrl-C** quits.

Know what you want already? Skip the prompts:

```bash
isnipe -i i-09920693e8c413d06 -r us-east-1 -d 2 -j
```

| Flag | Meaning |
|---|---|
| `-p, --profile NAME` | AWS profile to use |
| `-r, --region REGION` | Region (default: profile's region) |
| `-i, --instance-id ID` | Skip the picker |
| `-d, --delay SECONDS` | Delay between attempts (default 5, min 1) |
| `-j, --jitter` / `--no-jitter` | Randomize each delay by ±40% |
| `-V, --version` | Print version |
| `-h, --help` | Show help |

## Credentials

isnipe uses the standard AWS credential chain — environment variables, shared
config/credentials files, SSO sessions, and instance/container roles — so if
the AWS CLI or SDKs already work on your machine, isnipe does too. `--profile`
and `AWS_PROFILE` select a profile; SSO sessions are read from the usual cache.

## What it does with errors

- `InsufficientInstanceCapacity` — keeps retrying; that's the point.
- Throttling / `RequestLimitExceeded` — backs off exponentially (max 60s),
  then resumes.
- Instance already `pending`/`running` — counts as a win; still `stopping` —
  waits for it to settle.
- Expired or invalid credentials — stops and tells you how to re-auth.
- Anything else — prints the error and stops rather than spamming the API.

## If you're fighting ICE constantly

An [On-Demand Capacity Reservation](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-capacity-reservations.html)
guarantees the capacity (you pay for it even while stopped), and
attribute-based instance selection in a launch template lets AWS substitute
equivalent hardware.
