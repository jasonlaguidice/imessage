// ck-probe: Probe CloudKit Messages directly to count records and find gaps.
// Usage: go run ./cmd/ck-probe
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"go.mau.fi/mautrix-imessage/pkg/rustpushgo"
)

func main() {
	// Load session from server copy
	sessionPath := "/tmp/server-session.json"
	if len(os.Args) > 1 {
		sessionPath = os.Args[1]
	}

	sessionData, err := os.ReadFile(sessionPath)
	if err != nil {
		log.Fatalf("Failed to read session: %v", err)
	}

	var session struct {
		IDSIdentity     string `json:"ids_identity"`
		APSState        string `json:"aps_state"`
		IDSUsers        string `json:"ids_users"`
		PreferredHandle string `json:"preferred_handle"`
		Platform        string `json:"platform"`
		HardwareKey     string `json:"hardware_key"`
		DeviceID        string `json:"device_id"`

		AccountUsername       string `json:"account_username"`
		AccountHashedPwdHex  string `json:"account_hashed_password_hex"`
		AccountPET           string `json:"account_pet"`
		AccountADSID         string `json:"account_adsid"`
		AccountDSID          string `json:"account_dsid"`
		AccountSPDBase64     string `json:"account_spd_base64"`
		MMEDelegateJSON      string `json:"mme_delegate_json"`
	}
	if err := json.Unmarshal(sessionData, &session); err != nil {
		log.Fatalf("Failed to parse session: %v", err)
	}

	fmt.Println("=== CloudKit Messages Probe ===")
	fmt.Printf("Account: %s (DSID: %s)\n", session.AccountUsername, session.AccountDSID)
	fmt.Printf("Device: %s\n", session.DeviceID)
	fmt.Println()

	// Create the rustpush client
	// We need to work from the state directory that has the anisette data
	stateDir := os.Getenv("STATE_DIR")
	if stateDir == "" {
		stateDir = "/tmp/ck-probe-state"
	}
	os.MkdirAll(stateDir, 0700)

	// Write config.yaml
	configPath := stateDir + "/config.yaml"
	os.WriteFile(configPath, []byte("{}"), 0600)

	// Restore client from session
	fmt.Println("Restoring client from session...")
	client, err := rustpushgo.NewClient(
		session.IDSIdentity,
		session.APSState,
		session.IDSUsers,
		session.PreferredHandle,
		session.Platform,
		session.HardwareKey,
		session.DeviceID,
		session.AccountUsername,
		session.AccountHashedPwdHex,
		session.AccountPET,
		session.AccountADSID,
		session.AccountDSID,
		session.AccountSPDBase64,
		session.MMEDelegateJSON,
	)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	fmt.Println("Client created successfully!")
	fmt.Println()

	// Now do a full sync from scratch (no continuation token) and count
	fmt.Println("=== Full CloudKit Message Sync (from scratch) ===")
	var totalMessages int
	var totalDeleted int
	var newestTS int64
	var newestGUID string
	var newestChat string
	chatCounts := make(map[string]int)

	var token *string = nil
	for page := 0; page < 512; page++ {
		resp, err := client.CloudSyncMessages(token)
		if err != nil {
			log.Fatalf("Page %d failed: %v", page, err)
		}

		for _, msg := range resp.Messages {
			if msg.Deleted {
				totalDeleted++
				continue
			}
			totalMessages++
			chatCounts[msg.CloudChatId]++
			if msg.TimestampMs > newestTS {
				newestTS = msg.TimestampMs
				newestGUID = msg.Guid
				newestChat = msg.CloudChatId
			}
		}

		fmt.Printf("  page %d: %d messages (status=%d, done=%v)\n",
			page, len(resp.Messages), resp.Status, resp.Done)

		if resp.Done {
			break
		}
		token = resp.ContinuationToken
	}

	newestTime := time.UnixMilli(newestTS)
	fmt.Println()
	fmt.Printf("Total messages:  %d\n", totalMessages)
	fmt.Printf("Total deleted:   %d\n", totalDeleted)
	fmt.Printf("Unique chats:    %d\n", len(chatCounts))
	fmt.Printf("Newest message:  %s (GUID: %s)\n", newestTime.Format(time.RFC3339), newestGUID)
	fmt.Printf("Newest chat_id:  %s\n", newestChat)
	fmt.Println()

	// Check specifically for Kat's chat
	katChat := ""
	for chatID, count := range chatCounts {
		// We don't know Kat's chat_id in CloudKit, but let's list top chats by count
		_ = count
		_ = chatID
	}

	// Sort chats by count descending
	type chatEntry struct {
		ID    string
		Count int
	}
	var entries []chatEntry
	for id, count := range chatCounts {
		entries = append(entries, chatEntry{id, count})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Count > entries[j].Count
	})

	fmt.Println("=== Top 20 chats by message count ===")
	for i, e := range entries {
		if i >= 20 {
			break
		}
		fmt.Printf("  %3d msgs  %s\n", e.Count, e.ID)
	}

	// Now search for the specific GUID we know is missing
	missingGUID := "691BCBCA-7A83-4072-8441-EB5883503BB3"
	fmt.Printf("\n=== Searching for missing message GUID: %s ===\n", missingGUID)

	// Do another full scan looking for this specific GUID
	token = nil
	found := false
	for page := 0; page < 512; page++ {
		resp, err := client.CloudSyncMessages(token)
		if err != nil {
			log.Fatalf("Search page %d failed: %v", page, err)
		}
		for _, msg := range resp.Messages {
			if msg.Guid == missingGUID || msg.RecordName == missingGUID {
				fmt.Printf("  FOUND! guid=%s chat=%s ts=%s sender=%s from_me=%v\n",
					msg.Guid, msg.CloudChatId,
					time.UnixMilli(msg.TimestampMs).Format(time.RFC3339),
					msg.Sender, msg.IsFromMe)
				found = true
			}
		}
		if resp.Done {
			break
		}
		token = resp.ContinuationToken
	}
	if !found {
		fmt.Println("  NOT FOUND — CloudKit does not have this record")
	}

	_ = katChat
}
