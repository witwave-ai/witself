package agentemail

import "testing"

func TestDeriveAgentSegmentSettledPipeline(t *testing.T) {
	cases := map[string]string{
		" Scott__Thomas...Jr ": "scott-thomas-jr",
		"ＦＯＯ Bar":              "foo-bar",
		"Agent🚀Name":           "agentname",
		"--a---b--":            "a-b",
	}
	for input, want := range cases {
		got, err := DeriveAgentSegment(input)
		if err != nil || got != want {
			t.Errorf("DeriveAgentSegment(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"🚀", "postmaster", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if _, err := DeriveAgentSegment(input); err == nil {
			t.Errorf("DeriveAgentSegment(%q) succeeded", input)
		}
	}
}

func TestComposeAndParseRecipient(t *testing.T) {
	parts, err := ComposeAddress("scott", "realm_abcdefghijkl2345", "Agent-Mail.Witwave.AI.")
	if err != nil {
		t.Fatal(err)
	}
	if parts.BaseAddress != "scott.abcdefghijkl2345@agent-mail.witwave.ai" {
		t.Fatalf("base address = %q", parts.BaseAddress)
	}
	parsed, err := ParseRecipient("<SCOTT.ABCDEFGHIJKL2345+SignUp-1@AGENT-MAIL.WITWAVE.AI>", "agent-mail.witwave.ai")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.BaseAddress != parts.BaseAddress || parsed.SubaddressTag != "signup-1" {
		t.Fatalf("parsed = %+v", parsed)
	}
}

func TestParseRecipientFailsClosed(t *testing.T) {
	for _, recipient := range []string{
		"postmaster@agent-mail.witwave.ai",
		"a.b.c@agent-mail.witwave.ai",
		"scott.bad!label@agent-mail.witwave.ai",
		"scott.abcdefghijkl2345+@agent-mail.witwave.ai",
		"scott.abcdefghijkl2345@other.example",
	} {
		if _, err := ParseRecipient(recipient, "agent-mail.witwave.ai"); err == nil {
			t.Errorf("ParseRecipient(%q) succeeded", recipient)
		}
	}
}

func TestParseRecipientEnforcesFullSMTPLocalPartLimit(t *testing.T) {
	base := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.abcdefghijkl2345"
	if len(base) != maximumLocalPartBytes {
		t.Fatalf("test base local part is %d bytes", len(base))
	}
	if _, err := ParseRecipient(base+"@agent-mail.witwave.ai", "agent-mail.witwave.ai"); err != nil {
		t.Fatalf("64-byte base local part: %v", err)
	}
	if _, err := ParseRecipient(base+"+x@agent-mail.witwave.ai", "agent-mail.witwave.ai"); err == nil {
		t.Fatal("tagged local part exceeding 64 bytes succeeded")
	}
	if _, err := ParseRecipient("scott.abcdefghijkl2345+signup@agent-mail.witwave.ai", "agent-mail.witwave.ai"); err != nil {
		t.Fatalf("bounded tagged local part: %v", err)
	}
}
