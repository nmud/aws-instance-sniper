# isnipe

Retries `StartInstances` on a stopped EC2 instance until AWS frees up capacity
(`InsufficientInstanceCapacity`), then tells you the moment it's yours.

One bash script. The only dependency is the AWS CLI — and if it's missing,
isnipe walks you through installing and authenticating.

## Install

**curl**:

```bash
mkdir -p ~/.local/bin && curl -fsSL https://raw.githubusercontent.com/nmud/aws-instance-sniper/main/isnipe -o ~/.local/bin/isnipe && chmod +x ~/.local/bin/isnipe
```

**From a clone**, drop the script anywhere on your `PATH`:

```bash
install -m 755 isnipe ~/.local/bin/
```

Requires bash ≥ 4 (macOS ships 3.2 — `brew install bash` first).

## Usage

```bash
isnipe
```

The interactive flow checks the AWS CLI and your credentials (offering a
guided install / `aws sso login` / `aws configure` where needed), asks for a
region, then shows an arrow-key picker of your stopped instances. Pick one,
set the retry delay, and it starts sniping.

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
