package main

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"
)

type snipeOutcome int

const (
	outAcquired  snipeOutcome = iota
	outCancelled              // Esc — back to the picker
	outQuit                   // Ctrl-C — exit
	outError
)

func jitterDelay(d float64, jitter bool) float64 {
	if !jitter {
		return d
	}
	return d + (rand.Float64()-0.5)*0.8*d // ±40%
}

func isThrottle(code string) bool {
	switch code {
	case "RequestLimitExceeded", "Throttling", "ThrottlingException",
		"TooManyRequestsException", "Client.RequestLimitExceeded":
		return true
	}
	return false
}

func isAuthCode(code string) bool {
	switch code {
	case "ExpiredToken", "ExpiredTokenException", "RequestExpired",
		"InvalidClientTokenId", "AuthFailure", "UnauthorizedOperation",
		"UnrecognizedClientException", "SignatureDoesNotMatch":
		return true
	}
	return false
}

func looksLikeAuth(err error) bool {
	if err == nil {
		return false
	}
	s := lower(err.Error())
	for _, k := range []string{
		"unable to locate credentials", "no credentials", "failed to refresh cached credentials",
		"sso session has expired", "token has expired", "expired or invalid",
		"get credentials", "no valid providers",
	} {
		if contains(s, k) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- bubbletea UI

type apiResultMsg struct {
	success bool
	code    string
	err     error
}
type stateResultMsg struct {
	state string
	err   error
}
type tickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

type snipeModel struct {
	client     *ec2.Client
	id, region string
	delay      float64
	jitter     bool

	start     time.Time
	attempt   int
	throttles int

	phase     string // "calling" | "waiting"
	note      string
	remaining int

	outcome  snipeOutcome
	err      error
	authHint bool
	already  bool
}

func newSnipeModel(c *ec2.Client, id, region string, delay float64, jitter bool) snipeModel {
	return snipeModel{
		client: c, id: id, region: region, delay: delay, jitter: jitter,
		start: time.Now(), attempt: 1, phase: "calling",
	}
}

func (m snipeModel) callStart() tea.Cmd {
	client, id := m.client, m.id
	return func() tea.Msg {
		if err := startInstance(context.Background(), client, id); err != nil {
			return apiResultMsg{code: apiErrorCode(err), err: err}
		}
		return apiResultMsg{success: true}
	}
}

func (m snipeModel) describeState() tea.Cmd {
	client, id := m.client, m.id
	return func() tea.Msg {
		st, err := instanceState(context.Background(), client, id)
		return stateResultMsg{state: st, err: err}
	}
}

func (m snipeModel) Init() tea.Cmd { return m.callStart() }

func (m snipeModel) beginWait(sec float64, note string) (snipeModel, tea.Cmd) {
	m.phase = "waiting"
	m.note = note
	m.remaining = int(math.Round(sec))
	if m.remaining < 1 {
		m.remaining = 1
	}
	return m, tickCmd()
}

func (m snipeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEsc:
			m.outcome = outCancelled
			return m, tea.Quit
		case tea.KeyCtrlC:
			m.outcome = outQuit
			return m, tea.Quit
		}
		return m, nil

	case apiResultMsg:
		if msg.success {
			m.outcome = outAcquired
			return m, tea.Quit
		}
		switch {
		case msg.code == "InsufficientInstanceCapacity":
			m.throttles = 0
			return m.beginWait(jitterDelay(m.delay, m.jitter), "no capacity yet")
		case isThrottle(msg.code):
			m.throttles++
			w := m.delay * math.Pow(2, float64(m.throttles))
			if w > 60 {
				w = 60
			}
			return m.beginWait(w, "rate limited by AWS — backing off")
		case msg.code == "IncorrectInstanceState":
			m.phase = "calling"
			return m, m.describeState()
		case isAuthCode(msg.code) || looksLikeAuth(msg.err):
			m.outcome, m.err, m.authHint = outError, msg.err, true
			return m, tea.Quit
		default:
			m.outcome, m.err = outError, msg.err
			return m, tea.Quit
		}

	case stateResultMsg:
		switch msg.state {
		case "pending", "running":
			m.already, m.outcome = true, outAcquired
			return m, tea.Quit
		case "stopping":
			return m.beginWait(m.delay, "instance still stopping — waiting to settle")
		default:
			m.outcome = outError
			if msg.err != nil {
				m.err = msg.err
			} else {
				m.err = fmt.Errorf("instance is in state %q and can't be started", msg.state)
			}
			return m, tea.Quit
		}

	case tickMsg:
		if m.phase == "waiting" && m.remaining > 0 {
			m.remaining--
			if m.remaining <= 0 {
				m.attempt++
				m.phase = "calling"
				return m, m.callStart()
			}
			return m, tickCmd()
		}
		return m, nil
	}
	return m, nil
}

func (m snipeModel) View() string {
	header := fmt.Sprintf("%s Sniping %s in %s every %ss%s. %s",
		cyn("›"), bold(m.id), bold(m.region), trimNum(m.delay),
		ternary(m.jitter, " (jittered)", ""),
		dim("Esc cancels · Ctrl-C quits."))

	elapsed := dim("(elapsed " + fmtElapsed(time.Since(m.start)) + ")")
	var status string
	switch m.phase {
	case "waiting":
		status = fmt.Sprintf("%s attempt #%d — %s, retrying in %ds %s",
			dim("…"), m.attempt, m.note, m.remaining, elapsed)
	default: // calling
		status = fmt.Sprintf("%s attempt #%d — contacting EC2… %s",
			dim("…"), m.attempt, elapsed)
	}
	return header + "\n" + status + "\n"
}

// ---------------------------------------------------------------- entry points

// snipe runs the retry loop and prints the outcome. It returns the outcome so
// the caller can loop back to the picker (cancelled) or exit.
func (a *App) snipe(ctx context.Context) snipeOutcome {
	fmt.Fprintln(out)
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return a.snipePlain(ctx)
	}

	res, err := tea.NewProgram(
		newSnipeModel(a.ec2, a.opts.instanceID, a.region, a.opts.delay, a.opts.jitter),
		tea.WithContext(ctx),
	).Run()
	if err != nil {
		errmsg("Terminal error: %v", err)
		return outError
	}
	m := res.(snipeModel)

	switch m.outcome {
	case outAcquired:
		a.celebrate(ctx, m.attempt, m.start, m.already)
	case outCancelled:
		warn("Cancelled after %d attempt(s) (%s) — back to instance selection.",
			m.attempt, fmtElapsed(time.Since(m.start)))
	case outError:
		a.reportTerminalError(m.attempt, m.err, m.authHint)
	}
	return m.outcome
}

// snipePlain is the non-interactive fallback (stdin isn't a TTY): no Esc-cancel,
// Ctrl-C via signal still quits.
func (a *App) snipePlain(ctx context.Context) snipeOutcome {
	attempt, throttles := 0, 0
	start := time.Now()
	info("Sniping %s in %s every %ss%s.", bold(a.opts.instanceID), bold(a.region),
		trimNum(a.opts.delay), ternary(a.opts.jitter, " (jittered)", ""))

	for {
		if ctx.Err() != nil {
			return outQuit
		}
		attempt++
		err := startInstance(ctx, a.ec2, a.opts.instanceID)
		if err == nil {
			a.celebrate(ctx, attempt, start, false)
			return outAcquired
		}
		code := apiErrorCode(err)
		switch {
		case code == "InsufficientInstanceCapacity":
			throttles = 0
			w := jitterDelay(a.opts.delay, a.opts.jitter)
			info("attempt #%d — no capacity yet, retrying in %ss.", attempt, trimNum(w))
			if sleepCtx(ctx, w) != nil {
				return outQuit
			}
		case isThrottle(code):
			throttles++
			w := a.opts.delay * math.Pow(2, float64(throttles))
			if w > 60 {
				w = 60
			}
			warn("Rate limited by AWS (attempt #%d) — backing off %ss.", attempt, trimNum(w))
			if sleepCtx(ctx, w) != nil {
				return outQuit
			}
		case code == "IncorrectInstanceState":
			st, _ := instanceState(ctx, a.ec2, a.opts.instanceID)
			switch st {
			case "pending", "running":
				info("Instance is already %s — someone (or a previous attempt) beat us to it.", st)
				a.celebrate(ctx, attempt, start, true)
				return outAcquired
			case "stopping":
				warn("Instance is still stopping — waiting %ss for it to settle.", trimNum(a.opts.delay))
				if sleepCtx(ctx, a.opts.delay) != nil {
					return outQuit
				}
			default:
				errmsg("Instance is in state %q and can't be started. Aborting.", st)
				return outError
			}
		case isAuthCode(code) || looksLikeAuth(err):
			a.reportTerminalError(attempt, err, true)
			return outError
		default:
			a.reportTerminalError(attempt, err, false)
			return outError
		}
	}
}

func (a *App) reportTerminalError(attempt int, err error, auth bool) {
	if auth {
		errmsg("Credentials problem on attempt #%d — stopping so you can re-authenticate:", attempt)
		fmt.Fprintf(out, "  %s\n", dim(firstLine(err.Error())))
		if a.profile != "" {
			info("If it's an SSO profile, try: %s then re-run isnipe.",
				bold("aws sso login --profile "+a.profile))
		}
		return
	}
	errmsg("Unexpected error on attempt #%d — stopping:", attempt)
	fmt.Fprintf(out, "  %s\n", dim(firstLine(err.Error())))
}

func (a *App) celebrate(ctx context.Context, attempt int, start time.Time, already bool) {
	ok("%s %s is starting — capacity acquired on attempt #%d after %s.",
		grn(bold("🎯 Got it!")), bold(a.opts.instanceID), attempt, fmtElapsed(time.Since(start)))
	_ = already
	link := fmt.Sprintf("https://%s.console.aws.amazon.com/ec2/home?region=%s#InstanceDetails:instanceId=%s",
		a.region, a.region, a.opts.instanceID)
	info("Console: %s", link)
	info("Waiting for the instance to reach 'running'...")
	if err := waitRunning(ctx, a.ec2, a.opts.instanceID, 5*time.Minute); err != nil {
		warn("Start succeeded, but the 'running' wait timed out — check the console link above.")
		return
	}
	ok("Instance is %s.", grn(bold("running")))
	ip, dns := publicAddr(ctx, a.ec2, a.opts.instanceID)
	if ip != "" && ip != "None" {
		info("Public IP:  %s", bold(ip))
	}
	if dns != "" && dns != "None" {
		info("Public DNS: %s", dns)
	}
}

func sleepCtx(ctx context.Context, sec float64) error {
	t := time.NewTimer(time.Duration(sec * float64(time.Second)))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
