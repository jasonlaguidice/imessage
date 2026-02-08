//go:build darwin

// chatdb.go: HTTP endpoints for querying the macOS chat.db (iMessage database)
// remotely. These endpoints let the Linux bridge perform backfill and initial
// sync by proxying chat.db queries over HTTP.
//
// Endpoints:
//   GET /chats?since_days=N          → recent chats with member info
//   GET /messages?chat_guid=X&since_ts=T
//   GET /messages?chat_guid=X&before_ts=T&limit=N
//   GET /attachment?path=X           → raw attachment file bytes

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Apple epoch: 2001-01-01 00:00:00 UTC (nanosecond timestamps in chat.db)
var appleEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// RelayMessage is the JSON format sent to the bridge for each message.
type RelayMessage struct {
	GUID         string            `json:"guid"`
	Timestamp    int64             `json:"timestamp_ms"` // Unix millis
	Subject      string            `json:"subject,omitempty"`
	Text         string            `json:"text"`
	ChatGUID     string            `json:"chat_guid"`
	SenderID     string            `json:"sender_id,omitempty"`
	SenderSvc    string            `json:"sender_service,omitempty"`
	IsFromMe     bool              `json:"is_from_me"`
	IsEmote      bool              `json:"is_emote,omitempty"`
	IsAudio      bool              `json:"is_audio_message,omitempty"`
	ReplyToGUID  string            `json:"reply_to_guid,omitempty"`
	ReplyToPart  int               `json:"reply_to_part,omitempty"`
	TapbackGUID  string            `json:"tapback_guid,omitempty"`
	TapbackType  int               `json:"tapback_type,omitempty"`
	GroupTitle    string            `json:"group_title,omitempty"`
	ItemType     int               `json:"item_type"`
	GroupAction  int               `json:"group_action_type,omitempty"`
	ThreadID     string            `json:"thread_id,omitempty"`
	Attachments  []RelayAttachment `json:"attachments,omitempty"`
	Service      string            `json:"service,omitempty"`
}

// RelayAttachment is attachment metadata (the bridge fetches content via /attachment).
type RelayAttachment struct {
	GUID       string `json:"guid"`
	PathOnDisk string `json:"path_on_disk"`
	MimeType   string `json:"mime_type,omitempty"`
	FileName   string `json:"file_name"`
}

// RelayChatInfo is returned by /chats for each conversation.
type RelayChatInfo struct {
	ChatGUID    string   `json:"chat_guid"`
	DisplayName string   `json:"display_name,omitempty"`
	Identifier  string   `json:"identifier"`
	Service     string   `json:"service"`
	Members     []string `json:"members,omitempty"`
	ThreadID    string   `json:"thread_id,omitempty"`
}

var chatDB *sql.DB

// installAppBundle creates a macOS .app bundle at ~/Applications/nac-relay.app.
// macOS properly tracks .app bundles in TCC (Full Disk Access list) — raw
// binaries launched by launchd don't appear. The LaunchAgent references the
// binary inside the .app bundle so TCC grants persist.
func installAppBundle() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	appDir := filepath.Join(home, "Applications", "nac-relay.app")
	macosDir := filepath.Join(appDir, "Contents", "MacOS")
	if err := os.MkdirAll(macosDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create .app bundle: %w", err)
	}

	// Write Info.plist
	infoPlist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>com.imessage.nac-relay</string>
	<key>CFBundleName</key>
	<string>nac-relay</string>
	<key>CFBundleDisplayName</key>
	<string>iMessage Bridge Relay</string>
	<key>CFBundleExecutable</key>
	<string>nac-relay</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleVersion</key>
	<string>1.0</string>
	<key>CFBundleShortVersionString</key>
	<string>1.0</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
	<key>LSMinimumSystemVersion</key>
	<string>13.0</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSContactsUsageDescription</key>
	<string>nac-relay needs access to your contacts to provide contact names for bridged iMessage conversations.</string>
</dict>
</plist>`
	if err := os.WriteFile(filepath.Join(appDir, "Contents", "Info.plist"), []byte(infoPlist), 0644); err != nil {
		return "", fmt.Errorf("failed to write Info.plist: %w", err)
	}

	// Copy our own binary into the .app bundle
	selfPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to find own executable: %w", err)
	}
	selfPath, _ = filepath.EvalSymlinks(selfPath)

	destPath := filepath.Join(macosDir, "nac-relay")
	srcData, err := os.ReadFile(selfPath)
	if err != nil {
		return "", fmt.Errorf("failed to read own binary: %w", err)
	}
	if err := os.WriteFile(destPath, srcData, 0755); err != nil {
		return "", fmt.Errorf("failed to write binary to .app bundle: %w", err)
	}

	// Codesign so macOS recognizes it as a proper app for TCC prompts
	exec.Command("codesign", "--force", "--sign", "-", appDir).Run()

	log.Printf("Installed .app bundle: %s", appDir)
	return destPath, nil
}

// runSetup installs the .app bundle and LaunchAgent plist, then starts
// the service. Permissions (FDA, Contacts) are handled automatically on
// first run — macOS shows system prompts when the relay tries to access
// chat.db and contacts.
func runSetup() {
	log.Println("=== nac-relay setup ===")
	log.Println()

	// Step 1: Create .app bundle
	log.Println("Installing .app bundle...")
	binPath, err := installAppBundle()
	if err != nil {
		log.Fatalf("Failed to install .app bundle: %v", err)
	}
	log.Printf("✓ Installed: %s", binPath)
	log.Println()

	// Step 2: Install LaunchAgent
	home, _ := os.UserHomeDir()
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.imessage.nac-relay.plist")
	plistContent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.imessage.nac-relay</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/tmp/nac-relay.log</string>
	<key>StandardErrorPath</key>
	<string>/tmp/nac-relay.log</string>
</dict>
</plist>`, binPath)

	exec.Command("launchctl", "unload", plistPath).Run()
	os.MkdirAll(filepath.Dir(plistPath), 0755)
	if err := os.WriteFile(plistPath, []byte(plistContent), 0644); err != nil {
		log.Fatalf("Failed to write LaunchAgent: %v", err)
	}
	log.Printf("✓ LaunchAgent: %s", plistPath)

	// Step 3: Start the service
	if err := exec.Command("launchctl", "load", plistPath).Run(); err != nil {
		log.Printf("WARNING: failed to start service: %v", err)
	} else {
		log.Println("✓ Service started")
	}

	log.Println()
	log.Println("=== Setup complete! ===")
	log.Println("Permissions (FDA, Contacts) will be prompted on first run.")
	log.Println("Logs: tail -f /tmp/nac-relay.log")
}

func openChatDB() (*sql.DB, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("can't get home dir: %w", err)
	}
	dbPath := filepath.Join(home, "Library", "Messages", "chat.db")
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro", dbPath))
	if err != nil {
		return nil, fmt.Errorf("can't open chat.db: %w", err)
	}
	// Verify access
	var maxRowID sql.NullInt64
	if err := db.QueryRow("SELECT MAX(ROWID) FROM message").Scan(&maxRowID); err != nil {
		return nil, fmt.Errorf("can't read chat.db (Full Disk Access needed): %w", err)
	}
	return db, nil
}

func promptForFDA() {
	log.Println("Full Disk Access not granted — prompting user")
	script := `display dialog "nac-relay needs Full Disk Access to read your iMessage history for backfill.\n\nClick OK to open System Settings, then enable the toggle for nac-relay." with title "iMessage Bridge Relay" buttons {"OK"} default button "OK"`
	exec.Command("osascript", "-e", script).Run()
	exec.Command("open", "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles").Run()
}


func registerChatDBEndpoints() {
	var err error
	chatDB, err = openChatDB()
	if err != nil {
		log.Printf("WARNING: chat.db not accessible: %v", err)
		log.Println("  Backfill endpoints will be disabled.")
		log.Println("  Run 'nac-relay --setup' interactively first to grant Full Disk Access.")
		return
	}
	log.Println("chat.db: opened successfully, backfill endpoints available")

	http.HandleFunc("/chats", handleChats)
	http.HandleFunc("/chat-info", handleChatInfo)
	http.HandleFunc("/messages", handleMessages)
	http.HandleFunc("/attachment", handleAttachment)
}

func handleChats(w http.ResponseWriter, r *http.Request) {
	sinceDaysStr := r.URL.Query().Get("since_days")
	sinceDays := 365
	if sinceDaysStr != "" {
		if v, err := strconv.Atoi(sinceDaysStr); err == nil && v > 0 {
			sinceDays = v
		}
	}

	minDate := time.Now().AddDate(0, 0, -sinceDays)
	minApple := minDate.UnixNano() - appleEpoch.UnixNano()

	rows, err := chatDB.Query(`
		SELECT chat.guid, chat.group_id FROM message
		JOIN chat_message_join ON chat_message_join.message_id = message.ROWID
		JOIN chat              ON chat_message_join.chat_id = chat.ROWID
		WHERE message.date > ?
		  AND message.item_type = 0
		  AND COALESCE(message.associated_message_guid, '') = ''
		GROUP BY chat.guid, chat.group_id
		ORDER BY MAX(message.date) DESC
	`, minApple)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var chats []RelayChatInfo
	for rows.Next() {
		var chatGUID, threadID string
		if err := rows.Scan(&chatGUID, &threadID); err != nil {
			continue
		}
		info := getChatInfo(chatGUID)
		if info == nil {
			continue
		}
		info.ThreadID = threadID
		chats = append(chats, *info)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chats)
	log.Printf("Served %d chats (since %d days)", len(chats), sinceDays)
}

func getChatInfo(chatGUID string) *RelayChatInfo {
	row := chatDB.QueryRow(`
		SELECT chat_identifier, service_name, COALESCE(display_name, ''), group_id
		FROM chat WHERE guid = ?
	`, chatGUID)

	var info RelayChatInfo
	var identifier, service, displayName, groupID string
	if err := row.Scan(&identifier, &service, &displayName, &groupID); err != nil {
		return nil
	}
	info.ChatGUID = chatGUID
	info.Identifier = identifier
	info.Service = service
	info.DisplayName = displayName
	info.ThreadID = groupID

	// Get members for group chats
	memberRows, err := chatDB.Query(`
		SELECT handle.id FROM chat
		JOIN chat_handle_join ON chat_handle_join.chat_id = chat.ROWID
		JOIN handle ON chat_handle_join.handle_id = handle.ROWID
		WHERE chat.guid = ?
	`, chatGUID)
	if err == nil {
		defer memberRows.Close()
		for memberRows.Next() {
			var member string
			if memberRows.Scan(&member) == nil && member != "" {
				info.Members = append(info.Members, member)
			}
		}
	}

	return &info
}

func handleChatInfo(w http.ResponseWriter, r *http.Request) {
	guid := r.URL.Query().Get("guid")
	if guid == "" {
		http.Error(w, "missing ?guid= parameter", http.StatusBadRequest)
		return
	}
	info := getChatInfo(guid)
	w.Header().Set("Content-Type", "application/json")
	if info == nil {
		w.Write([]byte("null"))
		return
	}
	json.NewEncoder(w).Encode(info)
}

func handleMessages(w http.ResponseWriter, r *http.Request) {
	chatGUID := r.URL.Query().Get("chat_guid")
	if chatGUID == "" {
		http.Error(w, "missing ?chat_guid= parameter", http.StatusBadRequest)
		return
	}

	sinceTsStr := r.URL.Query().Get("since_ts")
	beforeTsStr := r.URL.Query().Get("before_ts")
	limitStr := r.URL.Query().Get("limit")

	var rows *sql.Rows
	var err error

	baseQuery := `
		SELECT
			message.ROWID, message.guid, message.date,
			COALESCE(message.subject, ''), COALESCE(message.text, ''),
			message.attributedBody,
			chat.guid,
			COALESCE(sender_handle.id, ''), COALESCE(sender_handle.service, ''),
			message.is_from_me, message.is_emote, message.is_audio_message,
			COALESCE(message.thread_originator_guid, ''),
			COALESCE(message.thread_originator_part, ''),
			COALESCE(message.associated_message_guid, ''), message.associated_message_type,
			COALESCE(message.group_title, ''),
			message.item_type, message.group_action_type,
			chat.group_id
		FROM message
		JOIN chat_message_join ON chat_message_join.message_id = message.ROWID
		JOIN chat ON chat_message_join.chat_id = chat.ROWID
		LEFT JOIN handle sender_handle ON message.handle_id = sender_handle.ROWID
	`

	if sinceTsStr != "" {
		sinceMs, _ := strconv.ParseInt(sinceTsStr, 10, 64)
		sinceApple := (sinceMs*1000000) - (appleEpoch.UnixNano() - time.Unix(0, 0).UnixNano())
		// sinceMs is unix millis. Convert to apple epoch nanos.
		sinceTime := time.UnixMilli(sinceMs)
		sinceApple = sinceTime.UnixNano() - appleEpoch.UnixNano()
		rows, err = chatDB.Query(baseQuery+`
			WHERE (chat.guid = ? OR ? = '') AND message.date > ?
			ORDER BY message.date ASC
		`, chatGUID, chatGUID, sinceApple)
	} else if beforeTsStr != "" {
		beforeMs, _ := strconv.ParseInt(beforeTsStr, 10, 64)
		beforeTime := time.UnixMilli(beforeMs)
		beforeApple := beforeTime.UnixNano() - appleEpoch.UnixNano()
		limit := 50
		if limitStr != "" {
			if v, e := strconv.Atoi(limitStr); e == nil && v > 0 {
				limit = v
			}
		}
		rows, err = chatDB.Query(baseQuery+`
			WHERE (chat.guid = ? OR ? = '') AND message.date < ?
			ORDER BY message.date DESC
			LIMIT ?
		`, chatGUID, chatGUID, beforeApple, limit)
	} else {
		http.Error(w, "need since_ts or before_ts parameter", http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var messages []RelayMessage
	for rows.Next() {
		var msg RelayMessage
		var rowID int
		var dateApple int64
		var attributedBody []byte
		var threadOriginatorPart string
		var tapbackGUID string
		var tapbackType int
		var groupTitle string
		var senderID, senderSvc string

		err := rows.Scan(
			&rowID, &msg.GUID, &dateApple,
			&msg.Subject, &msg.Text,
			&attributedBody,
			&msg.ChatGUID,
			&senderID, &senderSvc,
			&msg.IsFromMe, &msg.IsEmote, &msg.IsAudio,
			&msg.ReplyToGUID, &threadOriginatorPart,
			&tapbackGUID, &tapbackType,
			&groupTitle, &msg.ItemType, &msg.GroupAction,
			&msg.ThreadID,
		)
		if err != nil {
			log.Printf("Error scanning message row: %v", err)
			continue
		}

		// If text column is empty, decode attributedBody (modern iMessage stores
		// styled text in an NSKeyedArchive blob instead of the text column)
		if msg.Text == "" && len(attributedBody) > 0 {
			decoded := decodeAttributedBodyJSON(attributedBody)
			if decoded != "" {
				var ab struct {
					Content string `json:"content"`
				}
				if json.Unmarshal([]byte(decoded), &ab) == nil && ab.Content != "" {
					// Strip U+FFFC (object replacement character) used as
					// inline attachment placeholders in NSAttributedString
					cleaned := strings.ReplaceAll(ab.Content, "\uFFFC", "")
					msg.Text = strings.TrimSpace(cleaned)
				}
			}
		}

		msgTime := time.Unix(appleEpoch.Unix(), dateApple)
		msg.Timestamp = msgTime.UnixMilli()
		msg.SenderID = senderID
		msg.SenderSvc = senderSvc
		msg.GroupTitle = groupTitle
		msg.Service = senderSvc
		if msg.IsFromMe {
			msg.SenderID = ""
		}

		if threadOriginatorPart != "" {
			msg.ReplyToPart, _ = strconv.Atoi(strings.Split(threadOriginatorPart, ":")[0])
		}

		if tapbackGUID != "" {
			msg.TapbackGUID = tapbackGUID
			msg.TapbackType = tapbackType
		}

		// Fetch attachments for this message
		attRows, err := chatDB.Query(`
			SELECT guid, COALESCE(filename, ''), COALESCE(mime_type, ''), transfer_name
			FROM attachment
			JOIN message_attachment_join ON message_attachment_join.attachment_id = attachment.ROWID
			WHERE message_attachment_join.message_id = ?
			ORDER BY ROWID
		`, rowID)
		if err == nil {
			for attRows.Next() {
				var att RelayAttachment
				if attRows.Scan(&att.GUID, &att.PathOnDisk, &att.MimeType, &att.FileName) == nil {
					msg.Attachments = append(msg.Attachments, att)
				}
			}
			attRows.Close()
		}

		messages = append(messages, msg)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(messages)
	log.Printf("Served %d messages for chat %s", len(messages), chatGUID)
}

func handleAttachment(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "missing ?path= parameter", http.StatusBadRequest)
		return
	}

	// Expand ~/
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			http.Error(w, "can't expand path", http.StatusInternalServerError)
			return
		}
		path = filepath.Join(home, path[2:])
	}

	// Security: only allow files under ~/Library/Messages/Attachments
	home, _ := os.UserHomeDir()
	absPath, err := filepath.Abs(path)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	allowedPrefix := filepath.Join(home, "Library", "Messages", "Attachments")
	if !strings.HasPrefix(absPath, allowedPrefix) {
		http.Error(w, "path not allowed", http.StatusForbidden)
		return
	}

	f, err := os.Open(absPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("can't open file: %v", err), http.StatusNotFound)
		return
	}
	defer f.Close()

	stat, _ := f.Stat()
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	// Try to detect content type from extension
	ext := filepath.Ext(absPath)
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		w.Header().Set("Content-Type", "image/jpeg")
	case ".png":
		w.Header().Set("Content-Type", "image/png")
	case ".gif":
		w.Header().Set("Content-Type", "image/gif")
	case ".heic":
		w.Header().Set("Content-Type", "image/heic")
	case ".mp4", ".m4v":
		w.Header().Set("Content-Type", "video/mp4")
	case ".mov":
		w.Header().Set("Content-Type", "video/quicktime")
	case ".caf":
		w.Header().Set("Content-Type", "audio/x-caf")
	case ".m4a":
		w.Header().Set("Content-Type", "audio/mp4")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	io.Copy(w, f)
	log.Printf("Served attachment: %s (%d bytes)", filepath.Base(absPath), stat.Size())
}
