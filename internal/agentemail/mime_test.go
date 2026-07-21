package agentemail

import (
	"errors"
	"fmt"
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
