package agentemail

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestParseMessagePrefersPlainTextAndHidesAttachments(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"From: =?UTF-8?Q?Example_Service?= <security@example.com>",
		"To: agent@example.com",
		"Subject: =?UTF-8?Q?Your_code?=",
		"Message-ID: <untrusted@example.com>",
		"Date: Tue, 21 Jul 2026 05:30:00 +0000",
		"Content-Type: multipart/mixed; boundary=mix",
		"",
		"--mix",
		"Content-Type: multipart/alternative; boundary=alt",
		"",
		"--alt",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: quoted-printable",
		"",
		"Your code is 123456.",
		"--alt",
		"Content-Type: text/html; charset=utf-8",
		"",
		"<p>Your <b>code</b> is 999999.</p><script>ignore me</script>",
		"--alt--",
		"--mix",
		"Content-Type: application/octet-stream; name=secret.bin",
		"Content-Disposition: attachment; filename=secret.bin",
		"Content-Transfer-Encoding: base64",
		"",
		"c2VjcmV0LWF0dGFjaG1lbnQ=",
		"--mix--",
		"",
	}, "\r\n"))

	parsed, err := ParseMessage(raw, true)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if parsed.HeaderFrom != "Example Service <security@example.com>" || parsed.HeaderSubject != "Your code" {
		t.Fatalf("decoded headers = from %q subject %q", parsed.HeaderFrom, parsed.HeaderSubject)
	}
	if parsed.TextKind != "text/plain" || !strings.Contains(parsed.Text, "123456") || strings.Contains(parsed.Text, "999999") {
		t.Fatalf("preferred text = %q (%s)", parsed.Text, parsed.TextKind)
	}
	if parsed.AttachmentCount != 1 {
		t.Fatalf("attachment count = %d", parsed.AttachmentCount)
	}
	if parsed.MessageDate == nil || parsed.MessageDate.Unix() != 1784611800 {
		t.Fatalf("message date = %v", parsed.MessageDate)
	}
	if parsed.MIMEMessageID != "<untrusted@example.com>" {
		t.Fatalf("message id = %q", parsed.MIMEMessageID)
	}
}

func TestReplyToHeaderReadsOnlyBoundedTopLevelHeader(t *testing.T) {
	raw := []byte("From: sender@example.com\r\n" +
		"Reply-To: =?UTF-8?Q?Reply_Desk?= <reply@example.com>\r\n" +
		"Content-Type: application/octet-stream\r\n\r\nnot parsed")
	got, err := ReplyToHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Reply Desk <reply@example.com>" {
		t.Fatalf("ReplyToHeader = %q", got)
	}
	if _, err := ReplyToHeader([]byte("missing separator")); !errors.Is(err, ErrMIMEInvalid) {
		t.Fatalf("malformed ReplyToHeader error = %v", err)
	}
}

func TestParseMessageRendersHTMLWithoutExecutableText(t *testing.T) {
	raw := []byte("Subject: html\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<div>Code <b>654321</b></div><style>.x{}</style><script>steal()</script>")
	parsed, err := ParseMessage(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.TextKind != "text/html-rendered" || parsed.Text != "Code 654321" {
		t.Fatalf("rendered html = %q (%s)", parsed.Text, parsed.TextKind)
	}
}

func TestParseMessageDefaultsMissingContentTypeToPlainText(t *testing.T) {
	parsed, err := ParseMessage([]byte("Subject: plain\r\n\r\ncode 111222"), true)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.TextKind != "text/plain" || parsed.Text != "code 111222" {
		t.Fatalf("default text = %q (%s)", parsed.Text, parsed.TextKind)
	}
}

func TestMIMEBodyUsesEarliestHeaderSeparator(t *testing.T) {
	t.Parallel()
	raw := []byte("Subject: mixed\n\nfirst\r\n\r\nsecond")
	body, err := MIMEBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "first\r\n\r\nsecond"; got != want {
		t.Fatalf("MIME body = %q, want %q", got, want)
	}
}

func TestParseMessageReturnsStableBoundedErrorCodes(t *testing.T) {
	raw := []byte("Subject: test\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: attacker-chosen\r\n\r\nbody")
	_, err := ParseMessage(raw, true)
	if !errors.Is(err, ErrMIMETransfer) {
		t.Fatalf("error = %v", err)
	}
	if got := ParseErrorCode(err); got != "transfer_encoding" {
		t.Fatalf("code = %q", got)
	}

	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=x\r\n\r\n")
	for i := 0; i < maximumMIMEParts+1; i++ {
		fmt.Fprintf(&b, "--x\r\nContent-Type: text/plain\r\n\r\npart %d\r\n", i)
	}
	b.WriteString("--x--\r\n")
	_, err = ParseMessage([]byte(b.String()), false)
	if !errors.Is(err, ErrMIMEPartLimit) || ParseErrorCode(err) != "part_limit" {
		t.Fatalf("part limit error = %v (%s)", err, ParseErrorCode(err))
	}
}

func TestParseMessageCountsInlineNonTextLeavesAsAttachments(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"Subject: inline attachment",
		"Content-Type: multipart/mixed; boundary=mix",
		"",
		"--mix",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"body",
		"--mix",
		"Content-Type: image/png",
		"Content-Disposition: inline",
		"",
		"not-really-a-png",
		"--mix",
		"Content-Type: application/pdf",
		"",
		"not-really-a-pdf",
		"--mix--",
		"",
	}, "\r\n"))
	parsed, err := ParseMessage(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.AttachmentCount != 2 {
		t.Fatalf("attachment count = %d; want 2", parsed.AttachmentCount)
	}
}

func TestParseMessageCountsRawAttachmentPayloadBytes(t *testing.T) {
	base64Payload := "YXR0YWNobWVu\r\ndA=="
	quotedPrintablePayload := "a=3Db=0A"

	tests := []struct {
		name            string
		raw             string
		attachmentCount int64
		payloadBytes    int64
	}{
		{
			name:            "no attachment",
			raw:             "Subject: text\r\nContent-Type: text/plain\r\n\r\nbody",
			attachmentCount: 0,
			payloadBytes:    0,
		},
		{
			name: "single attachment",
			raw: strings.Join([]string{
				"Subject: single",
				"Content-Type: application/octet-stream",
				"Content-Disposition: attachment; filename=one.bin",
				"",
				"raw-payload",
			}, "\r\n"),
			attachmentCount: 1,
			payloadBytes:    int64(len("raw-payload")),
		},
		{
			name: "multiple attachments",
			raw: strings.Join([]string{
				"Subject: multiple",
				"Content-Type: multipart/mixed; boundary=mix",
				"",
				"--mix",
				"Content-Type: text/plain",
				"",
				"body",
				"--mix",
				"Content-Type: application/octet-stream",
				"",
				"abc",
				"--mix",
				"Content-Type: image/png",
				"Content-Disposition: inline",
				"",
				"12345",
				"--mix--",
				"",
			}, "\r\n"),
			attachmentCount: 2,
			payloadBytes:    int64(len("abc") + len("12345")),
		},
		{
			name: "nested attachments",
			raw: strings.Join([]string{
				"Subject: nested",
				"Content-Type: multipart/mixed; boundary=outer",
				"",
				"--outer",
				"Content-Type: multipart/related; boundary=inner",
				"",
				"--inner",
				"Content-Type: text/plain",
				"",
				"body",
				"--inner",
				"Content-Type: image/png",
				"",
				"nested-one",
				"--inner--",
				"--outer",
				"Content-Type: application/pdf",
				"Content-Disposition: attachment; filename=outer.pdf",
				"",
				"outer-two",
				"--outer--",
				"",
			}, "\r\n"),
			attachmentCount: 2,
			payloadBytes:    int64(len("nested-one") + len("outer-two")),
		},
		{
			name: "base64 counts encoded octets",
			raw: strings.Join([]string{
				"Subject: base64",
				"Content-Type: multipart/mixed; boundary=mix",
				"",
				"--mix",
				"Content-Type: application/octet-stream",
				"Content-Disposition: attachment; filename=encoded.bin",
				"Content-Transfer-Encoding: base64",
				"",
				base64Payload,
				"--mix--",
				"",
			}, "\r\n"),
			attachmentCount: 1,
			payloadBytes:    int64(len(base64Payload)),
		},
		{
			name: "quoted printable counts encoded octets",
			raw: strings.Join([]string{
				"Subject: quoted printable",
				"Content-Type: multipart/mixed; boundary=mix",
				"",
				"--mix",
				"Content-Type: application/octet-stream",
				"Content-Disposition: attachment; filename=encoded.bin",
				"Content-Transfer-Encoding: quoted-printable",
				"",
				quotedPrintablePayload,
				"--mix--",
				"",
			}, "\r\n"),
			attachmentCount: 1,
			payloadBytes:    int64(len(quotedPrintablePayload)),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := ParseMessage([]byte(test.raw), false)
			if err != nil {
				t.Fatalf("ParseMessage: %v", err)
			}
			if parsed.AttachmentCount != test.attachmentCount {
				t.Fatalf("attachment count = %d; want %d", parsed.AttachmentCount, test.attachmentCount)
			}
			if parsed.AttachmentPayloadBytes != test.payloadBytes {
				t.Fatalf("attachment payload bytes = %d; want %d", parsed.AttachmentPayloadBytes, test.payloadBytes)
			}
		})
	}
}

func TestParseMessageReturnsCountedAttachmentBytesOnLaterParserError(t *testing.T) {
	const payload = "count-me"
	raw := []byte(strings.Join([]string{
		"Subject: partial projection",
		"Content-Type: multipart/mixed; boundary=mix",
		"",
		"--mix",
		"Content-Type: application/octet-stream",
		"Content-Disposition: attachment; filename=first.bin",
		"",
		payload,
		"--mix",
		"Content-Type: application/octet-stream; name=\"unterminated",
		"",
		"not-counted",
		"--mix--",
		"",
	}, "\r\n"))

	parsed, err := ParseMessage(raw, false)
	if !errors.Is(err, ErrMIMEInvalid) {
		t.Fatalf("error = %v; want ErrMIMEInvalid", err)
	}
	if parsed.AttachmentCount != 1 {
		t.Fatalf("attachment count = %d; want 1", parsed.AttachmentCount)
	}
	if parsed.AttachmentPayloadBytes != int64(len(payload)) {
		t.Fatalf("attachment payload bytes = %d; want %d", parsed.AttachmentPayloadBytes, len(payload))
	}
}

func TestMIMEWalkerRejectsAttachmentPayloadByteOverflow(t *testing.T) {
	walker := mimeWalker{attachmentPayloadBytes: math.MaxInt64 - 1}
	err := walker.countAttachmentPayload(strings.NewReader("xx"))
	if !errors.Is(err, ErrMIMEInvalid) {
		t.Fatalf("error = %v; want ErrMIMEInvalid", err)
	}
	if walker.attachmentPayloadBytes != math.MaxInt64-1 {
		t.Fatalf("attachment payload bytes = %d; want unchanged", walker.attachmentPayloadBytes)
	}
}

func TestRetryCanaryChallengeRequiresOneCanonicalUUIDv4Header(t *testing.T) {
	const challenge = "11111111-2222-4333-8444-555555555555"
	raw := []byte("Subject: canary\r\n" + RetryCanaryHeader + ": " + challenge + "\r\n\r\nbody")
	got, present, err := RetryCanaryChallenge(raw)
	if err != nil || !present || got != challenge {
		t.Fatalf("challenge = %q present=%v err=%v", got, present, err)
	}
	if err := ValidateRetryCanaryChallenge(challenge); err != nil {
		t.Fatal(err)
	}
	if got, present, err := RetryCanaryChallenge([]byte("Subject: ordinary\r\n\r\nbody")); err != nil || present || got != "" {
		t.Fatalf("absent challenge = %q present=%v err=%v", got, present, err)
	}
	for name, raw := range map[string][]byte{
		"duplicate":          []byte(RetryCanaryHeader + ": " + challenge + "\r\n" + RetryCanaryHeader + ": " + challenge + "\r\n\r\nbody"),
		"folded":             []byte(RetryCanaryHeader + ": 11111111-2222-4333-8444-\r\n 555555555555\r\n\r\nbody"),
		"uppercase":          []byte(RetryCanaryHeader + ": 11111111-2222-4333-8444-55555555555A\r\n\r\nbody"),
		"wrong UUID version": []byte(RetryCanaryHeader + ": 11111111-2222-3333-8444-555555555555\r\n\r\nbody"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, present, err := RetryCanaryChallenge(raw); err == nil || !present {
				t.Fatalf("present=%v err=%v", present, err)
			}
		})
	}
}
