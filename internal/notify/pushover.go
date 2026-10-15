// Package notify sends push notifications.
package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Pushover priorities.
const (
	PriorityQuiet  = -1 // no sound or vibration
	PriorityNormal = 0
)

type Notifier interface {
	Notify(ctx context.Context, title, message string, priority int) error
}

// New returns a Pushover notifier, or a no-op when credentials are unset.
func New(token, user string) Notifier {
	if token == "" || user == "" {
		return Noop{}
	}
	return &Pushover{Token: token, User: user, Endpoint: "https://api.pushover.net/1/messages.json",
		HTTP: &http.Client{Timeout: 15 * time.Second}}
}

type Noop struct{}

func (Noop) Notify(context.Context, string, string, int) error { return nil }

type Pushover struct {
	Token, User, Endpoint string
	HTTP                  *http.Client
}

func (p *Pushover) Notify(ctx context.Context, title, message string, priority int) error {
	form := url.Values{
		"token":    {p.Token},
		"user":     {p.User},
		"title":    {title},
		"message":  {message},
		"priority": {strconv.Itoa(priority)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("pushover: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("pushover: %s: %s", resp.Status, body)
	}
	return nil
}
