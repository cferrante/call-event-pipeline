package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cferrante/call-event-pipeline/internal/twilio"
)

// The canary's numbers. From isn't E.164, so the canary never creates a lead; reports leave
// it out (migration 0007); To is in the 555-01xx range reserved for fiction.
const (
	CanaryFrom = "canary"
	CanaryTo   = "+15550100001"
)

// Canary sends a synthetic voice webhook, signed as Twilio would sign it. Its arrival in the
// database proves the whole path works, even when real traffic is naturally quiet.
type Canary struct {
	Target    string // the edge's address, e.g. http://127.0.0.1:8081
	PublicURL string // what Twilio would call; the signature covers it
	AuthToken string
	Client    *http.Client
}

// Send posts one canary webhook and reports an error unless the edge accepted it.
func (c Canary) Send(ctx context.Context) error {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	form := url.Values{"CallSid": {"CAcanary" + hex.EncodeToString(b)}, "From": {CanaryFrom}, "To": {CanaryTo}}
	const path = "/webhooks/voice"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.Target, "/")+path,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", twilio.Signature(c.AuthToken, strings.TrimSuffix(c.PublicURL, "/")+path, form))
	resp, err := c.Client.Do(req)
	if err != nil {
		return fmt.Errorf("canary: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("canary: edge answered %s", resp.Status)
	}
	return nil
}
