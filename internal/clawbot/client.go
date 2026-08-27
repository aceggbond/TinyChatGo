package clawbot

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultBaseURL = "https://ilinkai.weixin.qq.com"
	DefaultCDNURL  = "https://novac2c.cdn.weixin.qq.com/c2c"
	channelVersion = "2.4.6"
)

type Client struct {
	HTTP *http.Client
}

type QRCode struct {
	Code string `json:"qrcode"`
	URL  string `json:"qrcode_img_content"`
	Ret  int    `json:"ret"`
}

type QRStatus struct {
	Status       string `json:"status"`
	BotToken     string `json:"bot_token"`
	BotID        string `json:"ilink_bot_id"`
	WeixinUserID string `json:"ilink_user_id"`
	BaseURL      string `json:"baseurl"`
	RedirectHost string `json:"redirect_host"`
}

type Media struct {
	EncryptQueryParam string `json:"encrypt_query_param,omitempty"`
	AESKey            string `json:"aes_key,omitempty"`
	EncryptType       int    `json:"encrypt_type,omitempty"`
	FullURL           string `json:"full_url,omitempty"`
}

type MessageItem struct {
	Type      int        `json:"type,omitempty"`
	MessageID string     `json:"msg_id,omitempty"`
	Text      *TextItem  `json:"text_item,omitempty"`
	Image     *ImageItem `json:"image_item,omitempty"`
	File      *FileItem  `json:"file_item,omitempty"`
}

type TextItem struct {
	Text string `json:"text,omitempty"`
}
type ImageItem struct {
	Media      Media  `json:"media,omitempty"`
	ThumbMedia Media  `json:"thumb_media,omitempty"`
	AESKey     string `json:"aeskey,omitempty"`
	URL        string `json:"url,omitempty"`
	MidSize    int64  `json:"mid_size,omitempty"`
}
type FileItem struct {
	Media    Media  `json:"media,omitempty"`
	FileName string `json:"file_name,omitempty"`
	Length   string `json:"len,omitempty"`
}

type Message struct {
	MessageID    int64         `json:"message_id,omitempty"`
	FromUserID   string        `json:"from_user_id,omitempty"`
	ToUserID     string        `json:"to_user_id,omitempty"`
	ClientID     string        `json:"client_id,omitempty"`
	CreateTime   int64         `json:"create_time_ms,omitempty"`
	MessageType  int           `json:"message_type,omitempty"`
	MessageState int           `json:"message_state,omitempty"`
	Items        []MessageItem `json:"item_list,omitempty"`
	ContextToken string        `json:"context_token,omitempty"`
}

type Updates struct {
	Ret       int       `json:"ret"`
	ErrorCode int       `json:"errcode"`
	Error     string    `json:"errmsg"`
	Messages  []Message `json:"msgs"`
	Buffer    string    `json:"get_updates_buf"`
	TimeoutMS int       `json:"longpolling_timeout_ms"`
}

type Credentials struct{ BaseURL, Token string }

func (c *Client) httpClient(timeout time.Duration) *http.Client {
	if c != nil && c.HTTP != nil {
		copy := *c.HTTP
		copy.Timeout = timeout
		return &copy
	}
	return &http.Client{Timeout: timeout}
}

func (c *Client) StartQR(ctx context.Context) (QRCode, error) {
	var result QRCode
	err := c.request(ctx, http.MethodPost, DefaultBaseURL, "ilink/bot/get_bot_qrcode?bot_type=3", "", map[string]any{"local_token_list": []string{}}, &result, 20*time.Second)
	if err == nil && (result.Code == "" || result.URL == "" || result.Ret != 0) {
		err = errors.New("微信未返回有效绑定二维码")
	}
	return result, err
}

func (c *Client) PollQR(ctx context.Context, baseURL, code string) (QRStatus, error) {
	var result QRStatus
	endpoint := "ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(code)
	err := c.request(ctx, http.MethodGet, baseURL, endpoint, "", nil, &result, 40*time.Second)
	return result, err
}

func (c *Client) GetUpdates(ctx context.Context, credentials Credentials, buffer string) (Updates, error) {
	var result Updates
	err := c.request(ctx, http.MethodPost, credentials.BaseURL, "ilink/bot/getupdates", credentials.Token, map[string]any{
		"get_updates_buf": buffer, "base_info": baseInfo(),
	}, &result, 45*time.Second)
	if err == nil && (result.Ret != 0 || result.ErrorCode != 0) {
		err = fmt.Errorf("微信消息同步失败 ret=%d errcode=%d: %s", result.Ret, result.ErrorCode, result.Error)
	}
	return result, err
}

func (c *Client) SendText(ctx context.Context, credentials Credentials, to, contextToken, text string) error {
	item := MessageItem{Type: 1, Text: &TextItem{Text: text}}
	return c.sendItem(ctx, credentials, to, contextToken, item)
}

func (c *Client) SendMedia(ctx context.Context, credentials Credentials, to, contextToken, fileName string, data []byte, image bool) error {
	uploaded, err := c.upload(ctx, credentials, to, data, image)
	if err != nil {
		return err
	}
	// Weixin expects aes_key in outbound CDN media to be base64(hex(key)),
	// rather than base64(raw key). getuploadurl still receives the ordinary
	// hexadecimal key. Sending the raw-key encoding makes sendmessage fail with
	// ret=-2 / "prepare failed" after an otherwise successful CDN upload.
	hexKey := hex.EncodeToString(uploaded.Key)
	media := Media{EncryptQueryParam: uploaded.DownloadParam, AESKey: base64.StdEncoding.EncodeToString([]byte(hexKey)), EncryptType: 1}
	item := MessageItem{}
	if image {
		item.Type = 2
		item.Image = &ImageItem{Media: media, MidSize: int64(uploaded.CipherSize)}
	} else {
		item.Type = 4
		item.File = &FileItem{Media: media, FileName: fileName, Length: strconv.Itoa(len(data))}
	}
	return c.sendItem(ctx, credentials, to, contextToken, item)
}

func (c *Client) DownloadMedia(ctx context.Context, media Media) ([]byte, error) {
	target := strings.TrimSpace(media.FullURL)
	if target == "" && media.EncryptQueryParam != "" {
		target = DefaultCDNURL + "/download?encrypted_query_param=" + url.QueryEscape(media.EncryptQueryParam)
	}
	if target == "" {
		return nil, errors.New("微信媒体缺少下载地址")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient(60 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("微信媒体下载失败 %d", resp.StatusCode)
	}
	ciphertext, err := io.ReadAll(io.LimitReader(resp.Body, 1<<30))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(media.AESKey) == "" {
		// Some inbound image variants contain a direct plaintext URL instead of
		// a CDN encryption reference.
		return ciphertext, nil
	}
	key, err := decodeMediaAESKey(media.AESKey)
	if err != nil {
		return nil, err
	}
	return decryptECB(ciphertext, key)
}

func decodeMediaAESKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if len(value) == 32 {
		if key, err := hex.DecodeString(value); err == nil {
			return key, nil
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("解析微信媒体密钥: %w", err)
	}
	if len(decoded) == 32 {
		if key, decodeErr := hex.DecodeString(string(decoded)); decodeErr == nil {
			return key, nil
		}
	}
	if len(decoded) != 16 {
		return nil, errors.New("微信媒体密钥长度无效")
	}
	return decoded, nil
}

func (c *Client) sendItem(ctx context.Context, credentials Credentials, to, contextToken string, item MessageItem) error {
	clientID := randomHex(16)
	body := map[string]any{"msg": Message{
		FromUserID: "", ToUserID: to, ClientID: "tinychatgo-" + clientID,
		MessageType: 2, MessageState: 2, Items: []MessageItem{item}, ContextToken: contextToken,
	}, "base_info": baseInfo()}
	var result struct {
		Ret   int    `json:"ret"`
		Error string `json:"errmsg"`
	}
	if err := c.request(ctx, http.MethodPost, credentials.BaseURL, "ilink/bot/sendmessage", credentials.Token, body, &result, 20*time.Second); err != nil {
		return err
	}
	if result.Ret != 0 {
		return fmt.Errorf("微信发送失败 ret=%d: %s", result.Ret, result.Error)
	}
	return nil
}

type uploadedMedia struct {
	DownloadParam string
	Key           []byte
	CipherSize    int
}

func (c *Client) upload(ctx context.Context, credentials Credentials, to string, plaintext []byte, image bool) (uploadedMedia, error) {
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return uploadedMedia{}, err
	}
	fileKey := randomHex(16)
	ciphertext, err := encryptECB(plaintext, key)
	if err != nil {
		return uploadedMedia{}, err
	}
	digest := md5.Sum(plaintext)
	mediaType := 3
	if image {
		mediaType = 1
	}
	body := map[string]any{
		"filekey": fileKey, "media_type": mediaType, "to_user_id": to,
		"rawsize": len(plaintext), "rawfilemd5": hex.EncodeToString(digest[:]),
		"filesize": len(ciphertext), "no_need_thumb": true, "aeskey": hex.EncodeToString(key), "base_info": baseInfo(),
	}
	var uploadURL struct {
		Param   string `json:"upload_param"`
		FullURL string `json:"upload_full_url"`
	}
	if err = c.request(ctx, http.MethodPost, credentials.BaseURL, "ilink/bot/getuploadurl", credentials.Token, body, &uploadURL, 20*time.Second); err != nil {
		return uploadedMedia{}, err
	}
	target := strings.TrimSpace(uploadURL.FullURL)
	if target == "" && uploadURL.Param != "" {
		target = DefaultCDNURL + "/upload?encrypted_query_param=" + url.QueryEscape(uploadURL.Param) + "&filekey=" + url.QueryEscape(fileKey)
	}
	if target == "" {
		return uploadedMedia{}, errors.New("微信未返回媒体上传地址")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(ciphertext))
	if err != nil {
		return uploadedMedia{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.httpClient(45 * time.Second).Do(req)
	if err != nil {
		return uploadedMedia{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return uploadedMedia{}, fmt.Errorf("微信媒体上传失败 %d: %s", resp.StatusCode, raw)
	}
	downloadParam := resp.Header.Get("X-Encrypted-Param")
	if downloadParam == "" {
		return uploadedMedia{}, errors.New("微信媒体上传响应缺少下载参数")
	}
	return uploadedMedia{DownloadParam: downloadParam, Key: key, CipherSize: len(ciphertext)}, nil
}

func (c *Client) request(ctx context.Context, method, baseURL, endpoint, token string, body any, out any, timeout time.Duration) error {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+"/"+strings.TrimLeft(endpoint, "/"), reader)
	if err != nil {
		return err
	}
	req.Header.Set("iLink-App-Id", "bot")
	req.Header.Set("iLink-App-ClientVersion", "132102")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("AuthorizationType", "ilink_bot_token")
		req.Header.Set("X-WECHAT-UIN", randomUIN())
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient(timeout).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("微信接口 %s 返回 %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) != 0 {
		if err = json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("解析微信接口响应: %w", err)
		}
	}
	return nil
}

func baseInfo() map[string]string {
	return map[string]string{"channel_version": channelVersion, "bot_agent": "TinyChatGo/1.1.4"}
}
func randomHex(size int) string {
	raw := make([]byte, size)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}
func randomUIN() string {
	raw := make([]byte, 4)
	_, _ = rand.Read(raw)
	value := uint64(raw[0])<<24 | uint64(raw[1])<<16 | uint64(raw[2])<<8 | uint64(raw[3])
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(value, 10)))
}

func encryptECB(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padding := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte(nil), plaintext...), bytes.Repeat([]byte{byte(padding)}, padding)...)
	for offset := 0; offset < len(padded); offset += aes.BlockSize {
		block.Encrypt(padded[offset:offset+aes.BlockSize], padded[offset:offset+aes.BlockSize])
	}
	return padded, nil
}

func decryptECB(ciphertext, key []byte) ([]byte, error) {
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("微信媒体密文长度无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plain := append([]byte(nil), ciphertext...)
	for offset := 0; offset < len(plain); offset += aes.BlockSize {
		block.Decrypt(plain[offset:offset+aes.BlockSize], plain[offset:offset+aes.BlockSize])
	}
	padding := int(plain[len(plain)-1])
	if padding < 1 || padding > aes.BlockSize || padding > len(plain) {
		return nil, errors.New("微信媒体填充无效")
	}
	for _, value := range plain[len(plain)-padding:] {
		if int(value) != padding {
			return nil, errors.New("微信媒体填充无效")
		}
	}
	return plain[:len(plain)-padding], nil
}
