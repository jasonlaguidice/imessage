// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

// Bidirectional OGG Opus ↔ CAF Opus remuxer for iMessage voice messages.
//
// iMessage uses Opus audio in Apple's CAF (Core Audio Format) container.
// Beeper and most Matrix clients send voice recordings as OGG Opus.
// Since both use the same Opus codec, we just extract the compressed packets
// from one container and rewrap them in the other — no transcoding needed.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ffmpeg"
)

// looksLikeOGGOpus reports whether data starts with an OGG page header ("OggS")
// carrying an OpusHead identification packet.
func looksLikeOGGOpus(data []byte) bool {
	if len(data) < 36 || string(data[:4]) != "OggS" {
		return false
	}
	return bytes.Contains(data[:min(len(data), 128)], []byte("OpusHead"))
}

// looksLikeCAF reports whether data starts with a Core Audio Format ("caff") header.
func looksLikeCAF(data []byte) bool {
	return len(data) >= 8 && string(data[:4]) == "caff"
}

// convertAudioForIMessage remuxes OGG Opus audio to CAF Opus for native
// iMessage voice message playback. Non-OGG formats are returned unchanged
// since iOS can play most other audio formats directly.
func convertAudioForIMessage(data []byte, mimeType, fileName string) ([]byte, string, string) {
	if mimeType != "audio/ogg" && !strings.HasPrefix(mimeType, "audio/ogg;") && !looksLikeOGGOpus(data) {
		return data, mimeType, fileName
	}

	info, err := parseOGGOpus(data)
	if err != nil {
		return data, mimeType, fileName
	}

	cafData, err := writeCAFOpus(info)
	if err != nil {
		return data, mimeType, fileName
	}

	newName := strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ".caf"
	return cafData, "audio/x-caf", newName
}

// convertVoiceForIMessage converts Matrix voice messages (any format, including
// OGG Opus, M4A/MP4, WebM Opus, AAC, MP3, WAV, or magic-byte-sniffed OGG Opus)
// into Apple CoreAudio CAF Opus ("Audio Message.caf", "audio/x-caf") so
// iOS/macOS Messages.app renders and plays them as native voice messages.
// Returns (data, mimeType, fileName, isVoice).
func convertVoiceForIMessage(ctx context.Context, data []byte, mimeType, fileName string, isVoiceEvent bool) ([]byte, string, string, bool) {
	log := zerolog.Ctx(ctx)

	// Already a CAF file.
	if looksLikeCAF(data) || mimeType == "audio/x-caf" || mimeType == "audio/caf" {
		if looksLikeCAF(data) {
			return data, "audio/x-caf", "Audio Message.caf", true
		}
	}

	// Fast path: lossless OGG Opus -> CAF Opus remux (no re-encoding).
	if mimeType == "audio/ogg" || strings.HasPrefix(mimeType, "audio/ogg;") || looksLikeOGGOpus(data) {
		info, err := parseOGGOpus(data)
		if err == nil {
			cafData, err := writeCAFOpus(info)
			if err == nil {
				return cafData, "audio/x-caf", "Audio Message.caf", true
			}
			log.Warn().Err(err).Msg("Failed to write CAF Opus from OGG Opus")
		} else {
			log.Warn().Err(err).Str("mime", mimeType).Msg("Failed to parse OGG Opus for voice conversion")
		}
	}

	// Transcode path: Matrix voice message in a non-OGG container (e.g. audio/mp4,
	// audio/webm, audio/aac, audio/mpeg from Beeper Desktop/Android/iOS).
	// Encode to clean 20ms mono 48kHz OGG Opus via ffmpeg, then remux to CAF Opus.
	if isVoiceEvent && ffmpeg.Supported() {
		oggData, err := ffmpeg.ConvertBytes(
			ctx,
			data,
			".ogg",
			nil,
			[]string{
				"-vn",
				"-c:a", "libopus",
				"-b:a", "24k",
				"-ar", "48000",
				"-ac", "1",
				"-frame_duration", "20",
				"-application", "voip",
			},
			mimeType,
		)
		if err != nil {
			log.Warn().Err(err).Str("mime", mimeType).Str("file", fileName).
				Msg("FFmpeg voice transcode to OGG Opus failed; sending original audio")
			return data, mimeType, fileName, false
		}
		info, err := parseOGGOpus(oggData)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to parse ffmpeg-transcoded OGG Opus")
			return data, mimeType, fileName, false
		}
		cafData, err := writeCAFOpus(info)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to wrap transcoded Opus in CAF")
			return data, mimeType, fileName, false
		}
		log.Info().
			Str("orig_mime", mimeType).
			Int("orig_bytes", len(data)).
			Int("caf_bytes", len(cafData)).
			Int("packets", len(info.Packets)).
			Msg("Transcoded voice message to Apple CAF Opus")
		return cafData, "audio/x-caf", "Audio Message.caf", true
	}

	return data, mimeType, fileName, false
}

// convertAudioForMatrix remuxes CAF Opus audio to OGG Opus for Matrix clients.
// Returns the converted data, new MIME type, new filename, and duration in milliseconds.
// Non-CAF or non-Opus formats are returned unchanged with duration 0.
func convertAudioForMatrix(data []byte, mimeType, fileName string) ([]byte, string, string, int) {
	if mimeType != "audio/x-caf" && !strings.HasSuffix(strings.ToLower(fileName), ".caf") {
		return data, mimeType, fileName, 0
	}

	info, err := parseCAFOpus(data)
	if err != nil {
		return data, mimeType, fileName, 0
	}

	oggData, err := writeOGGOpus(info)
	if err != nil {
		return data, mimeType, fileName, 0
	}

	durationMs := int(info.GranulePos * 1000 / 48000)
	newName := strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ".ogg"
	return oggData, "audio/ogg", newName, durationMs
}

// ============================================================================
// CAF Opus parser
// ============================================================================

func parseCAFOpus(data []byte) (*oggOpusInfo, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("CAF too short")
	}
	if string(data[:4]) != "caff" {
		return nil, fmt.Errorf("not a CAF file")
	}

	r := bytes.NewReader(data[8:]) // skip file header

	var opusHead []byte
	var channels int
	var preSkip int
	var bytesPerPacket int
	var framesPerPacket int
	var validFrames int64
	var primingFrames int32
	var numPackets int64
	var vlqData []byte
	var audioData []byte

	// Read chunks
	for {
		var chunkType [4]byte
		if _, err := io.ReadFull(r, chunkType[:]); err != nil {
			break
		}
		var chunkSize int64
		if err := binary.Read(r, binary.BigEndian, &chunkSize); err != nil {
			break
		}

		switch string(chunkType[:]) {
		case "desc":
			if chunkSize < 32 {
				return nil, fmt.Errorf("CAF desc chunk too small")
			}
			var desc [32]byte
			if _, err := io.ReadFull(r, desc[:]); err != nil {
				return nil, err
			}
			formatID := string(desc[8:12])
			if formatID != "opus" {
				return nil, fmt.Errorf("CAF format is %q, not opus", formatID)
			}
			bytesPerPacket = int(binary.BigEndian.Uint32(desc[16:20]))
			framesPerPacket = int(binary.BigEndian.Uint32(desc[20:24]))
			channels = int(binary.BigEndian.Uint32(desc[24:28]))
			if chunkSize > 32 {
				io.CopyN(io.Discard, r, chunkSize-32)
			}

		case "kuki", "magc":
			cookie := make([]byte, chunkSize)
			if _, err := io.ReadFull(r, cookie); err != nil {
				return nil, err
			}
			if len(cookie) >= 12 && string(cookie[:8]) == "OpusHead" {
				opusHead = cookie
				preSkip = int(binary.LittleEndian.Uint16(cookie[10:12]))
				if cookie[9] > 0 {
					channels = int(cookie[9])
				}
			}

		case "pakt":
			if chunkSize < 24 {
				return nil, fmt.Errorf("CAF pakt chunk too small")
			}
			var paktHdr [24]byte
			if _, err := io.ReadFull(r, paktHdr[:]); err != nil {
				return nil, err
			}
			numPackets = int64(binary.BigEndian.Uint64(paktHdr[0:8]))
			validFrames = int64(binary.BigEndian.Uint64(paktHdr[8:16]))
			primingFrames = int32(binary.BigEndian.Uint32(paktHdr[16:20]))

			remaining := chunkSize - 24
			vlqData = make([]byte, remaining)
			if _, err := io.ReadFull(r, vlqData); err != nil {
				return nil, err
			}

		case "data":
			if chunkSize == -1 {
				// -1 means data extends to end of file
				audioData, _ = io.ReadAll(r)
			} else {
				audioData = make([]byte, chunkSize)
				if _, err := io.ReadFull(r, audioData); err != nil {
					return nil, err
				}
			}
			// Skip 4-byte edit count at the start of audio data
			if len(audioData) >= 4 {
				audioData = audioData[4:]
			}

		default:
			if chunkSize > 0 {
				io.CopyN(io.Discard, r, chunkSize)
			}
		}
	}

	if channels == 0 {
		channels = 1
	}

	// Build OpusHead if not found in magic cookie
	if opusHead == nil {
		opusHead = buildOpusHead(channels, preSkip)
	}

	// Decode packet sizes from the pakt chunk (deferred so desc fields are available).
	// CAF packet table entries contain:
	//   - byte size (VLQ) if mBytesPerPacket == 0
	//   - frame count (VLQ) if mFramesPerPacket == 0
	var packetSizes []int
	if bytesPerPacket > 0 {
		// CBR: all packets are the same size, packet table has no byte sizes
		packetSizes = make([]int, numPackets)
		for i := range packetSizes {
			packetSizes[i] = bytesPerPacket
		}
	} else if vlqData != nil {
		hasFrameSize := framesPerPacket == 0
		packetSizes = decodeCAFPacketSizes(vlqData, int(numPackets), hasFrameSize)
	} else {
		return nil, fmt.Errorf("no packet table in CAF")
	}

	if framesPerPacket == 0 {
		framesPerPacket = 960 // 20ms default
	}

	// Split audio data into packets
	var packets [][]byte
	offset := 0
	for _, size := range packetSizes {
		if offset+size > len(audioData) {
			break
		}
		packets = append(packets, audioData[offset:offset+size])
		offset += size
	}

	// Calculate granule position
	granulePos := validFrames + int64(primingFrames)
	if granulePos <= 0 {
		granulePos = int64(len(packets)) * int64(framesPerPacket)
	}

	return &oggOpusInfo{
		Channels:   channels,
		PreSkip:    preSkip,
		OpusHead:   opusHead,
		Packets:    packets,
		GranulePos: granulePos,
	}, nil
}

// decodeCAFPacketSizes decodes n packet byte-sizes from VLQ-encoded data.
// When hasFrameSize is true, each entry has two VLQs (byte size + frame count)
// and the frame count is skipped.
func decodeCAFPacketSizes(data []byte, n int, hasFrameSize bool) []int {
	sizes := make([]int, 0, n)
	pos := 0
	for i := 0; i < n && pos < len(data); i++ {
		// Read byte size VLQ
		val := 0
		for pos < len(data) {
			b := data[pos]
			pos++
			val = (val << 7) | int(b&0x7F)
			if b&0x80 == 0 {
				break
			}
		}
		sizes = append(sizes, val)
		// Skip frame count VLQ if present
		if hasFrameSize {
			for pos < len(data) {
				b := data[pos]
				pos++
				if b&0x80 == 0 {
					break
				}
			}
		}
	}
	return sizes
}

// buildOpusHead creates a minimal OpusHead packet.
func buildOpusHead(channels, preSkip int) []byte {
	head := make([]byte, 19)
	copy(head[0:8], "OpusHead")
	head[8] = 1 // version
	head[9] = byte(channels)
	binary.LittleEndian.PutUint16(head[10:12], uint16(preSkip))
	binary.LittleEndian.PutUint32(head[12:16], 48000) // input sample rate
	// bytes 16-17: output gain = 0
	// byte 18: channel mapping = 0
	return head
}

// ============================================================================
// OGG Opus writer
// ============================================================================

// oggCRCTable is the CRC-32 lookup table for OGG (polynomial 0x04C11DB7, direct).
var oggCRCTable = func() *[256]uint32 {
	var t [256]uint32
	for i := 0; i < 256; i++ {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = (r << 1) ^ 0x04C11DB7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return &t
}()

func oggCRC(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = (crc << 8) ^ oggCRCTable[byte(crc>>24)^b]
	}
	return crc
}

func writeOGGOpus(info *oggOpusInfo) ([]byte, error) {
	var buf bytes.Buffer
	serial := uint32(0x4F707573) // "Opus"
	seq := uint32(0)

	// Page 1: OpusHead (BOS)
	writeOGGPage(&buf, serial, seq, 0, 0x02, [][]byte{info.OpusHead})
	seq++

	// Page 2: OpusTags
	tags := buildOpusTags()
	writeOGGPage(&buf, serial, seq, 0, 0x00, [][]byte{tags})
	seq++

	// Audio pages: pack multiple packets per page (max ~48KB, max 255 segments)
	const maxPagePayload = 48000
	const maxSegments = 255
	var pagePackets [][]byte
	var pageSize int
	var pageSegs int
	var granule int64
	framesPerPacket := opusPacketFrames(info.Packets[0])

	for i, pkt := range info.Packets {
		pktSegs := len(pkt)/255 + 1
		if (pageSize+len(pkt) > maxPagePayload || pageSegs+pktSegs > maxSegments) && len(pagePackets) > 0 {
			writeOGGPage(&buf, serial, seq, granule, 0x00, pagePackets)
			seq++
			pagePackets = nil
			pageSize = 0
			pageSegs = 0
		}
		pagePackets = append(pagePackets, pkt)
		pageSize += len(pkt)
		pageSegs += pktSegs
		granule = int64(info.PreSkip) + int64(i+1)*int64(framesPerPacket)
		if granule > info.GranulePos {
			granule = info.GranulePos
		}
	}
	if len(pagePackets) > 0 {
		writeOGGPage(&buf, serial, seq, info.GranulePos, 0x04, pagePackets) // EOS
	}

	return buf.Bytes(), nil
}

// writeOGGPage writes a single OGG page containing the given packets.
func writeOGGPage(buf *bytes.Buffer, serial, seq uint32, granule int64, flags byte, packets [][]byte) {
	// Build segment table
	var segTable []byte
	for _, pkt := range packets {
		remaining := len(pkt)
		for remaining >= 255 {
			segTable = append(segTable, 255)
			remaining -= 255
		}
		segTable = append(segTable, byte(remaining))
	}

	// Build page header (without CRC)
	var hdr bytes.Buffer
	hdr.WriteString("OggS")
	hdr.WriteByte(0)     // version
	hdr.WriteByte(flags) // header type
	binary.Write(&hdr, binary.LittleEndian, granule)
	binary.Write(&hdr, binary.LittleEndian, serial)
	binary.Write(&hdr, binary.LittleEndian, seq)
	binary.Write(&hdr, binary.LittleEndian, uint32(0)) // CRC placeholder
	hdr.WriteByte(byte(len(segTable)))
	hdr.Write(segTable)

	// Compute CRC over header + payload
	hdrBytes := hdr.Bytes()
	// Set CRC field to 0 for computation (already 0)
	var payload bytes.Buffer
	for _, pkt := range packets {
		payload.Write(pkt)
	}

	crcData := append(hdrBytes, payload.Bytes()...)
	checksum := oggCRC(crcData)
	binary.LittleEndian.PutUint32(hdrBytes[22:26], checksum)

	buf.Write(hdrBytes)
	buf.Write(payload.Bytes())
}

// buildOpusTags creates a minimal OpusTags packet.
func buildOpusTags() []byte {
	var tags bytes.Buffer
	tags.WriteString("OpusTags")
	vendor := "mautrix-imessage"
	binary.Write(&tags, binary.LittleEndian, uint32(len(vendor)))
	tags.WriteString(vendor)
	binary.Write(&tags, binary.LittleEndian, uint32(0)) // no comments
	return tags.Bytes()
}

// ============================================================================
// OGG Opus parser
// ============================================================================

type oggOpusInfo struct {
	Channels   int
	PreSkip    int
	OpusHead   []byte   // raw OpusHead packet (used as CAF magic cookie)
	Packets    [][]byte // Opus audio packets (excluding header packets)
	GranulePos int64    // last page granule = total PCM frames at 48kHz
}

func parseOGGOpus(data []byte) (*oggOpusInfo, error) {
	packets, granule, err := readOGGPackets(data)
	if err != nil {
		return nil, err
	}
	if len(packets) < 3 {
		return nil, fmt.Errorf("OGG stream too short: %d packets", len(packets))
	}

	head := packets[0]
	if len(head) < 19 || string(head[:8]) != "OpusHead" {
		return nil, fmt.Errorf("not an OGG Opus stream")
	}

	return &oggOpusInfo{
		Channels:   int(head[9]),
		PreSkip:    int(binary.LittleEndian.Uint16(head[10:12])),
		OpusHead:   head,
		Packets:    packets[2:], // skip OpusHead + OpusTags
		GranulePos: granule,
	}, nil
}

// readOGGPackets reads all OGG pages and assembles complete packets.
// Returns the packets and the granule position from the last page.
func readOGGPackets(data []byte) ([][]byte, int64, error) {
	r := bytes.NewReader(data)
	var packets [][]byte
	var current []byte
	var lastGranule int64

	for {
		// Sync: read "OggS" magic
		var magic [4]byte
		if _, err := io.ReadFull(r, magic[:]); err != nil {
			break
		}
		if string(magic[:]) != "OggS" {
			return nil, 0, fmt.Errorf("invalid OGG page sync")
		}

		// Page header: version(1) + type(1) + granule(8) + serial(4) + seq(4) + crc(4) = 22 bytes
		var hdr [22]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, 0, fmt.Errorf("truncated OGG page header: %w", err)
		}
		granule := int64(binary.LittleEndian.Uint64(hdr[2:10]))
		if granule > 0 {
			lastGranule = granule
		}

		// Segment count + table
		var nSeg [1]byte
		if _, err := io.ReadFull(r, nSeg[:]); err != nil {
			return nil, 0, err
		}
		segTable := make([]byte, nSeg[0])
		if _, err := io.ReadFull(r, segTable); err != nil {
			return nil, 0, err
		}

		// Read segments, assemble packets (segment < 255 = packet boundary)
		for _, segSize := range segTable {
			seg := make([]byte, segSize)
			if _, err := io.ReadFull(r, seg); err != nil {
				return nil, 0, err
			}
			current = append(current, seg...)
			if segSize < 255 {
				packets = append(packets, current)
				current = nil
			}
		}
	}

	if current != nil {
		packets = append(packets, current)
	}
	return packets, lastGranule, nil
}

// ============================================================================
// CAF Opus writer
// ============================================================================

const (
	cafChannelLayoutTagMono   uint32 = (100 << 16) | 1 // kCAFChannelLayoutTag_Mono
	cafChannelLayoutTagStereo uint32 = (101 << 16) | 2 // kCAFChannelLayoutTag_Stereo
)

func writeCAFOpus(info *oggOpusInfo) ([]byte, error) {
	if len(info.Packets) == 0 {
		return nil, fmt.Errorf("no audio packets")
	}

	channels := info.Channels
	if channels <= 0 {
		channels = 1
	}

	// Determine whether all Opus packets share the same frame count (constant
	// frames-per-packet, e.g. 960 = 20ms at 48kHz) or vary across packets.
	// In CAF, mFramesPerPacket = 0 means variable frames per packet and requires
	// each pakt entry to encode (byteSize, frameCount) VLQ pairs.
	packetFrames := make([]int, len(info.Packets))
	firstFrames := opusPacketFrames(info.Packets[0])
	constantFrames := true
	var totalFrames int64
	for i, pkt := range info.Packets {
		f := opusPacketFrames(pkt)
		packetFrames[i] = f
		totalFrames += int64(f)
		if f != firstFrames {
			constantFrames = false
		}
	}
	descFramesPerPacket := uint32(0)
	if constantFrames {
		descFramesPerPacket = uint32(firstFrames)
	}

	// CoreAudio CAF requires: mPrimingFrames + mNumberValidFrames + mRemainderFrames == totalFrames.
	primingFrames := int32(info.PreSkip)
	if primingFrames < 0 || int64(primingFrames) >= totalFrames {
		primingFrames = 0
	}
	var validFrames int64
	var remainderFrames int32
	if info.GranulePos > int64(primingFrames) && info.GranulePos <= totalFrames {
		validFrames = info.GranulePos - int64(primingFrames)
		remainderFrames = int32(totalFrames - info.GranulePos)
	} else {
		validFrames = totalFrames - int64(primingFrames)
		remainderFrames = 0
	}

	var buf bytes.Buffer

	// -- File header --
	buf.WriteString("caff")
	binary.Write(&buf, binary.BigEndian, uint16(1)) // version
	binary.Write(&buf, binary.BigEndian, uint16(0)) // flags

	// -- Audio Description chunk ('desc') --
	var desc bytes.Buffer
	binary.Write(&desc, binary.BigEndian, math.Float64bits(48000.0)) // sample rate
	desc.WriteString("opus")                                          // format ID
	binary.Write(&desc, binary.BigEndian, uint32(0))                  // format flags
	binary.Write(&desc, binary.BigEndian, uint32(0))                  // bytes per packet (VBR=0)
	binary.Write(&desc, binary.BigEndian, descFramesPerPacket)        // frames per packet (0 if variable)
	binary.Write(&desc, binary.BigEndian, uint32(channels))           // channels per frame
	binary.Write(&desc, binary.BigEndian, uint32(0))                  // bits per channel (compressed=0)
	cafWriteChunk(&buf, "desc", desc.Bytes())

	// -- Channel Layout chunk ('chan') --
	// Apple CoreAudio CAF Opus files declare explicit channel layout (Mono/Stereo)
	// and do NOT use a non-standard 'magc' chunk.
	var layoutTag uint32
	switch channels {
	case 1:
		layoutTag = cafChannelLayoutTagMono
	case 2:
		layoutTag = cafChannelLayoutTagStereo
	}
	if layoutTag != 0 {
		var chanChunk bytes.Buffer
		binary.Write(&chanChunk, binary.BigEndian, layoutTag)
		binary.Write(&chanChunk, binary.BigEndian, uint32(0)) // mChannelBitmap
		binary.Write(&chanChunk, binary.BigEndian, uint32(0)) // mNumberChannelDescriptions
		cafWriteChunk(&buf, "chan", chanChunk.Bytes())
	}

	// -- Packet Table chunk ('pakt') --
	var pakt bytes.Buffer
	binary.Write(&pakt, binary.BigEndian, int64(len(info.Packets))) // number of packets
	binary.Write(&pakt, binary.BigEndian, validFrames)               // valid frames
	binary.Write(&pakt, binary.BigEndian, primingFrames)             // priming frames
	binary.Write(&pakt, binary.BigEndian, remainderFrames)           // remainder frames
	for i, pkt := range info.Packets {
		pakt.Write(cafVLQ(int64(len(pkt))))
		if !constantFrames {
			pakt.Write(cafVLQ(int64(packetFrames[i])))
		}
	}
	cafWriteChunk(&buf, "pakt", pakt.Bytes())

	// -- Audio Data chunk ('data') --
	var audioData bytes.Buffer
	binary.Write(&audioData, binary.BigEndian, uint32(0)) // edit count
	for _, pkt := range info.Packets {
		audioData.Write(pkt)
	}
	cafWriteChunk(&buf, "data", audioData.Bytes())

	return buf.Bytes(), nil
}

// cafWriteChunk writes a CAF chunk: type (4 bytes) + size (int64) + data.
func cafWriteChunk(w *bytes.Buffer, chunkType string, data []byte) {
	w.WriteString(chunkType)
	binary.Write(w, binary.BigEndian, int64(len(data)))
	w.Write(data)
}

// cafVLQ encodes an integer as a CAF variable-length quantity (MIDI-style VLQ).
func cafVLQ(value int64) []byte {
	if value <= 0 {
		return []byte{0}
	}
	var tmp [10]byte
	i := len(tmp) - 1
	tmp[i] = byte(value & 0x7F)
	value >>= 7
	for value > 0 {
		i--
		tmp[i] = byte(value&0x7F) | 0x80
		value >>= 7
	}
	return tmp[i:]
}

// ============================================================================
// Opus TOC parser
// ============================================================================

// opusPacketFrames returns the number of PCM frames (at 48kHz) in an Opus packet
// by parsing the TOC byte and frame count code per RFC 6716.
func opusPacketFrames(packet []byte) int {
	if len(packet) == 0 {
		return 960 // default 20ms at 48kHz
	}

	toc := packet[0]
	config := int(toc >> 3)

	// Frame duration in 48kHz samples based on TOC config
	var samplesPerFrame int
	switch {
	case config < 12:
		// SILK-only: configs 0-11 cycle through {10,20,40,60}ms
		samplesPerFrame = [4]int{480, 960, 1920, 2880}[config%4]
	case config < 16:
		// Hybrid: configs 12-15 cycle through {10,20}ms
		samplesPerFrame = [2]int{480, 960}[config%2]
	default:
		// CELT-only: configs 16-31 cycle through {2.5,5,10,20}ms
		samplesPerFrame = [4]int{120, 240, 480, 960}[(config-16)%4]
	}

	// Frame count from code field (bits 0-1 of TOC)
	switch toc & 0x3 {
	case 0:
		return samplesPerFrame
	case 1, 2:
		return samplesPerFrame * 2
	case 3:
		if len(packet) >= 2 {
			n := int(packet[1] & 0x3F)
			if n > 0 {
				return samplesPerFrame * n
			}
		}
	}
	return samplesPerFrame
}
