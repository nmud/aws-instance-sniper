package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "2.0.0"

var (
	errUserQuit  = errors.New("user quit")
	instanceIDRe = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)
)

type options struct {
	profile     string
	region      string
	instanceID  string
	delay       float64
	delaySet    bool
	jitter      bool
	jitterSet   bool
	showVersion bool
	showHelp    bool
}

type App struct {
	opts     options
	cfg      aws.Config
	ec2      *ec2.Client
	profile  string
	region   string
	identity string
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		errmsg("%v", err)
		fmt.Fprintln(out)
		usage()
		os.Exit(1)
	}
	if opts.showHelp {
		usage()
		return
	}
	if opts.showVersion {
		fmt.Fprintf(out, "isnipe %s\n", version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	banner()
	app := &App{opts: opts}
	if err := app.run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(out)
			warn("Interrupted.")
			os.Exit(130)
		}
		die("%v", err)
	}
}

func (a *App) run(ctx context.Context) error {
	if err := a.ensureAuth(ctx); err != nil {
		if errors.Is(err, errUserQuit) {
			return nil
		}
		return err
	}
	if err := a.ensureRegion(ctx); err != nil {
		return err
	}
	for {
		if a.opts.instanceID == "" {
			if err := a.pickInstance(ctx); err != nil {
				if errors.Is(err, errUserQuit) {
					return nil
				}
				if errors.Is(err, errExit) {
					os.Exit(1)
				}
				return err
			}
		}
		a.askTuning()
		switch a.snipe(ctx) {
		case outAcquired:
			return nil
		case outCancelled:
			a.opts.instanceID = ""
			fmt.Fprintln(out)
		case outQuit:
			return nil
		case outError:
			os.Exit(1)
		}
	}
}

// ---------------------------------------------------------------- auth

func hasAWSCLI() bool {
	_, err := exec.LookPath("aws")
	return err == nil
}

func runAWS(args ...string) error {
	cmd := exec.Command("aws", args...)
	// Inherit the real terminal so the AWS CLI's interactive prompts work.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func (a *App) authSuccess() {
	msg := "Authenticated as " + bold(a.identity)
	if a.profile != "" {
		msg += " " + dim("(profile: "+a.profile+")")
	}
	ok("%s", msg)
}

func (a *App) commit(cfg aws.Config, profile, arn string) {
	a.cfg, a.profile, a.identity = cfg, profile, arn
	a.authSuccess()
}

func (a *App) ensureAuth(ctx context.Context) error {
	a.profile, a.region = a.opts.profile, a.opts.region

	if cfg, err := loadConfig(ctx, a.profile, a.region); err == nil {
		if arn, e2 := callerIdentity(ctx, cfg); e2 == nil {
			a.commit(cfg, a.profile, arn)
			return nil
		}
	}
	if a.profile != "" {
		warn("Profile %q isn't authenticated yet.", a.profile)
	}

	for {
		profiles := listProfiles()
		cli := hasAWSCLI()

		var menu []string
		for _, p := range profiles {
			menu = append(menu, "Use profile: "+p)
		}
		if cli {
			menu = append(menu, "Set up a new profile with access keys  (aws configure)")
			menu = append(menu, "Set up SSO / IAM Identity Center       (aws configure sso)")
		}
		menu = append(menu, "Quit")

		fmt.Fprintln(out)
		if cli {
			warn("You're not authenticated with AWS yet.")
		} else {
			warn("You're not authenticated with AWS yet. %s",
				dim("(install the AWS CLI to set up new credentials from here)"))
		}
		idx, sel := selectMenu("How do you want to authenticate?", menu)
		if !sel {
			return errUserQuit
		}

		n := len(profiles)
		switch {
		case idx < n:
			p := profiles[idx]
			cfg, err := loadConfig(ctx, p, a.region)
			if err != nil {
				errmsg("Profile %q failed to load: %s", p, firstLine(err.Error()))
				continue
			}
			arn, e2 := callerIdentity(ctx, cfg)
			if e2 == nil {
				a.commit(cfg, p, arn)
				return nil
			}
			if cli && looksLikeAuth(e2) {
				warn("Profile %q needs an SSO login.", p)
				info("Running: aws sso login --profile %s", p)
				if runAWS("sso", "login", "--profile", p) == nil {
					if cfg2, e3 := loadConfig(ctx, p, a.region); e3 == nil {
						if arn2, e4 := callerIdentity(ctx, cfg2); e4 == nil {
							a.commit(cfg2, p, arn2)
							return nil
						}
					}
				}
				errmsg("SSO login didn't complete — pick another option.")
			} else {
				errmsg("Profile %q failed to authenticate: %s", p, firstLine(e2.Error()))
			}

		case cli && idx == n:
			name := askInput("Name for the new profile", "default")
			_ = runAWS("configure", "--profile", name)
			cfg, err := loadConfig(ctx, name, a.region)
			if err == nil {
				if arn, e2 := callerIdentity(ctx, cfg); e2 == nil {
					a.commit(cfg, name, arn)
					return nil
				}
			}
			errmsg("Still not authenticated — double-check the keys.")

		case cli && idx == n+1:
			info("Running: aws configure sso %s", dim("(follow the prompts — note the profile name it creates)"))
			_ = runAWS("configure", "sso")
			name := askInput("Profile name you just created", "")
			if name == "" {
				continue
			}
			cfg, err := loadConfig(ctx, name, a.region)
			if err == nil {
				if arn, e2 := callerIdentity(ctx, cfg); e2 == nil {
					a.commit(cfg, name, arn)
					return nil
				}
			}
			if runAWS("sso", "login", "--profile", name) == nil {
				if cfg2, e3 := loadConfig(ctx, name, a.region); e3 == nil {
					if arn2, e4 := callerIdentity(ctx, cfg2); e4 == nil {
						a.commit(cfg2, name, arn2)
						return nil
					}
				}
			}
			errmsg("That profile didn't authenticate.")

		default:
			return errUserQuit
		}
	}
}

// ---------------------------------------------------------------- region

func (a *App) ensureRegion(ctx context.Context) error {
	if a.region == "" {
		a.region = a.cfg.Region
	}
	if a.region == "" {
		return a.applyRegion(ctx, askInput("AWS region", "us-east-1"))
	}
	return a.applyRegion(ctx, a.region)
}

func (a *App) promptRegion(ctx context.Context) error {
	return a.applyRegion(ctx, askInput("AWS region", a.region))
}

func (a *App) applyRegion(ctx context.Context, region string) error {
	cfg, err := loadConfig(ctx, a.profile, region)
	if err != nil {
		return err
	}
	cfg.Region = region
	a.cfg, a.region = cfg, region
	a.ec2 = ec2.NewFromConfig(cfg)
	info("Region: %s", bold(region))
	return nil
}

// ---------------------------------------------------------------- pick instance

func (a *App) pickInstance(ctx context.Context) error {
	for {
		info("Looking for stopped instances in %s...", bold(a.region))
		list, err := describeStopped(ctx, a.ec2)
		if err != nil {
			errmsg("Couldn't list instances:")
			fmt.Fprintf(out, "  %s\n", dim(firstLine(err.Error())))
			if isAuthCode(apiErrorCode(err)) || looksLikeAuth(err) {
				info("Re-authenticate and try again.")
			}
			return errExit
		}

		if len(list) == 0 {
			warn("No stopped instances found in %s.", a.region)
			idx, sel := selectMenu("What next?", []string{"Refresh", "Change region", "Quit"})
			if !sel {
				return errUserQuit
			}
			switch idx {
			case 0:
				continue
			case 1:
				if err := a.promptRegion(ctx); err != nil {
					return err
				}
				continue
			default:
				return errUserQuit
			}
		}

		menu := make([]string, len(list))
		for i, in := range list {
			name := in.Name
			if name == "" {
				name = "(no name)"
			}
			menu[i] = fmt.Sprintf("%-21s %-16s %-14s %s", in.ID, in.Type, in.AZ, name)
		}
		fmt.Fprintln(out)
		idx, sel := selectMenu("Select the stopped instance to snipe:", menu)
		if !sel {
			return errUserQuit
		}
		a.opts.instanceID = list[idx].ID
		ok("Target locked: %s", bold(list[idx].ID))
		return nil
	}
}

var errExit = errors.New("exit")

// ---------------------------------------------------------------- tuning

func (a *App) askTuning() {
	if !a.opts.delaySet {
		for {
			s := askInput("Delay between attempts, in seconds", "5")
			if d, err := strconv.ParseFloat(s, 64); err == nil && d >= 1 {
				a.opts.delay, a.opts.delaySet = d, true
				break
			}
			warn("Please enter a number ≥ 1.")
		}
	}
	if a.opts.delay < 2 {
		warn("Delays under 2s can trip EC2 request throttling — isnipe will back off automatically if that happens.")
	}
	if !a.opts.jitterSet {
		v, sel := confirm(
			"Add random jitter to each attempt? (±40% of delay — avoids syncing with other retry loops)",
			"Yes (recommended)",
			fmt.Sprintf("No — fixed %ss", trimNum(a.opts.delay)),
		)
		if !sel {
			os.Exit(0)
		}
		a.opts.jitter, a.opts.jitterSet = v, true
	}
}

// ---------------------------------------------------------------- args / help

func parseArgs(args []string) (options, error) {
	var o options
	for i := 0; i < len(args); i++ {
		arg := args[i]
		need := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return args[i], nil
		}
		switch arg {
		case "-p", "--profile":
			v, err := need()
			if err != nil {
				return o, err
			}
			o.profile = v
		case "-r", "--region":
			v, err := need()
			if err != nil {
				return o, err
			}
			o.region = v
		case "-i", "--instance-id":
			v, err := need()
			if err != nil {
				return o, err
			}
			o.instanceID = v
		case "-d", "--delay":
			v, err := need()
			if err != nil {
				return o, err
			}
			d, perr := strconv.ParseFloat(v, 64)
			if perr != nil || d < 1 {
				return o, fmt.Errorf("--delay must be a number ≥ 1")
			}
			o.delay, o.delaySet = d, true
		case "-j", "--jitter":
			o.jitter, o.jitterSet = true, true
		case "--no-jitter":
			o.jitter, o.jitterSet = false, true
		case "-V", "--version":
			o.showVersion = true
		case "-h", "--help":
			o.showHelp = true
		default:
			return o, fmt.Errorf("unknown option: %s", arg)
		}
	}
	if o.instanceID != "" && !instanceIDRe.MatchString(o.instanceID) {
		return o, fmt.Errorf("%q doesn't look like an instance ID (expected i-xxxxxxxxxxxxxxxxx)", o.instanceID)
	}
	return o, nil
}

func banner() {
	fmt.Fprintf(out, "%s %s\n\n", bold("🎯 isnipe"), dim("v"+version+" — EC2 capacity sniper"))
}

func usage() {
	fmt.Fprintf(out, `%s v%s — EC2 capacity sniper

Retries StartInstances on a stopped instance until AWS frees up capacity
(InsufficientInstanceCapacity). Interactive by default; flags skip prompts.

%s isnipe [options]

%s
  -p, --profile NAME     AWS profile to use
  -r, --region REGION    AWS region (default: profile's region)
  -i, --instance-id ID   Skip the picker and snipe this instance
  -d, --delay SECONDS    Delay between attempts (default: 5, min: 1)
  -j, --jitter           Randomize each delay by ±40%%
      --no-jitter        Fixed interval
  -V, --version          Print version
  -h, --help             Show this help

%s
  isnipe
  isnipe -i i-09920693e8c413d06 -r us-east-1 -d 2 -j
`, bold("isnipe"), version, bold("Usage:"), bold("Options:"), bold("Examples:"))
}

// ---------------------------------------------------------------- small helpers

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func trimNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func fmtElapsed(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s%3600/60, s%60)
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func readLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(string(b), "\n")
}

func lower(s string) string       { return strings.ToLower(s) }
func contains(s, sub string) bool { return strings.Contains(s, sub) }
