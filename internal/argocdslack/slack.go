package argocdslack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// SlackError is a Web API "ok": false.
type SlackError struct {
	Method string
	Code   string
}

func (e *SlackError) Error() string { return e.Method + ": " + e.Code }

// IsInvalidBlocks reports a payload Slack refused to render.
func IsInvalidBlocks(err error) bool {
	var se *SlackError
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code {
	case "invalid_blocks", "invalid_blocks_format", "invalid_attachments", "invalid_arguments":
		return true
	}
	return false
}

// Slack is the subset of the Web API the app uses.
type Slack interface {
	Post(ctx context.Context, p Payload) (Message, error)
	Update(ctx context.Context, p Payload) error
	Ephemeral(ctx context.Context, channel, user, text string) error
	PublishHome(ctx context.Context, user string, view any) error
	OpenView(ctx context.Context, triggerID string, view any) error
}

// SlackClient calls slack.com with a bot token.
type SlackClient struct {
	Token   string
	DryRun  bool
	BaseURL string
	HTTP    *http.Client
	Log     *slog.Logger

	dry atomic.Int64
}

func (c *SlackClient) base() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return "https://slack.com/api/"
}

func (c *SlackClient) call(ctx context.Context, method string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if c.DryRun {
		c.Log.Info("slack dry run", "method", method, "payload", json.RawMessage(payload))
		return nil
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+method, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		resp, err := hc.Do(req)
		if err != nil {
			return fmt.Errorf("%s: %w", method, err)
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 2 {
			wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			if wait <= 0 || wait > 20 {
				wait = 3
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(wait) * time.Second):
			}
			continue
		}
		var res struct {
			OK      bool   `json:"ok"`
			Error   string `json:"error"`
			Warning string `json:"warning"`
		}
		if err := json.Unmarshal(b, &res); err != nil {
			return fmt.Errorf("%s: %s: %s", method, resp.Status, trimTo(string(b), 200))
		}
		if !res.OK {
			c.Log.Warn("slack call failed", "method", method, "error", res.Error, "response", trimTo(string(b), 500))
			return &SlackError{Method: method, Code: res.Error}
		}
		if out != nil {
			return json.Unmarshal(b, out)
		}
		return nil
	}
}

func (c *SlackClient) Post(ctx context.Context, p Payload) (Message, error) {
	var out struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := c.call(ctx, "chat.postMessage", p, &out); err != nil {
		return Message{}, err
	}
	if c.DryRun {
		return Message{Channel: p.Channel, TS: fmt.Sprintf("dry.%d", c.dry.Add(1))}, nil
	}
	return Message{Channel: out.Channel, TS: out.TS}, nil
}

func (c *SlackClient) Update(ctx context.Context, p Payload) error {
	p.ThreadTS, p.UnfurlLinks, p.UnfurlMedia = "", false, false
	return c.call(ctx, "chat.update", p, nil)
}

func (c *SlackClient) Ephemeral(ctx context.Context, channel, user, text string) error {
	return c.call(ctx, "chat.postEphemeral", map[string]string{"channel": channel, "user": user, "text": text}, nil)
}

func (c *SlackClient) PublishHome(ctx context.Context, user string, view any) error {
	return c.call(ctx, "views.publish", map[string]any{"user_id": user, "view": view}, nil)
}

func (c *SlackClient) OpenView(ctx context.Context, triggerID string, view any) error {
	return c.call(ctx, "views.open", map[string]any{"trigger_id": triggerID, "view": view}, nil)
}

// VerifySignature checks Slack's v0 request signature within a 5-minute window.
func VerifySignature(secret, timestamp, signature string, body []byte, now time.Time) error {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("missing or bad X-Slack-Request-Timestamp")
	}
	if d := now.Sub(time.Unix(ts, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return errors.New("stale Slack request")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%s:", timestamp)
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(signature)) {
		return errors.New("bad Slack signature")
	}
	return nil
}
