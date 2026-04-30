// Package notify sends email when interesting things happen to processes.
//
// The whole reason this exists in-tree (rather than as an external listener
// process talking to direktord over the API) is that supervisor's "do this
// kind of thing" story was the absolutely byzantine eventlistener system —
// you wrote a separate Python script that read events on stdin in a special
// protocol and supervisor would helpfully pause your processes if your
// listener fell over. It was a fucking shambles.
//
// So we just baked email in. SMTP host, port, from, recipients, done. If
// you need fancier (Slack, PagerDuty, telegrams via owl), the JSON API is
// right there — write something that polls /api/processes and acts on
// state changes. That'll be more reliable than anything we'd build in here.
package notify

import (
	"crypto/tls"
	"fmt"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"github.com/andydixon/direktor/internal/logging"
	"github.com/andydixon/direktor/pkg/types"
)

// EmailConfig — SMTP knobs. NotifyOn is a list of states that should trigger
// an email; if it's empty we use a sensible default set (see shouldNotify).
type EmailConfig struct {
	Enabled    bool                 `json:"enabled"`
	SMTPHost   string               `json:"smtp_host"`
	SMTPPort   int                  `json:"smtp_port"`
	Username   string               `json:"username"`
	Password   string               `json:"password"`
	From       string               `json:"from"`
	Recipients []string             `json:"recipients"`
	UseTLS     bool                 `json:"use_tls"`
	NotifyOn   []types.ProcessState `json:"notify_on"`
}

// StateChangeEvent is what we queue up. One of these per state transition;
// the queue gets batched into a single email to keep the volume down.
type StateChangeEvent struct {
	ProcessName string
	OldState    types.ProcessState
	NewState    types.ProcessState
	PID         int
	ExitCode    int
	Timestamp   time.Time
	Message     string
}

// Notifier owns the queue and the goroutine that drains it. One per daemon.
type Notifier struct {
	mu       sync.RWMutex
	config   EmailConfig
	logger   *logging.Logger
	queue    chan StateChangeEvent
	stopCh   chan struct{}
	hostname string // baked into the subject line so multi-host alerts are distinguishable
}

// NewNotifier builds a Notifier and (if email's enabled) starts the
// background drain goroutine. If email's off, this is essentially a no-op
// constructor — Notify will accept events and drop them on the floor.
func NewNotifier(cfg EmailConfig, logger *logging.Logger, hostname string) *Notifier {
	n := &Notifier{
		config:   cfg,
		logger:   logger,
		queue:    make(chan StateChangeEvent, 100), // 100 = arbitrary, but large enough to absorb a flap
		stopCh:   make(chan struct{}),
		hostname: hostname,
	}

	if cfg.Enabled {
		go n.processQueue()
	}

	return n
}

// UpdateConfig swaps the config at runtime. Note: doesn't start/stop the
// drain goroutine — if you toggle Enabled at runtime you'll want a restart
// to actually take effect. We don't, because this isn't called from anywhere
// yet, but it's here for future use.
func (n *Notifier) UpdateConfig(cfg EmailConfig) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.config = cfg
}

// Notify queues a state change event for emailing. Non-blocking: if the
// queue's full we log and drop, rather than wedge whatever was trying to
// send the event.
//
// "Drop on full" beats "block until drained" every time, because the alternative
// is your supervisor freezing because a misconfigured SMTP server is timing
// out. Supervisor used to have something like this and people *hated* it.
func (n *Notifier) Notify(event StateChangeEvent) {
	n.mu.RLock()
	cfg := n.config
	n.mu.RUnlock()

	if !cfg.Enabled {
		return
	}

	if !n.shouldNotify(event.NewState) {
		return
	}

	select {
	case n.queue <- event:
	default:
		n.logger.Warn("notification queue full, dropping event",
			"process", event.ProcessName, "state", event.NewState)
	}
}

// Stop signals the drain goroutine to flush any pending batch and exit.
func (n *Notifier) Stop() {
	close(n.stopCh)
}

// shouldNotify decides whether the new state is worth an email. If the user
// listed states explicitly in NotifyOn, that wins. Otherwise we use a
// hard-coded "interesting transitions" set — failures and significant
// successes — so that out-of-the-box behaviour is useful.
func (n *Notifier) shouldNotify(state types.ProcessState) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()

	if len(n.config.NotifyOn) == 0 {
		switch state {
		case types.StateFatal, types.StateExited, types.StateRunning, types.StateStopped:
			return true
		}
		return false
	}

	for _, s := range n.config.NotifyOn {
		if s == state {
			return true
		}
	}
	return false
}

// processQueue drains the queue and sends batched emails. Two flush triggers:
//
//   - Batch hits 10 events: send immediately. Stops one runaway flap from
//     pushing the queue's tail too far behind realtime.
//   - 5-second tick with anything pending: send what we've got. Stops a
//     single lonely event from sitting in the queue forever.
//
// On stop, flush whatever's left and exit.
func (n *Notifier) processQueue() {
	var batch []StateChangeEvent
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case event := <-n.queue:
			batch = append(batch, event)
			if len(batch) >= 10 {
				n.sendBatch(batch)
				batch = nil
			}
		case <-ticker.C:
			if len(batch) > 0 {
				n.sendBatch(batch)
				batch = nil
			}
		case <-n.stopCh:
			if len(batch) > 0 {
				n.sendBatch(batch)
			}
			return
		}
	}
}

// sendBatch actually fires off the email. Logs success and failure both —
// you want to know if alerts are being delivered, *and* you want to know if
// they're not.
func (n *Notifier) sendBatch(events []StateChangeEvent) {
	n.mu.RLock()
	cfg := n.config
	n.mu.RUnlock()

	if len(cfg.Recipients) == 0 {
		// No recipients = nowhere to send. Don't even try.
		return
	}

	subject := n.buildSubject(events)
	body := n.buildBody(events)

	if err := n.sendEmail(cfg, subject, body); err != nil {
		n.logger.Error("failed to send notification email",
			"error", err, "events", len(events))
	} else {
		n.logger.Info("notification email sent",
			"recipients", len(cfg.Recipients), "events", len(events))
	}
}

// buildSubject generates a nice scannable subject. Single-event batches get
// the specifics; multi-event batches get a count. Both include the hostname
// so you can tell your prod from your staging at a glance.
func (n *Notifier) buildSubject(events []StateChangeEvent) string {
	if len(events) == 1 {
		e := events[0]
		return fmt.Sprintf("[Direktor/%s] %s → %s", n.hostname, e.ProcessName, e.NewState)
	}
	return fmt.Sprintf("[Direktor/%s] %d process state changes", n.hostname, len(events))
}

// buildBody renders the plain-text email body. Plain text on purpose — it
// works in every email client, every terminal mail reader, every tooling
// pipeline. HTML email is only ever a problem.
func (n *Notifier) buildBody(events []StateChangeEvent) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Direktor Process Supervisor (%s)\n", n.hostname))
	sb.WriteString(strings.Repeat("=", 50))
	sb.WriteString("\n\n")

	for _, e := range events {
		sb.WriteString(fmt.Sprintf("Process: %s\n", e.ProcessName))
		sb.WriteString(fmt.Sprintf("  State:     %s → %s\n", e.OldState, e.NewState))
		sb.WriteString(fmt.Sprintf("  Time:      %s\n", e.Timestamp.Format(time.RFC3339)))
		if e.PID > 0 {
			sb.WriteString(fmt.Sprintf("  PID:       %d\n", e.PID))
		}
		if e.NewState == types.StateExited || e.NewState == types.StateFatal {
			sb.WriteString(fmt.Sprintf("  Exit Code: %d\n", e.ExitCode))
		}
		if e.Message != "" {
			sb.WriteString(fmt.Sprintf("  Details:   %s\n", e.Message))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("--\nSent by Direktor Process Supervisor\n")
	return sb.String()
}

// sendEmail dispatches to the right transport. TLS goes via our hand-rolled
// helper because net/smtp.SendMail doesn't natively do implicit TLS (port
// 465 style); for plain / STARTTLS the stdlib SendMail is fine.
func (n *Notifier) sendEmail(cfg EmailConfig, subject, body string) error {
	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	msg := buildMIMEMessage(cfg.From, cfg.Recipients, subject, body)

	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.SMTPHost)
	}

	if cfg.UseTLS {
		return n.sendWithTLS(addr, auth, cfg.From, cfg.Recipients, msg)
	}

	return smtp.SendMail(addr, auth, cfg.From, cfg.Recipients, msg)
}

// sendWithTLS handles implicit-TLS SMTP (typically port 465) — open a TLS
// socket *first*, then speak SMTP over it. That's distinct from STARTTLS,
// which net/smtp's SendMail handles automatically. We need both; the world
// hasn't picked a winner.
func (n *Notifier) sendWithTLS(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	tlsConfig := &tls.Config{
		ServerName: strings.Split(addr, ":")[0],
	}

	conn, err := tls.Dial("tcp", addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("TLS dial: %w", err)
	}

	client, err := smtp.NewClient(conn, strings.Split(addr, ":")[0])
	if err != nil {
		return fmt.Errorf("SMTP client: %w", err)
	}
	defer client.Close()

	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}

	if err := client.Mail(from); err != nil {
		return fmt.Errorf("SMTP MAIL: %w", err)
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			return fmt.Errorf("SMTP RCPT %s: %w", recipient, err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("SMTP write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP close: %w", err)
	}

	return client.Quit()
}

// buildMIMEMessage stitches headers and body into a single byte slice
// suitable for handing to smtp.SendMail (or our TLS variant). Nothing
// fancy: From, To, Subject, Date, plain-text content.
func buildMIMEMessage(from string, to []string, subject, body string) []byte {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("From: %s\r\n", from))
	sb.WriteString(fmt.Sprintf("To: %s\r\n", strings.Join(to, ", ")))
	sb.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	sb.WriteString(fmt.Sprintf("Date: %s\r\n", time.Now().Format(time.RFC1123Z)))
	sb.WriteString("\r\n")
	sb.WriteString(body)
	return []byte(sb.String())
}
