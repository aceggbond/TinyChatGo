//go:build windows

package gui

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"runtime"
	"strings"
	"syscall"
	unicodeutf16 "unicode/utf16"
	"unsafe"

	"hfsgo/internal/server"
)

var (
	richEditDLL      = syscall.NewLazyDLL("msftedit.dll")
	richStreamProc   = syscall.NewCallback(streamRichText)
	richEditLoadOnce bool
	richEditLoaded   bool
)

const (
	emStreamIn       = 0x0449
	emSetBackground  = 0x0443
	streamFormatRTF  = 0x0002
	chatImageMaxWide = 300
	chatImageMaxHigh = 220
)

type editStream struct {
	Cookie   uintptr
	Error    uint32
	Callback uintptr
}

type richTextSource struct {
	data   []byte
	offset int
}

func loadRichEdit() bool {
	if richEditLoadOnce {
		return richEditLoaded
	}
	richEditLoadOnce = true
	richEditLoaded = richEditDLL.Load() == nil
	return richEditLoaded
}

func streamRichText(source *richTextSource, buffer *byte, requested int32, transferred *int32) uintptr {
	if source == nil || buffer == nil || transferred == nil || requested < 0 {
		return 1
	}
	*transferred = 0
	if source.offset >= len(source.data) || requested == 0 {
		return 0
	}
	count := int(requested)
	remaining := len(source.data) - source.offset
	if count > remaining {
		count = remaining
	}
	target := unsafe.Slice(buffer, count)
	copied := copy(target, source.data[source.offset:source.offset+count])
	source.offset += copied
	*transferred = int32(copied)
	return 0
}

func setChatHistoryConversation(conversation *server.ChatConversation) {
	if app == nil || app.chatHistory == 0 {
		return
	}
	if !richEditLoaded {
		setText(app.chatHistory, chatConversationPlainText(conversation))
		return
	}
	streamChatHistory(chatConversationRTF(conversation))
	sendMessage.Call(uintptr(app.chatHistory), emSetSel, ^uintptr(0), ^uintptr(0))
	sendMessage.Call(uintptr(app.chatHistory), emScrollCaret, 0, 0)
}

func chatConversationPlainText(conversation *server.ChatConversation) string {
	if conversation == nil {
		return "选择访客后可查看聊天内容。"
	}
	if len(conversation.Messages) == 0 {
		return "会话已连接，尚无消息。"
	}
	var output strings.Builder
	for _, message := range conversation.Messages {
		who := conversation.Name
		if name := strings.TrimSpace(message.Name); name != "" {
			who = name
		}
		if message.Sender == "admin" {
			who = "管理员（后台）"
		}
		text := message.Text
		if message.Kind == server.ChatMessageKindImage {
			text = "[图片]"
		}
		fmt.Fprintf(&output, "[%s] %s：%s\r\n", message.SentAt.Local().Format("15:04:05"), who, text)
	}
	return output.String()
}

func streamChatHistory(data []byte) {
	source := &richTextSource{data: data}
	stream := editStream{
		Cookie:   uintptr(unsafe.Pointer(source)),
		Callback: richStreamProc,
	}
	sendMessage.Call(
		uintptr(app.chatHistory),
		emStreamIn,
		streamFormatRTF,
		uintptr(unsafe.Pointer(&stream)),
	)
	runtime.KeepAlive(source)
	runtime.KeepAlive(stream)
}

func chatConversationRTF(conversation *server.ChatConversation) []byte {
	var output bytes.Buffer
	output.WriteString(`{\rtf1\ansi\deff0{\fonttbl{\f0\fnil Segoe UI;}}`)
	output.WriteString(`{\colortbl;\red31\green42\blue58;\red23\green105\blue170;\red112\green128\blue148;\red36\green116\blue64;}`)
	output.WriteString(`\viewkind4\uc1\pard\f0\fs20 `)
	if conversation == nil {
		output.WriteString(`\cf3 `)
		writeRTFText(&output, "选择访客后可查看聊天内容；图片会直接显示在这里。")
		output.WriteString(`\par}`)
		return output.Bytes()
	}
	if len(conversation.Messages) == 0 {
		output.WriteString(`\cf3 `)
		writeRTFText(&output, "会话已连接，尚无消息。")
		output.WriteString(`\par}`)
		return output.Bytes()
	}
	for _, message := range conversation.Messages {
		who := conversation.Name
		if name := strings.TrimSpace(message.Name); name != "" {
			who = name
		}
		color := 4
		if message.Sender == "admin" {
			who = "管理员（后台）"
			color = 2
		} else if conversation.ID == server.ChatGroupConversationID {
			who += "（访客）"
		}
		output.WriteString(`\pard\sa40\cf3\fs17 `)
		writeRTFText(&output, "["+message.SentAt.Local().Format("15:04:05")+"] ")
		output.WriteString(fmt.Sprintf(`\cf%d\b `, color))
		writeRTFText(&output, who)
		output.WriteString(`\b0\line\cf1\fs20 `)
		if strings.EqualFold(message.Kind, server.ChatMessageKindImage) {
			if !writeRTFImage(&output, message.Data, message.Mime) {
				writeRTFText(&output, "[图片数据不可用]")
			}
		} else {
			writeRTFText(&output, message.Text)
		}
		output.WriteString(`\par `)
	}
	output.WriteByte('}')
	return output.Bytes()
}

func writeRTFText(output *bytes.Buffer, value string) {
	for _, unit := range unicodeutf16.Encode([]rune(value)) {
		switch unit {
		case '\\', '{', '}':
			output.WriteByte('\\')
			output.WriteByte(byte(unit))
		case '\r':
		case '\n':
			output.WriteString(`\line `)
		default:
			if unit >= 0x20 && unit <= 0x7e {
				output.WriteByte(byte(unit))
			} else {
				fmt.Fprintf(output, `\u%d?`, int16(unit))
			}
		}
	}
}

func writeRTFImage(output *bytes.Buffer, data []byte, mimeType string) bool {
	if len(data) == 0 {
		return false
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return false
	}
	blip := ""
	switch {
	case strings.EqualFold(mimeType, "image/png") && format == "png":
		blip = `\pngblip`
	case strings.EqualFold(mimeType, "image/jpeg") && format == "jpeg":
		blip = `\jpegblip`
	default:
		return false
	}
	width, height := fitChatImage(config.Width, config.Height)
	fmt.Fprintf(
		output,
		`{\pict%s\picw%d\pich%d\picwgoal%d\pichgoal%d `,
		blip,
		config.Width,
		config.Height,
		width*15,
		height*15,
	)
	encoder := hex.NewEncoder(output)
	if _, err = encoder.Write(data); err != nil {
		return false
	}
	output.WriteByte('}')
	return true
}

func fitChatImage(width, height int) (int, int) {
	scale := 1.0
	if width > chatImageMaxWide {
		scale = float64(chatImageMaxWide) / float64(width)
	}
	if scaledHeight := float64(height) * scale; scaledHeight > chatImageMaxHigh {
		scale *= float64(chatImageMaxHigh) / scaledHeight
	}
	width = maxInt(1, int(float64(width)*scale+0.5))
	height = maxInt(1, int(float64(height)*scale+0.5))
	return width, height
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
