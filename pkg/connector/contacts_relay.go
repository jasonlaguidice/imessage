package connector

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rs/zerolog"

	"github.com/lrhodin/imessage/imessage"
)

// contactRelayClient fetches contacts from a NAC relay server running on a Mac
// and caches them locally for fast phone/email lookups with normalization.
type contactRelayClient struct {
	baseURL    string // e.g. "http://199.201.161.163:5001"
	httpClient *http.Client

	mu       sync.RWMutex
	byPhone  map[string]*imessage.Contact // normalized phone → contact
	byEmail  map[string]*imessage.Contact // lowercase email → contact
	lastSync time.Time
}

// newContactRelayFromKey extracts the relay base URL from a hardware key (base64 JSON).
// Returns nil if no relay URL is configured.
func newContactRelayFromKey(hardwareKey string) *contactRelayClient {
	if hardwareKey == "" {
		return nil
	}

	// Strip all non-base64 characters (Beeper UI can inject non-breaking spaces, newlines, etc.)
	cleaned := stripNonBase64(hardwareKey)
	decoded, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(cleaned)
		if err != nil {
			return nil
		}
	}

	var config struct {
		NACRelayURL string `json:"nac_relay_url"`
	}
	if err := json.Unmarshal(decoded, &config); err != nil || config.NACRelayURL == "" {
		return nil
	}

	baseURL := config.NACRelayURL
	if idx := strings.LastIndex(baseURL, "/validation-data"); idx > 0 {
		baseURL = baseURL[:idx]
	}

	return &contactRelayClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		byPhone: make(map[string]*imessage.Contact),
		byEmail: make(map[string]*imessage.Contact),
	}
}

// stripNonBase64 removes all characters that are not valid in base64 encoding.
// This handles garbage injected by chat UIs (non-breaking spaces, newlines, etc.).
func stripNonBase64(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizePhone strips all non-digit characters (except leading +).
func normalizePhone(phone string) string {
	var b strings.Builder
	for i, r := range phone {
		if r == '+' && i == 0 {
			b.WriteRune(r)
		} else if unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// phoneSuffixes returns the number and its last 10/7 digits for flexible matching.
func phoneSuffixes(phone string) []string {
	n := normalizePhone(phone)
	if n == "" {
		return nil
	}
	suffixes := []string{n}
	// Strip leading + for matching
	without := strings.TrimPrefix(n, "+")
	if without != n {
		suffixes = append(suffixes, without)
	}
	// Last 10 digits (US number without country code)
	if len(without) > 10 {
		suffixes = append(suffixes, without[len(without)-10:])
	}
	// Last 7 digits (local number)
	if len(without) > 7 {
		suffixes = append(suffixes, without[len(without)-7:])
	}
	return suffixes
}

type relayContactInfo struct {
	FirstName string   `json:"first_name,omitempty"`
	LastName  string   `json:"last_name,omitempty"`
	Nickname  string   `json:"nickname,omitempty"`
	Phones    []string `json:"phones,omitempty"`
	Emails    []string `json:"emails,omitempty"`
}

// SyncContacts fetches all contacts from the relay and builds the local cache.
func (c *contactRelayClient) SyncContacts(log zerolog.Logger) {
	resp, err := c.httpClient.Get(c.baseURL + "/contacts")
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch contacts from relay")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Warn().Int("status", resp.StatusCode).Msg("Contact relay returned error")
		return
	}

	var contacts []relayContactInfo
	if err := json.NewDecoder(resp.Body).Decode(&contacts); err != nil {
		log.Warn().Err(err).Msg("Failed to decode contacts from relay")
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.byPhone = make(map[string]*imessage.Contact, len(contacts)*2)
	c.byEmail = make(map[string]*imessage.Contact, len(contacts))

	for _, rc := range contacts {
		contact := &imessage.Contact{
			FirstName: rc.FirstName,
			LastName:  rc.LastName,
			Nickname:  rc.Nickname,
			Phones:    rc.Phones,
			Emails:    rc.Emails,
		}
		// Index by all phone number variations
		for _, phone := range rc.Phones {
			for _, suffix := range phoneSuffixes(phone) {
				c.byPhone[suffix] = contact
			}
		}
		// Index by lowercase email
		for _, email := range rc.Emails {
			c.byEmail[strings.ToLower(email)] = contact
		}
	}
	c.lastSync = time.Now()

	log.Info().Int("contacts", len(contacts)).
		Int("phone_keys", len(c.byPhone)).
		Int("email_keys", len(c.byEmail)).
		Msg("Contact cache synced from relay")
}

// GetContactInfo looks up a contact by phone number or email.
func (c *contactRelayClient) GetContactInfo(identifier string) (*imessage.Contact, error) {
	if c == nil {
		return nil, nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	// Try email first (simple lowercase match)
	if !strings.HasPrefix(identifier, "+") && strings.Contains(identifier, "@") {
		if contact, ok := c.byEmail[strings.ToLower(identifier)]; ok {
			return contact, nil
		}
		return nil, nil
	}

	// Phone number: try all suffix variations
	for _, suffix := range phoneSuffixes(identifier) {
		if contact, ok := c.byPhone[suffix]; ok {
			return contact, nil
		}
	}

	return nil, nil
}
