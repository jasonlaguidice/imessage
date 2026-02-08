package connector

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lrhodin/imessage/imessage"
)

// contactRelayClient fetches contact info from a NAC relay server running on a Mac.
// This provides contact name resolution on Linux when there's no local Contacts.app.
type contactRelayClient struct {
	baseURL    string // e.g. "http://199.201.161.163:5001"
	httpClient *http.Client
}

// newContactRelayFromKey extracts the relay base URL from a hardware key (base64 JSON).
// Returns nil if no relay URL is configured.
func newContactRelayFromKey(hardwareKey string) *contactRelayClient {
	if hardwareKey == "" {
		return nil
	}

	// Decode base64
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(hardwareKey))
	if err != nil {
		// Try RawStdEncoding (no padding)
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(hardwareKey))
		if err != nil {
			return nil
		}
	}

	// Parse JSON to find nac_relay_url
	var config struct {
		NACRelayURL string `json:"nac_relay_url"`
	}
	if err := json.Unmarshal(decoded, &config); err != nil || config.NACRelayURL == "" {
		return nil
	}

	// Derive base URL from the relay URL (strip the /validation-data path)
	baseURL := config.NACRelayURL
	if idx := strings.LastIndex(baseURL, "/validation-data"); idx > 0 {
		baseURL = baseURL[:idx]
	}

	return &contactRelayClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

func (c *contactRelayClient) GetContactInfo(identifier string) (*imessage.Contact, error) {
	if c == nil {
		return nil, nil
	}

	reqURL := fmt.Sprintf("%s/contact?id=%s", c.baseURL, url.QueryEscape(identifier))
	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("contact relay request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("contact relay returned %d", resp.StatusCode)
	}

	var contact imessage.Contact
	if err := json.NewDecoder(resp.Body).Decode(&contact); err != nil {
		return nil, nil // null response = not found
	}

	if !contact.HasName() {
		return nil, nil
	}
	return &contact, nil
}
