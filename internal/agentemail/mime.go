package agentemail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const (
	maximumMIMEHeaderBytes = 256 * 1024
	maximumMIMEParts       = 64
	maximumMIMEDepth       = 8
	maximumDecodedText     = 1024 * 1024
	maximumHeaderValue     = 4096
	maximumMessageID       = 998
)

var (
	// ErrMIMEInvalid reports an empty, oversized, or structurally invalid message.
	ErrMIMEInvalid = errors.New("invalid MIME message")
	// ErrMIMEHeaderLimit reports a message whose header section is too large.
	ErrMIMEHeaderLimit = errors.New("MIME header exceeds limit")
	// ErrMIMEPartLimit reports a message with too many MIME parts.
	ErrMIMEPartLimit = errors.New("MIME part limit exceeded")
	// ErrMIMEDepthLimit reports a message nested beyond the MIME depth bound.
	ErrMIMEDepthLimit = errors.New("MIME nesting depth exceeded")
	// ErrMIMETextLimit reports decoded text beyond the bounded read limit.
	ErrMIMETextLimit = errors.New("decoded MIME text exceeds limit")
	// ErrMIMETransfer reports an unsupported content-transfer encoding.
	ErrMIMETransfer = errors.New("unsupported MIME transfer encoding")
	// ErrMIMECharset reports an unsupported MIME character set.
	ErrMIMECharset = errors.New("unsupported MIME charset")
)

// ParsedMessage is a bounded deterministic MIME projection. Header values and
// text remain untrusted external content. Attachment bytes are never returned.
type ParsedMessage struct {
	HeaderFrom      string
	HeaderTo        string
	HeaderSubject   string
	MIMEMessageID   string
	MessageDate     *time.Time
	AttachmentCount int64
	Text            string
	TextKind        string
}

// ParseMessage extracts bounded metadata and, when includeText is true, one
// preferred human-readable part. It never returns attachment content. An
// error is safe to collapse to ParseErrorCode while retaining the raw message.
func ParseMessage(raw []byte, includeText bool) (ParsedMessage, error) {
	if len(raw) == 0 || len(raw) > PilotMaximumRawBytes {
		return ParsedMessage{}, ErrMIMEInvalid
	}
	end := headerEnd(raw)
	if end < 0 {
		return ParsedMessage{}, ErrMIMEInvalid
	} else if end > maximumMIMEHeaderBytes {
		return ParsedMessage{}, ErrMIMEHeaderLimit
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ParsedMessage{}, fmt.Errorf("%w: read message", ErrMIMEInvalid)
	}
	parsed := ParsedMessage{
		HeaderFrom:    decodedHeader(message.Header.Get("From"), maximumHeaderValue),
		HeaderTo:      decodedHeader(message.Header.Get("To"), maximumHeaderValue),
		HeaderSubject: decodedHeader(message.Header.Get("Subject"), maximumHeaderValue),
		MIMEMessageID: boundedText(message.Header.Get("Message-ID"), maximumMessageID),
	}
	if rawDate := strings.TrimSpace(message.Header.Get("Date")); rawDate != "" {
		if parsedDate, err := mail.ParseDate(rawDate); err == nil {
			parsedDate = parsedDate.UTC()
			parsed.MessageDate = &parsedDate
		}
	}

	walker := mimeWalker{includeText: includeText}
	if err := walker.walk(textproto.MIMEHeader(message.Header), message.Body, 0); err != nil {
		parsed.AttachmentCount = int64(walker.attachmentCount)
		return parsed, err
	}
	parsed.AttachmentCount = int64(walker.attachmentCount)
	if includeText {
		if walker.plainText != "" {
			parsed.Text = walker.plainText
			parsed.TextKind = "text/plain"
		} else if walker.htmlText != "" {
			parsed.Text = walker.htmlText
			parsed.TextKind = "text/html-rendered"
		}
	}
	return parsed, nil
}

// ParseErrorCode converts parser failures into bounded value-free storage and
// telemetry codes. Raw parser errors may include attacker-controlled boundary
// or charset text and must not be persisted or logged.
func ParseErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrMIMEHeaderLimit):
		return "header_limit"
	case errors.Is(err, ErrMIMEPartLimit):
		return "part_limit"
	case errors.Is(err, ErrMIMEDepthLimit):
		return "depth_limit"
	case errors.Is(err, ErrMIMETextLimit):
		return "text_limit"
	case errors.Is(err, ErrMIMETransfer):
		return "transfer_encoding"
	case errors.Is(err, ErrMIMECharset):
		return "charset"
	default:
		return "malformed_message"
	}
}

type mimeWalker struct {
	includeText     bool
	parts           int
	attachmentCount int
	decodedBytes    int
	plainText       string
	htmlText        string
}

func (w *mimeWalker) walk(header textproto.MIMEHeader, body io.Reader, depth int) error {
	if depth > maximumMIMEDepth {
		return ErrMIMEDepthLimit
	}
	w.parts++
	if w.parts > maximumMIMEParts {
		return ErrMIMEPartLimit
	}
	contentType := header.Get("Content-Type")
	mediaType := "text/plain"
	params := map[string]string{}
	if contentType != "" {
		var err error
		mediaType, params, err = mime.ParseMediaType(contentType)
		if err != nil {
			return fmt.Errorf("%w: content type", ErrMIMEInvalid)
		}
	}
	mediaType = strings.ToLower(mediaType)
	disposition, dispositionParams, dispositionErr := mime.ParseMediaType(header.Get("Content-Disposition"))
	if dispositionErr != nil && header.Get("Content-Disposition") != "" {
		return fmt.Errorf("%w: content disposition", ErrMIMEInvalid)
	}
	filename := params["name"]
	if dispositionParams["filename"] != "" {
		filename = dispositionParams["filename"]
	}
	if strings.EqualFold(disposition, "attachment") || filename != "" {
		w.attachmentCount++
		return nil
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return fmt.Errorf("%w: multipart boundary", ErrMIMEInvalid)
		}
		reader := multipart.NewReader(body, boundary)
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%w: multipart body", ErrMIMEInvalid)
			}
			walkErr := w.walk(part.Header, part, depth+1)
			_ = part.Close()
			if walkErr != nil {
				return walkErr
			}
		}
	}
	// Every non-text leaf is an attachment for the bounded pilot projection,
	// including inline parts that omit both filename and attachment disposition.
	if mediaType != "text/plain" && mediaType != "text/html" {
		w.attachmentCount++
		return nil
	}
	if !w.includeText {
		return nil
	}
	if mediaType == "text/plain" && w.plainText != "" {
		return nil
	}
	if mediaType == "text/html" && w.htmlText != "" {
		return nil
	}
	decoded, err := decodedTransferReader(header.Get("Content-Transfer-Encoding"), body)
	if err != nil {
		return err
	}
	if label := strings.TrimSpace(params["charset"]); label != "" && !strings.EqualFold(label, "utf-8") && !strings.EqualFold(label, "us-ascii") {
		decoded, err = charset.NewReaderLabel(label, decoded)
		if err != nil {
			return ErrMIMECharset
		}
	}
	remaining := maximumDecodedText - w.decodedBytes
	if remaining <= 0 {
		return ErrMIMETextLimit
	}
	content, err := io.ReadAll(io.LimitReader(decoded, int64(remaining+1)))
	if err != nil {
		return fmt.Errorf("%w: decode text", ErrMIMEInvalid)
	}
	if len(content) > remaining {
		return ErrMIMETextLimit
	}
	w.decodedBytes += len(content)
	text := boundedText(string(content), maximumDecodedText)
	if mediaType == "text/html" {
		text = renderHTMLText(text)
		w.htmlText = text
	} else {
		w.plainText = text
	}
	return nil
}

func decodedTransferReader(encoding string, body io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "7bit", "8bit", "binary":
		return body, nil
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, body), nil
	case "quoted-printable":
		return quotedprintable.NewReader(body), nil
	default:
		return nil, ErrMIMETransfer
	}
}

func headerEnd(raw []byte) int {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return i
	}
	return bytes.Index(raw, []byte("\n\n"))
}

func decodedHeader(value string, limit int) string {
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err == nil {
		value = decoded
	}
	return boundedText(value, limit)
}

func boundedText(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func renderHTMLText(value string) string {
	tokenizer := xhtml.NewTokenizer(strings.NewReader(value))
	var b strings.Builder
	suppressed := 0
	for {
		typeOfToken := tokenizer.Next()
		switch typeOfToken {
		case xhtml.ErrorToken:
			return strings.Join(strings.Fields(html.UnescapeString(b.String())), " ")
		case xhtml.StartTagToken:
			token := tokenizer.Token()
			if token.Data == "script" || token.Data == "style" || token.Data == "template" {
				suppressed++
			} else if suppressed == 0 && isHTMLBlock(token.Data) {
				b.WriteByte(' ')
			}
		case xhtml.EndTagToken:
			token := tokenizer.Token()
			if token.Data == "script" || token.Data == "style" || token.Data == "template" {
				if suppressed > 0 {
					suppressed--
				}
			} else if suppressed == 0 && isHTMLBlock(token.Data) {
				b.WriteByte(' ')
			}
		case xhtml.TextToken:
			if suppressed == 0 {
				b.Write(tokenizer.Text())
				b.WriteByte(' ')
			}
		}
	}
}

func isHTMLBlock(name string) bool {
	switch name {
	case "br", "p", "div", "li", "tr", "td", "th", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "pre":
		return true
	default:
		return false
	}
}
