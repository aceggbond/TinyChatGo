//go:build windows

package gui

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tinychatgo/internal/server"
)

func TestChatConversationRTFEmbedsImagesAndEscapesText(t *testing.T) {
	picture := image.NewRGBA(image.Rect(0, 0, 8, 4))
	picture.Set(0, 0, color.RGBA{R: 0xff, A: 0xff})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	conversation := &server.ChatConversation{
		ID:     "visitor",
		Name:   `访客{一}`,
		Online: true,
		Messages: []server.ChatMessage{
			{
				Kind:   server.ChatMessageKindText,
				Sender: "user",
				Name:   `访客{一}`,
				Text:   `hello {\rtf \par}`,
				SentAt: time.Unix(1, 0),
			},
			{
				Kind:   server.ChatMessageKindImage,
				Sender: "admin",
				Mime:   "image/png",
				Data:   encoded.Bytes(),
				SentAt: time.Unix(2, 0),
			},
		},
	}
	rtf := string(chatConversationRTF(conversation))
	for _, marker := range []string{`\pngblip`, `89504e470d0a1a0a`, `hello \{\\rtf \\par\}`, `\u`} {
		if !strings.Contains(rtf, marker) {
			t.Fatalf("RTF is missing %q: %s", marker, rtf)
		}
	}
	if strings.Contains(rtf, `hello {\rtf \par}`) {
		t.Fatal("chat text was inserted into RTF without escaping")
	}
}

func TestFitChatImageBounds(t *testing.T) {
	for _, size := range [][2]int{{100, 50}, {2000, 1000}, {1000, 2000}} {
		width, height := fitChatImage(size[0], size[1])
		if width < 1 || height < 1 || width > chatImageMaxWide || height > chatImageMaxHigh {
			t.Fatalf("fitChatImage(%d, %d) = %d, %d", size[0], size[1], width, height)
		}
	}
}

func TestReadDroppedChatImage(t *testing.T) {
	picture := image.NewRGBA(image.Rect(0, 0, 12, 6))
	picture.Set(1, 1, color.RGBA{G: 0xff, A: 0xff})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(t.TempDir(), "拖入图片.png")
	if err := os.WriteFile(imagePath, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	data, mimeType, err := readDroppedChatImage(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || mimeType != "image/jpeg" || format != "jpeg" || config.Width != 12 || config.Height != 6 {
		t.Fatalf("dropped image = mime %q, format %q, size %dx%d, error %v", mimeType, format, config.Width, config.Height, err)
	}
}

func TestDecodeClipboardDIB(t *testing.T) {
	const width, height, rowStride = 2, 2, 8
	data := make([]byte, 40+rowStride*height)
	binary.LittleEndian.PutUint32(data[0:4], 40)
	binary.LittleEndian.PutUint32(data[4:8], width)
	binary.LittleEndian.PutUint32(data[8:12], height)
	binary.LittleEndian.PutUint16(data[12:14], 1)
	binary.LittleEndian.PutUint16(data[14:16], 24)
	// DIB rows are bottom-up. Store blue/white below red/green.
	copy(data[40:46], []byte{0xff, 0x00, 0x00, 0xff, 0xff, 0xff})
	copy(data[48:54], []byte{0x00, 0x00, 0xff, 0x00, 0xff, 0x00})
	decoded, err := decodeClipboardDIB(data)
	if err != nil {
		t.Fatal(err)
	}
	assertColor := func(x, y int, want color.RGBA) {
		t.Helper()
		red, green, blue, alpha := decoded.At(x, y).RGBA()
		got := color.RGBA{R: uint8(red >> 8), G: uint8(green >> 8), B: uint8(blue >> 8), A: uint8(alpha >> 8)}
		if got != want {
			t.Fatalf("pixel (%d,%d) = %#v, want %#v", x, y, got, want)
		}
	}
	assertColor(0, 0, color.RGBA{R: 0xff, A: 0xff})
	assertColor(1, 0, color.RGBA{G: 0xff, A: 0xff})
	assertColor(0, 1, color.RGBA{B: 0xff, A: 0xff})
	assertColor(1, 1, color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})
}
