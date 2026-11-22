package connector

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

func TestWriteCAFOpus_CoreAudioChunksAndRemainder(t *testing.T) {
	// 50 packets * 960 frames = 48000 total frames.
	// PreSkip = 312, GranulePos = 47500 -> valid = 47188, remainder = 500.
	// CoreAudio invariant: priming + valid + remainder == totalFrames.
	info := &oggOpusInfo{
		Channels:   1,
		PreSkip:    312,
		OpusHead:   buildOpusHead(1, 312),
		GranulePos: 47500,
	}
	for i := 0; i < 50; i++ {
		pkt := make([]byte, 32)
		pkt[0] = 0x98 // CELT 20ms, code 0 -> 960 frames
		info.Packets = append(info.Packets, pkt)
	}

	cafData, err := writeCAFOpus(info)
	if err != nil {
		t.Fatalf("writeCAFOpus error: %v", err)
	}
	if string(cafData[:4]) != "caff" {
		t.Fatalf("missing caff header")
	}

	// Walk chunks and verify CoreAudio structure.
	pos := 8
	chunks := map[string][]byte{}
	for pos+12 <= len(cafData) {
		ct := string(cafData[pos : pos+4])
		sz := int(int64(binary.BigEndian.Uint64(cafData[pos+4 : pos+12])))
		pos += 12
		chunks[ct] = cafData[pos : pos+sz]
		pos += sz
	}

	if _, hasMagc := chunks["magc"]; hasMagc {
		t.Error("CAF output must not contain non-standard 'magc' chunk")
	}
	chanData, hasChan := chunks["chan"]
	if !hasChan || len(chanData) != 12 {
		t.Fatalf("missing or malformed 'chan' chunk: len=%d", len(chanData))
	}
	if tag := binary.BigEndian.Uint32(chanData[0:4]); tag != cafChannelLayoutTagMono {
		t.Errorf("chan layout tag = 0x%x, want 0x%x", tag, cafChannelLayoutTagMono)
	}

	paktData, hasPakt := chunks["pakt"]
	if !hasPakt || len(paktData) < 24 {
		t.Fatalf("missing or malformed 'pakt' chunk")
	}
	numPkts := int64(binary.BigEndian.Uint64(paktData[0:8]))
	validFrames := int64(binary.BigEndian.Uint64(paktData[8:16]))
	primingFrames := int32(binary.BigEndian.Uint32(paktData[16:20]))
	remainderFrames := int32(binary.BigEndian.Uint32(paktData[20:24]))
	if numPkts != 50 {
		t.Errorf("numPkts = %d, want 50", numPkts)
	}
	if validFrames != 47188 || primingFrames != 312 || remainderFrames != 500 {
		t.Errorf("pakt frames = (valid=%d, priming=%d, remainder=%d), want (47188, 312, 500)",
			validFrames, primingFrames, remainderFrames)
	}
	if int64(primingFrames)+validFrames+int64(remainderFrames) != 48000 {
		t.Errorf("CoreAudio frame invariant violated: %d + %d + %d != 48000",
			primingFrames, validFrames, remainderFrames)
	}
}

func TestWriteCAFOpus_VariableFramesPerPacket(t *testing.T) {
	info := &oggOpusInfo{
		Channels:   1,
		PreSkip:    0,
		OpusHead:   buildOpusHead(1, 0),
		GranulePos: 960 + 1920,
		Packets: [][]byte{
			{0x08, 1, 2, 3}, // SILK 20ms -> 960 frames
			{0x10, 4, 5, 6}, // SILK 40ms -> 1920 frames
		},
	}
	cafData, err := writeCAFOpus(info)
	if err != nil {
		t.Fatalf("writeCAFOpus error: %v", err)
	}
	// desc starts at offset 8 + 12 = 20; framesPerPacket is at desc[20:24]
	framesPerPacket := binary.BigEndian.Uint32(cafData[20+20 : 20+24])
	if framesPerPacket != 0 {
		t.Errorf("framesPerPacket = %d, want 0 for variable frame durations", framesPerPacket)
	}
	parsed, err := parseCAFOpus(cafData)
	if err != nil {
		t.Fatalf("parseCAFOpus roundtrip error: %v", err)
	}
	if len(parsed.Packets) != 2 {
		t.Fatalf("roundtrip packets = %d, want 2", len(parsed.Packets))
	}
}

func TestConvertVoiceForIMessage_SniffsOGGWithOctetStream(t *testing.T) {
	oggData := buildMinimalOGGOpus()
	cafData, mime, name, isVoice := convertVoiceForIMessage(context.Background(), oggData, "application/octet-stream", "voice", true)
	if !isVoice {
		t.Fatal("expected isVoice = true")
	}
	if mime != "audio/x-caf" {
		t.Errorf("mime = %q, want audio/x-caf", mime)
	}
	if name != "Audio Message.caf" {
		t.Errorf("name = %q, want Audio Message.caf", name)
	}
	if !bytes.HasPrefix(cafData, []byte("caff")) {
		t.Error("expected caff output")
	}
}

func TestUTF16Len(t *testing.T) {
	tests := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"hello", 5},
		{"cześć", 5},  // 2-byte UTF-8 Polish chars are 1 UTF-16 code unit each
		{"hello 😀", 8}, // emoji is a UTF-16 surrogate pair (2 units)
	}
	for _, tt := range tests {
		if got := utf16Len(tt.s); got != tt.want {
			t.Errorf("utf16Len(%q) = %d, want %d", tt.s, got, tt.want)
		}
	}
}

func TestExtractReplyInfo_NonZeroLengthAndUppercase(t *testing.T) {
	msg := &database.Message{
		ID: networkid.MessageID("abcdef01-2345-6789-abcd-ef0123456789"),
		Metadata: &MessageMetadata{
			Text:     "cześć 😀",
			UTF16Len: utf16Len("cześć 😀"),
		},
	}
	guid, part := extractReplyInfo(msg)
	if guid == nil || *guid != "ABCDEF01-2345-6789-ABCD-EF0123456789" {
		t.Fatalf("guid = %v, want uppercase UUID", guid)
	}
	if part == nil || *part != "0:0:8" {
		t.Fatalf("part = %v, want 0:0:8", part)
	}

	// Message with no metadata must still never emit 0:0:0.
	bare := &database.Message{ID: networkid.MessageID("11111111-2222-3333-4444-555555555555")}
	_, barePart := extractReplyInfo(bare)
	if barePart == nil || *barePart != "0:0:1" {
		t.Fatalf("bare part = %v, want 0:0:1", barePart)
	}
}

func TestEmojiToTapbackType_Variants(t *testing.T) {
	if typ, em := emojiToTapbackType("❤"); typ != 0 || em != nil {
		t.Errorf("plain heart = (%d, %v), want (0, nil)", typ, em)
	}
	if typ, em := emojiToTapbackType("❤️"); typ != 0 || em != nil {
		t.Errorf("FE0F heart = (%d, %v), want (0, nil)", typ, em)
	}
	if typ, em := emojiToTapbackType("🔥"); typ != 6 || em == nil || *em != "🔥" {
		t.Errorf("custom emoji = (%d, %v), want (6, 🔥)", typ, em)
	}
}
