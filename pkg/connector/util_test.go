package connector

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/crypto/goolm/session"
	"maunium.net/go/mautrix/crypto/olm"
	"maunium.net/go/mautrix/event"
)

func TestNormalizePhone(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"+1 (555) 123-4567", "+15551234567"},
		{"15551234567", "15551234567"},
		{"+1-555-123-4567", "+15551234567"},
		{"", ""},
		{"+", "+"},
		{"abc", ""},
		{"+44 20 7946 0958", "+442079460958"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizePhone(tt.input)
			if got != tt.want {
				t.Errorf("normalizePhone(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPhoneSuffixes(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"+15551234567", []string{"+15551234567", "15551234567", "5551234567", "1234567"}},
		{"+4412345678901", []string{"+4412345678901", "4412345678901", "2345678901", "5678901"}},
		{"5551234", []string{"5551234"}},
		{"+1234567", []string{"+1234567", "1234567"}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := phoneSuffixes(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("phoneSuffixes(%q) returned %d items, want %d: %v", tt.input, len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("phoneSuffixes(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestStripNonBase64(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"SGVsbG8=", "SGVsbG8="},
		{"SGVs bG8=", "SGVsbG8="},
		{"abc" + "!@#" + "def", "abcdef"},
		{"", ""},
		{"ABCD+/==", "ABCD+/=="},
	}
	for _, tt := range tests {
		got := stripNonBase64(tt.input)
		if got != tt.want {
			t.Errorf("stripNonBase64(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestStripIdentifierPrefix(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"tel:+15551234567", "+15551234567"},
		{"mailto:user@example.com", "user@example.com"},
		{"+15551234567", "+15551234567"},
		{"user@example.com", "user@example.com"},
		{"", ""},
		{"tel:mailto:nested", "nested"},
	}
	for _, tt := range tests {
		got := stripIdentifierPrefix(tt.input)
		if got != tt.want {
			t.Errorf("stripIdentifierPrefix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestAddIdentifierPrefix(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"+15551234567", "tel:+15551234567"},
		{"user@example.com", "mailto:user@example.com"},
		{"12345", "tel:12345"},
		{"tel:+1555", "tel:+1555"},
		{"mailto:a@b.com", "mailto:a@b.com"},
		{"some-guid", "some-guid"},
		{"", ""},
	}
	for _, tt := range tests {
		got := addIdentifierPrefix(tt.input)
		if got != tt.want {
			t.Errorf("addIdentifierPrefix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsNumeric(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"12345", true},
		{"0", true},
		{"", false},
		{"12a45", false},
		{"+1234", false},
		{"123.45", false},
	}
	for _, tt := range tests {
		got := isNumeric(tt.input)
		if got != tt.want {
			t.Errorf("isNumeric(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestIdentifierToDisplaynameParams(t *testing.T) {
	tests := []struct {
		input string
		phone string
		email string
		id    string
	}{
		{"tel:+15551234567", "+15551234567", "", "+15551234567"},
		{"mailto:user@example.com", "", "user@example.com", "user@example.com"},
		{"+15551234567", "+15551234567", "", "+15551234567"},
		{"some-guid", "", "", "some-guid"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := identifierToDisplaynameParams(tt.input)
			if got.Phone != tt.phone {
				t.Errorf("Phone = %q, want %q", got.Phone, tt.phone)
			}
			if got.Email != tt.email {
				t.Errorf("Email = %q, want %q", got.Email, tt.email)
			}
			if got.ID != tt.id {
				t.Errorf("ID = %q, want %q", got.ID, tt.id)
			}
		})
	}
}

// --- oversized text splitting ---
//
// The contract buildTextParts has to hold: every emitted part's marshaled
// content stays under maxTextContentBytes (so the encrypted event fits Matrix's
// 65,536-byte PDU limit), the chunks concatenate back to the exact input, and
// the first part keeps the empty part ID that replies, tapbacks and edits
// target. The PDU-level check below encrypts with a real megolm session rather
// than modelling the expansion.

func repeatTo(unit string, n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(unit)
	}
	s := b.String()
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

var splitCorpus = []struct {
	name    string
	subject string
	text    string
}{
	{"short message stays one part", "", "hello there"},
	{"empty text", "", ""},
	{"whitespace only", "", "   \n  "},
	{"subject with no text", "Just a subject", ""},
	{"63365B prose", "", repeatTo("The quick brown fox jumps over the lazy dog.\nAnother line here.\n\n", 63365)},
	{"316829B prose", "", repeatTo("The quick brown fox jumps over the lazy dog.\nAnother line here.\n\n", 316829)},
	{"63365B prose with subject", "Dinner plans", repeatTo("The quick brown fox jumps over the lazy dog.\n", 63365)},
	{"316829B emoji", "", repeatTo("Look 🎉🚀 family 👨‍👩‍👧‍👦 flags 🇯🇵 text.\n", 316829)},
	{"63365B emoji with subject", "Trip 🎉", repeatTo("Look 🎉🚀 family 👨‍👩‍👧‍👦 flags 🇯🇵 text.\n", 63365)},
	{"all angle brackets with subject", "Subj", repeatTo("<", 63365)},
	{"all control characters", "", repeatTo("\x01", 63365)},
	{"control characters with subject", "S", repeatTo("\x01", 63365)},
	{"no whitespace at all", "", repeatTo("x", 100000)},
	{"all newlines", "", repeatTo("\n", 63365)},
	{"four-byte runes only", "", repeatTo("𝕏", 63365)},
	// Overhead that splitting cannot reduce: shrinking the chunk does nothing
	// about a huge subject, so these force the bounding/degradation path.
	{"huge subject, short text", repeatTo("s", 60000), "hello"},
	{"huge subject, huge text", repeatTo("s", 60000), repeatTo("word ", 200000)},
	{"huge multibyte subject", repeatTo("é", 60000), "hello"},
}

// splitCorpusPreviews is attached to the corpus by the encrypted-PDU tests so
// preview-bearing parts are validated against a real megolm event, not only
// against measuredSize — the same function the implementation uses to decide a
// part is fine. Unbounded previews were round one's first defect.
var splitCorpusPreviews = []*event.BeeperLinkPreview{
	{MatchedURL: repeatTo("https://example.com/", 30000), LinkPreview: event.LinkPreview{
		Title:        repeatTo("t", 50000),
		Description:  repeatTo("d", 50000),
		SiteName:     repeatTo("s", 50000),
		CanonicalURL: repeatTo("https://example.com/", 30000),
	}},
	{MatchedURL: "https://b.example", LinkPreview: event.LinkPreview{Description: repeatTo("d", 50000)}},
}

func TestBuildTextPartsContract(t *testing.T) {
	for _, tt := range splitCorpus {
		t.Run(tt.name, func(t *testing.T) {
			parts := buildTextParts(event.MsgText, tt.subject, tt.text, nil, false)
			if len(parts) == 0 {
				t.Fatal("expected at least one part")
			}
			if parts[0].ID != "" {
				t.Errorf("first part ID = %q, want empty so replies/tapbacks still target it", parts[0].ID)
			}
			for i, p := range parts[1:] {
				want := networkid.PartID(fmt.Sprintf("text%03d", i+1))
				if p.ID != want {
					t.Errorf("part %d ID = %q, want %q", i+1, p.ID, want)
				}
				if p.Content.FormattedBody != "" || p.Content.Format != "" {
					t.Errorf("part %d carries formatting; only the first part should", i+1)
				}
			}
			for i, p := range parts {
				encoded, err := json.Marshal(p.Content)
				if err != nil {
					t.Fatalf("part %d: marshal: %v", i, err)
				}
				if len(encoded) > maxTextContentBytes {
					t.Errorf("part %d marshaled content = %dB, over the %dB budget",
						i, len(encoded), maxTextContentBytes)
				}
				if p.Content.MsgType != event.MsgText {
					t.Errorf("part %d msgtype = %q, want %q", i, p.Content.MsgType, event.MsgText)
				}
			}
		})
	}
}

func TestBuildTextPartsIsLossless(t *testing.T) {
	for _, tt := range splitCorpus {
		t.Run(tt.name, func(t *testing.T) {
			parts := buildTextParts(event.MsgText, tt.subject, tt.text, nil, false)
			var rebuilt strings.Builder
			for i, p := range parts {
				body := p.Content.Body
				if i == 0 && tt.subject != "" {
					// The first part carries the subject as a markdown prefix,
					// or is the subject alone when there is no text at all.
					prefix := "**" + truncateBytes(tt.subject, maxSubjectBytes) + "**\n"
					if strings.HasPrefix(body, prefix) {
						body = body[len(prefix):]
					} else {
						body = ""
					}
				}
				rebuilt.WriteString(body)
			}
			if rebuilt.String() != tt.text {
				t.Errorf("round trip lost data: got %dB, want %dB", rebuilt.Len(), len(tt.text))
			}
		})
	}
}

func TestBuildTextPartsSubjectAndPreviewsOnFirstPartOnly(t *testing.T) {
	previews := []*event.BeeperLinkPreview{{MatchedURL: "https://example.com"}}
	text := repeatTo("word ", 100000)
	parts := buildTextParts(event.MsgText, "Subject line", text, previews, false)
	if len(parts) < 2 {
		t.Fatalf("expected a split, got %d part(s)", len(parts))
	}
	if !strings.HasPrefix(parts[0].Content.Body, "**Subject line**\n") {
		t.Errorf("first part body does not start with the subject: %.40q", parts[0].Content.Body)
	}
	if len(parts[0].Content.BeeperLinkPreviews) != 1 {
		t.Errorf("first part should carry the link previews")
	}
	for i, p := range parts[1:] {
		if len(p.Content.BeeperLinkPreviews) != 0 {
			t.Errorf("part %d repeats the link previews", i+1)
		}
		if strings.Contains(p.Content.Body, "Subject line") {
			t.Errorf("part %d repeats the subject", i+1)
		}
	}
}

func TestBuildTextPartsPreservesEmoteType(t *testing.T) {
	parts := buildTextParts(event.MsgEmote, "", repeatTo("waves ", 100000), nil, false)
	for i, p := range parts {
		if p.Content.MsgType != event.MsgEmote {
			t.Errorf("part %d msgtype = %q, want %q", i, p.Content.MsgType, event.MsgEmote)
		}
	}
}

func TestSplitTextChunksNeverSplitsRunes(t *testing.T) {
	for _, unit := range []string{"é", "𝕏", "👨‍👩‍👧‍👦", "🇯🇵"} {
		text := repeatTo(unit, 20000)
		for _, chunk := range splitTextChunks(text, 1000) {
			if !utf8.ValidString(chunk) {
				t.Errorf("unit %q: chunk is not valid UTF-8", unit)
			}
		}
	}
}

func TestSplitTextChunksDegenerateLimits(t *testing.T) {
	// Guard against the chunks(0) shape: a non-positive limit must not loop or
	// panic, it must return the input untouched.
	for _, max := range []int{0, -1, -1000} {
		got := splitTextChunks("some text", max)
		if len(got) != 1 || got[0] != "some text" {
			t.Errorf("max=%d: got %q, want the input unchanged", max, got)
		}
	}
	if got := splitTextChunks("", 100); len(got) != 1 || got[0] != "" {
		t.Errorf("empty input: got %q, want one empty chunk", got)
	}
}

// TestBuildTextPartsFitsRealEncryptedPDU encrypts every emitted part with a real
// megolm session and assembles a pessimistic Matrix PDU around it, so the size
// guarantee rests on measurement rather than on arithmetic about base64.
// matrixPDULimit is the spec cap on the serialized size of a single event.
const matrixPDULimit = 65536

// encryptedPDUSize encrypts content with a real megolm session and assembles a
// deliberately pessimistic Matrix PDU around it — more auth/prev events and a
// fatter unsigned block than a bridged message carries — so size guarantees rest
// on measurement rather than on arithmetic about base64 expansion.
func encryptedPDUSize(t *testing.T, sess olm.OutboundGroupSession, content *event.MessageEventContent) int {
	t.Helper()
	const roomID = "!AbCdEfGhIjKlMnOpQrStUv:beeper.local"
	evtID := "$" + strings.Repeat("A", 43)

	contentJSON, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	plaintext := fmt.Sprintf(`{"type":"m.room.message","room_id":%q,"content":%s}`, roomID, contentJSON)
	ciphertext, err := sess.Encrypt([]byte(plaintext))
	if err != nil {
		t.Fatalf("megolm encrypt: %v", err)
	}
	pdu := map[string]any{
		"auth_events":      []string{evtID, evtID, evtID, evtID, evtID, evtID},
		"prev_events":      []string{evtID, evtID, evtID, evtID, evtID},
		"depth":            123456,
		"hashes":           map[string]string{"sha256": strings.Repeat("h", 43)},
		"origin_server_ts": 1753440000000,
		"room_id":          roomID,
		"sender":           "@imessage_15551234567:beeper.local",
		"type":             "m.room.encrypted",
		"signatures": map[string]map[string]string{
			"beeper.local": {"ed25519:a_AbCd": strings.Repeat("s", 86)},
		},
		"unsigned": map[string]any{
			"age_ts":         1753440000000,
			"transaction_id": strings.Repeat("t", 64),
		},
		"content": map[string]any{
			"algorithm":  "m.megolm.v1.aes-sha2",
			"sender_key": strings.Repeat("k", 43),
			"device_id":  "ABCDEFGHIJ",
			"session_id": string(sess.ID()),
			"ciphertext": string(ciphertext),
		},
	}
	encoded, err := json.Marshal(pdu)
	if err != nil {
		t.Fatalf("marshal pdu: %v", err)
	}
	return len(encoded)
}

func TestBuildTextPartsFitsRealEncryptedPDU(t *testing.T) {
	session.Register()
	sess, err := olm.NewOutboundGroupSession()
	if err != nil {
		t.Fatalf("create megolm session: %v", err)
	}

	for _, tt := range splitCorpus {
		t.Run(tt.name, func(t *testing.T) {
			for i, p := range buildTextParts(event.MsgText, tt.subject, tt.text, splitCorpusPreviews, false) {
				if got := encryptedPDUSize(t, sess, p.Content); got > matrixPDULimit {
					t.Errorf("part %d: encrypted PDU = %dB, over the %dB Matrix limit",
						i, got, matrixPDULimit)
				}
			}
		})
	}
}

// TestEditPartsFitRealEncryptedPDU covers what actually goes on the wire for an
// inbound edit. sendConvertedEdit calls SetEdit on every modified part after the
// converter returns, which copies the whole content into m.new_content — so the
// part measured by TestBuildTextPartsFitsRealEncryptedPDU is not the event that
// gets sent. Measure the post-SetEdit content instead.
func TestEditPartsFitRealEncryptedPDU(t *testing.T) {
	session.Register()
	sess, err := olm.NewOutboundGroupSession()
	if err != nil {
		t.Fatalf("create megolm session: %v", err)
	}

	for _, tt := range splitCorpus {
		t.Run(tt.name, func(t *testing.T) {
			for i, p := range buildTextParts(event.MsgText, tt.subject, tt.text, splitCorpusPreviews, true) {
				p.Content.SetEdit(editProbeEventID)
				if got := encryptedPDUSize(t, sess, p.Content); got > matrixPDULimit {
					t.Errorf("part %d: edit PDU = %dB, over the %dB Matrix limit",
						i, got, matrixPDULimit)
				}
			}
		})
	}
}

// TestEditPartsStayUnderBudgetAfterSetEdit checks the edit path measures what
// actually ships: sendConvertedEdit calls SetEdit after the converter returns,
// copying the whole content into m.new_content. buildTextParts(forEdit=true)
// measures the post-SetEdit size in its own loop, so every emitted part must
// still be under the ceiling after that transformation is applied.
func TestEditPartsStayUnderBudgetAfterSetEdit(t *testing.T) {
	for _, tt := range splitCorpus {
		t.Run(tt.name, func(t *testing.T) {
			for i, p := range buildTextParts(event.MsgText, tt.subject, tt.text, splitCorpusPreviews, true) {
				p.Content.SetEdit(editProbeEventID)
				encoded, err := json.Marshal(p.Content)
				if err != nil {
					t.Fatalf("part %d: marshal: %v", i, err)
				}
				if len(encoded) > maxTextContentBytes {
					t.Errorf("part %d: post-SetEdit content = %dB, over the %dB ceiling",
						i, len(encoded), maxTextContentBytes)
				}
			}
		})
	}
}

// TestBuildTextPartsBoundsUnboundedOverhead covers the class the first round
// missed: bytes that splitting cannot reduce. A link preview's title and
// description come verbatim from a remote page (urlpreview.go caps only the
// 512 KB body read) and the subject comes off the wire, so either can exceed the
// whole event budget on its own. Shrinking the text chunk never helps, so
// without bounding them the loop bottoms out and emits an over-budget part.
func TestBuildTextPartsBoundsUnboundedOverhead(t *testing.T) {
	cases := []struct {
		name     string
		subject  string
		text     string
		previews []*event.BeeperLinkPreview
	}{
		{"huge preview description", "", "check this out https://example.com",
			[]*event.BeeperLinkPreview{{MatchedURL: "https://example.com", LinkPreview: event.LinkPreview{Description: repeatTo("d", 400000)}}}},
		{"huge preview title", "", "look https://example.com",
			[]*event.BeeperLinkPreview{{MatchedURL: "https://example.com", LinkPreview: event.LinkPreview{Title: repeatTo("t", 400000)}}}},
		{"many huge previews", "", "hi", []*event.BeeperLinkPreview{
			{MatchedURL: "https://a.example", LinkPreview: event.LinkPreview{Description: repeatTo("d", 100000)}},
			{MatchedURL: "https://b.example", LinkPreview: event.LinkPreview{Description: repeatTo("d", 100000)}},
			{MatchedURL: "https://c.example", LinkPreview: event.LinkPreview{Description: repeatTo("d", 100000)}},
		}},
		{"huge subject and huge preview", repeatTo("s", 60000), repeatTo("word ", 100000),
			[]*event.BeeperLinkPreview{{MatchedURL: "https://example.com", LinkPreview: event.LinkPreview{Description: repeatTo("d", 200000)}}}},
		{"nil entry among previews", "", "hi",
			[]*event.BeeperLinkPreview{nil, {MatchedURL: "https://example.com"}}},
	}
	for _, tc := range cases {
		for _, forEdit := range []bool{false, true} {
			name := tc.name
			if forEdit {
				name += " (edit)"
			}
			t.Run(name, func(t *testing.T) {
				parts := buildTextParts(event.MsgText, tc.subject, tc.text, tc.previews, forEdit)
				if len(parts) == 0 {
					t.Fatal("expected at least one part")
				}
				for i, p := range parts {
					if got := measuredSize(p.Content, forEdit); got > maxTextContentBytes {
						t.Errorf("part %d: measured size = %dB, over the %dB budget", i, got, maxTextContentBytes)
					}
				}
			})
		}
	}
}

// TestBoundPreviewsDoesNotMutateCaller guards the copy in boundPreviews: the
// previews belong to the caller and are reused (the CloudKit path builds them
// once per row), so truncating in place would corrupt them.
func TestBoundPreviewsDoesNotMutateCaller(t *testing.T) {
	original := &event.BeeperLinkPreview{
		MatchedURL: "https://example.com",
		LinkPreview: event.LinkPreview{
			Title:       repeatTo("t", 50000),
			Description: repeatTo("d", 50000),
		},
	}
	previews := []*event.BeeperLinkPreview{original}
	buildTextParts(event.MsgText, "", "hi", previews, false)
	if len(original.Title) != 50000 || len(original.Description) != 50000 {
		t.Errorf("caller's preview was mutated: title=%dB description=%dB",
			len(original.Title), len(original.Description))
	}
	bounded := boundPreviews(previews)
	if len(bounded[0].Title) > maxPreviewFieldBytes || len(bounded[0].Description) > maxPreviewFieldBytes {
		t.Errorf("preview fields not bounded: title=%dB description=%dB",
			len(bounded[0].Title), len(bounded[0].Description))
	}
	// A 3-byte rune is what actually exercises the backoff: maxPreviewFieldBytes
	// is 2048 and 2048%3 == 2, so a naive s[:max] lands mid-rune. A 2-byte rune
	// divides evenly and would pass even with the backoff deleted.
	for _, r := range []string{"€", "𝕏", "👨‍👩‍👧‍👦"} {
		for _, max := range []int{maxPreviewFieldBytes, maxSubjectBytes, 1000, 999, 7, 3, 1} {
			got := truncateBytes(repeatTo(r, 10000), max)
			if !utf8.ValidString(got) {
				t.Errorf("truncateBytes(%q, %d) split a rune", r, max)
			}
			if len(got) > max {
				t.Errorf("truncateBytes(%q, %d) returned %dB", r, max, len(got))
			}
		}
	}
}

// TestTextPartIDsSortInEmissionOrder pins the zero padding. GetAllPartsByID has
// no ORDER BY, and the edit path maps new parts onto stored parts positionally,
// so unpadded IDs would sort text1, text10, text11, text2 … and an edit would
// rewrite and redact the wrong events.
func TestTextPartIDsSortInEmissionOrder(t *testing.T) {
	// Escape-heavy text at the edit budget is the cheapest way to many parts.
	parts := buildTextParts(event.MsgText, "", repeatTo("\x01", 200000), nil, true)
	if len(parts) < 11 {
		t.Fatalf("need more than 10 parts to expose lexical ordering, got %d", len(parts))
	}
	ids := make([]string, len(parts))
	for i, p := range parts {
		ids[i] = string(p.ID)
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("emission order != lexical order at %d: emitted %q, sorted %q\nemitted: %v\nsorted:  %v",
				i, ids[i], sorted[i], ids, sorted)
		}
	}
	if ids[0] != "" {
		t.Errorf("first part ID = %q, want empty", ids[0])
	}
}

// TestBuildTextPartsShedsOverheadLosslessly is the regression for the one way
// this change could have caused the very data loss it exists to prevent. Bounding
// a single subject or preview field is not enough — the *number* of previews is
// unbounded, so overhead can still exceed the whole budget. When that happens the
// fit loop must shed formatting and then previews and keep every byte of text,
// never truncate: the caller advances by len(chunk), so a byte dropped inside the
// loop would never reach a later part.
//
// Twenty previews at the bounded field size is enough to push fixed overhead past
// the budget no matter how small the chunk gets, which is what drives the loop to
// its floor and forces the shed path.
func TestBuildTextPartsShedsOverheadLosslessly(t *testing.T) {
	previews := make([]*event.BeeperLinkPreview, 20)
	for i := range previews {
		previews[i] = &event.BeeperLinkPreview{
			MatchedURL: "https://example.com",
			LinkPreview: event.LinkPreview{
				Title:       repeatTo("t", maxPreviewFieldBytes*2),
				Description: repeatTo("d", maxPreviewFieldBytes*2),
			},
		}
	}
	for _, forEdit := range []bool{false, true} {
		for _, text := range []string{
			repeatTo("\x01", 60000),
			repeatTo("<", 60000),
			repeatTo("word ", 60000),
			repeatTo("é", 60000),
		} {
			subject := repeatTo("s", 60000)
			parts := buildTextParts(event.MsgText, subject, text, previews, forEdit)
			var rebuilt strings.Builder
			shed := false
			for i, p := range parts {
				body := p.Content.Body
				if i == 0 {
					prefix := "**" + truncateBytes(subject, maxSubjectBytes) + "**\n"
					if strings.HasPrefix(body, prefix) {
						body = body[len(prefix):]
					} else {
						body = ""
					}
					if len(p.Content.BeeperLinkPreviews) == 0 {
						shed = true
					}
				}
				rebuilt.WriteString(body)
				if got := measuredSize(p.Content, forEdit); got > maxTextContentBytes {
					t.Errorf("forEdit=%v: part %d = %dB, over the %dB budget", forEdit, i, got, maxTextContentBytes)
				}
			}
			if !shed {
				t.Errorf("forEdit=%v: expected the previews to be shed, but part 0 kept them", forEdit)
			}
			if rebuilt.String() != text {
				t.Errorf("forEdit=%v: lost text while shedding: rebuilt %dB, want %dB",
					forEdit, rebuilt.Len(), len(text))
			}
		}
	}
}

// TestTextPartIndexOrdersPastPaddingOverflow is the regression for the second
// round's ordering defect at its real boundary. Zero padding to text%03d makes
// lexical order correct only up to 999 parts: "text1000" sorts before "text999",
// so past that the edit path would again map new parts onto the wrong stored
// rows and redact the wrong events. Ordering is therefore numeric, not lexical,
// and padding width is not load-bearing.
func TestTextPartIndexOrdersPastPaddingOverflow(t *testing.T) {
	ids := []networkid.PartID{""}
	for i := 1; i <= 1500; i++ {
		ids = append(ids, networkid.PartID(fmt.Sprintf("text%03d", i)))
	}
	// Reverse first: a copy already in target order would pass even with a
	// comparator that never reorders anything.
	shuffled := append([]networkid.PartID(nil), ids...)
	for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	sort.Slice(shuffled, func(i, j int) bool {
		return textPartIndex(shuffled[i]) < textPartIndex(shuffled[j])
	})
	for i := range ids {
		if shuffled[i] != ids[i] {
			t.Fatalf("numeric sort wrong at %d: got %q, want %q", i, shuffled[i], ids[i])
		}
	}
	// Lexical sort must actually disagree here, otherwise this test proves nothing.
	lexical := append([]networkid.PartID(nil), ids...)
	sort.Slice(lexical, func(i, j int) bool { return lexical[i] < lexical[j] })
	same := true
	for i := range ids {
		if lexical[i] != ids[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("lexical order agrees with emission order, so this corpus does not exercise the overflow")
	}
	// Malformed and default IDs must not panic and must sort ahead of real ones.
	// Malformed IDs must sort LAST, not first: sorting them ahead of "" would
	// push every valid part down one position and make the edit path rewrite and
	// redact the wrong rows.
	for _, bad := range []networkid.PartID{"textXYZ", "text", "text 1", "text-1", "att0",
		"text99999999999999999999999"} {
		if textPartIndex(bad) != math.MaxInt {
			t.Errorf("textPartIndex(%q) = %d, want it to sort last", bad, textPartIndex(bad))
		}
	}
	if textPartIndex("") != 0 {
		t.Errorf("textPartIndex(\"\") = %d, want 0", textPartIndex(""))
	}
	mixed := []networkid.PartID{"textZZZ", "text002", "", "text001"}
	sort.Slice(mixed, func(i, j int) bool { return textPartIndex(mixed[i]) < textPartIndex(mixed[j]) })
	want := []networkid.PartID{"", "text001", "text002", "textZZZ"}
	for i := range want {
		if mixed[i] != want[i] {
			t.Fatalf("malformed ID reordered valid parts: got %v, want %v", mixed, want)
		}
	}
}

// TestBuildTextPartsKeepsFormattingOnFirstPart pins level-1 shedding. Nothing
// else in the suite asserts that part 0 *keeps* its HTML — every other check
// asserts absence on continuation parts — so a regression that made
// shedOverhead(content, 1) unconditional would silently strip format and
// formatted_body from every subject-bearing message and still pass.
func TestBuildTextPartsKeepsFormattingOnFirstPart(t *testing.T) {
	for _, tc := range []struct{ name, subject, text string }{
		{"short", "Dinner plans", "see you at 7"},
		{"split", "Dinner plans", repeatTo("word ", 200000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := buildTextParts(event.MsgText, tc.subject, tc.text, nil, false)
			if tc.name == "split" && len(parts) < 2 {
				t.Fatalf("expected this input to split, got %d part(s)", len(parts))
			}
			if parts[0].Content.Format != event.FormatHTML {
				t.Errorf("part 0 format = %q, want %q", parts[0].Content.Format, event.FormatHTML)
			}
			if !strings.Contains(parts[0].Content.FormattedBody, "<strong>Dinner plans</strong>") {
				t.Errorf("part 0 lost its formatted subject: %.80q", parts[0].Content.FormattedBody)
			}
		})
	}
}

// TestBuildTextContentEscapesHTML covers the behaviour change this work
// introduced: convertMessage and convertChatDBMessage previously interpolated
// raw subject and body into formatted_body, which is an HTML injection into
// every Matrix client that renders it.
func TestBuildTextContentEscapesHTML(t *testing.T) {
	parts := buildTextParts(event.MsgText, `<img src=x onerror="alert(1)">`,
		`plain & <b>bold</b>`, nil, false)
	fb := parts[0].Content.FormattedBody
	if strings.Contains(fb, "<img") || strings.Contains(fb, "<b>bold</b>") {
		t.Errorf("formatted_body carries unescaped markup: %q", fb)
	}
	for _, want := range []string{"&lt;img", "&amp;", "&lt;b&gt;bold"} {
		if !strings.Contains(fb, want) {
			t.Errorf("formatted_body missing %q: %q", want, fb)
		}
	}
	// The plain-text body keeps the original characters; only the HTML view is escaped.
	if !strings.Contains(parts[0].Content.Body, "plain & <b>bold</b>") {
		t.Errorf("plain body was altered: %q", parts[0].Content.Body)
	}
}

// TestVCardPreviewFitsBudget is the round-three regression: a per-field cap was
// not a bound, because the notice assembles up to seven fields into both body
// and formatted_body and escape-heavy input costs up to 16 wire bytes per input
// byte. Two 2 KB '&' fields were enough to exceed the PDU limit.
func TestVCardPreviewFitsBudget(t *testing.T) {
	huge := strings.Repeat("&", 8000)
	for _, tc := range []struct{ name, vcard string }{
		{"escape-heavy name and one phone", "BEGIN:VCARD\nFN:" + huge + "\nTEL:" + huge + "\nEND:VCARD"},
		{"all seven fields escape-heavy", "BEGIN:VCARD\nFN:" + huge +
			"\nTEL:" + huge + "\nTEL:" + huge + "\nTEL:" + huge +
			"\nEMAIL:" + huge + "\nEMAIL:" + huge + "\nEMAIL:" + huge + "\nEND:VCARD"},
		{"plain but very long", "BEGIN:VCARD\nFN:" + strings.Repeat("x", 200000) + "\nEND:VCARD"},
		{"angle brackets", "BEGIN:VCARD\nFN:" + strings.Repeat("<", 8000) + "\nEND:VCARD"},
		{"single escape-heavy name", "BEGIN:VCARD\nFN:" + strings.Repeat("&", 60000) + "\nEND:VCARD"},
		{"ordinary contact", "BEGIN:VCARD\nFN:Jane Doe\nTEL:+15551234567\nEMAIL:jane@example.com\nEND:VCARD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := makeVCardPreviewContent([]byte(tc.vcard))
			if content == nil {
				t.Fatal("expected a preview")
			}
			if got := measuredSize(content, false); got > maxTextContentBytes {
				t.Errorf("vCard notice = %dB, over the %dB budget", got, maxTextContentBytes)
			}
			if !strings.HasPrefix(content.Body, "Shared contact") {
				t.Errorf("lost the header: %.60q", content.Body)
			}
			// The drop loop must actually run on the multi-field case: seven
			// escape-heavy fields cannot all fit, so lines have to come off.
			if tc.name == "all seven fields escape-heavy" {
				if got := strings.Count(content.Body, "\n") + 1; got >= 8 {
					t.Errorf("expected lines to be dropped, still have %d", got)
				}
			}
			// A single field can never on its own force a drop to header-only:
			// truncateBytes caps one value at maxSubjectBytes, and even all-'&'
			// input costs ~16 wire bytes each, so one line tops out around 33 KB.
			if tc.name == "single escape-heavy name" && content.Body == "Shared contact" {
				t.Error("dropped the name unnecessarily; one bounded field always fits")
			}
		})
	}
}

// TestVCardBoundStaysBelowTheDropFloor makes an implicit coupling explicit.
// makeVCardPreviewContent's drop loop can shed detail lines but never the
// "Shared contact" header, so the bound only holds while a single
// maxSubjectBytes-capped field still fits on its own. Escape-heavy input costs
// up to 16 wire bytes per input byte ('&' -> "&amp;" via html.EscapeString ->
// "&amp;" via encoding/json), and the value appears in both body and
// formatted_body. Raising maxSubjectBytes without raising maxTextContentBytes
// would make the header-only exit reachable and silently reduce every
// escape-heavy shared contact to a bare header.
func TestVCardBoundStaysBelowTheDropFloor(t *testing.T) {
	const worstWireBytesPerInputByte = 16
	if maxSubjectBytes*worstWireBytesPerInputByte >= maxTextContentBytes {
		t.Fatalf("maxSubjectBytes=%d at %dx = %dB, which no longer fits the %dB budget: "+
			"a single vCard field could be dropped entirely",
			maxSubjectBytes, worstWireBytesPerInputByte,
			maxSubjectBytes*worstWireBytesPerInputByte, maxTextContentBytes)
	}
	// And prove it empirically for the worst escape classes, not just by arithmetic.
	for _, ch := range []string{"&", "<", ">", `"`, "'", "\x01"} {
		content := makeVCardPreviewContent([]byte(
			"BEGIN:VCARD\nFN:" + strings.Repeat(ch, maxSubjectBytes*4) + "\nEND:VCARD"))
		if content == nil {
			t.Fatalf("%q: expected a preview", ch)
		}
		if content.Body == "Shared contact" {
			t.Errorf("%q: name was dropped; one bounded field should always fit", ch)
		}
		if got := measuredSize(content, false); got > maxTextContentBytes {
			t.Errorf("%q: %dB, over the %dB budget", ch, got, maxTextContentBytes)
		}
	}
}

// TestVCardPreviewFitsRealEncryptedPDU gives the vCard bound an oracle that is
// independent of the implementation: TestVCardPreviewFitsBudget checks against
// measuredSize, the same function makeVCardPreviewContent uses to decide it
// fits, so it cannot detect the budget itself being wrong. This encrypts with a
// real megolm session instead.
//
// Measured sensitivity: the worst vCard sits around 45 KB of real PDU against
// the 65,536 limit, so this catches maxTextContentBytes being raised past ~60,000
// (89,141B at 80,000) or a structural change to how the notice is assembled. It
// does not catch a small raise — that headroom is real, not an untested gap.
func TestVCardPreviewFitsRealEncryptedPDU(t *testing.T) {
	session.Register()
	sess, err := olm.NewOutboundGroupSession()
	if err != nil {
		t.Fatalf("create megolm session: %v", err)
	}
	huge := strings.Repeat("&", 8000)
	for _, vcard := range []string{
		"BEGIN:VCARD\nFN:" + huge + "\nTEL:" + huge + "\nTEL:" + huge + "\nTEL:" + huge +
			"\nEMAIL:" + huge + "\nEMAIL:" + huge + "\nEMAIL:" + huge + "\nEND:VCARD",
		"BEGIN:VCARD\nFN:" + strings.Repeat("<", 20000) + "\nTEL:" + strings.Repeat("<", 20000) + "\nEND:VCARD",
		"BEGIN:VCARD\nFN:Jane Doe\nTEL:+15551234567\nEMAIL:jane@example.com\nEND:VCARD",
	} {
		content := makeVCardPreviewContent([]byte(vcard))
		if content == nil {
			t.Fatal("expected a preview")
		}
		if got := encryptedPDUSize(t, sess, content); got > matrixPDULimit {
			t.Errorf("vCard notice encrypts to %dB, over the %dB Matrix limit", got, matrixPDULimit)
		}
	}
}
