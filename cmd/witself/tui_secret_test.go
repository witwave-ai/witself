package main

import (
	"context"
	"errors"
	"testing"

	"github.com/witwave-ai/witself/internal/agenttui"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/sealed"
	"github.com/witwave-ai/witself/internal/secretclient"
)

type fakeTUISecretReader struct {
	secret   *client.Secret
	value    []byte
	err      error
	getErr   error
	accessed bool
}

func (f *fakeTUISecretReader) Get(context.Context, string) (*client.Secret, error) {
	return f.secret, f.getErr
}
func (f *fakeTUISecretReader) RevealExistingField(_ context.Context, secretID, fieldID, retry string) ([]byte, error) {
	f.accessed = secretID == f.secret.ID && fieldID == "fld_selected" && retry != ""
	return f.value, f.err
}

func TestTUISecretAccessIsExactAndClearsFailedValues(t *testing.T) {
	for _, test := range []struct {
		name, kind, encoding string
		sensitive            bool
		value                []byte
		cancel               bool
		accessErr            error
		want                 string
		wantErr              bool
	}{
		{name: "password", kind: "password", encoding: sealed.ValueEncodingUTF8, sensitive: true, value: []byte("synthetic password"), want: "synthetic password"},
		{name: "public", kind: "username", encoding: sealed.ValueEncodingUTF8, value: []byte("demo-user"), want: "demo-user"},
		{name: "binary", kind: "private_key", encoding: sealed.ValueEncodingBinary, sensitive: true, value: []byte{0, 1, 2}, want: "AAEC"},
		{name: "totp", kind: "totp", encoding: sealed.ValueEncodingJSON, sensitive: true, value: []byte(`{"seed":"fake"}`), wantErr: true},
		{name: "bad-utf8", kind: "password", encoding: sealed.ValueEncodingUTF8, sensitive: true, value: []byte{0xff}, wantErr: true},
		{name: "bad-json", kind: "note", encoding: sealed.ValueEncodingJSON, sensitive: true, value: []byte("not json"), wantErr: true},
		{name: "canceled", kind: "password", encoding: sealed.ValueEncodingUTF8, sensitive: true, value: []byte("discard-on-cancel"), cancel: true, wantErr: true},
		{name: "error-with-bytes", kind: "password", encoding: sealed.ValueEncodingUTF8, sensitive: true, value: []byte("discard-on-error"), accessErr: errors.New("do-not-display-upstream"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			field := client.SecretField{ID: "fld_selected", Kind: test.kind, Encoding: test.encoding, Sensitive: test.sensitive}
			if !test.sensitive {
				v := string(test.value)
				field.PublicValue = &v
			}
			service := &fakeTUISecretReader{secret: &client.Secret{ID: "sec_selected", Lifecycle: "active", Fields: []client.SecretField{field}}, value: test.value, err: test.accessErr}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.cancel {
				cancel()
			}
			value, err := revealTUISecret(ctx, service, "sec_selected", "fld_selected")
			defer clear(value)
			if (err != nil) != test.wantErr {
				t.Fatalf("unexpected error state: %v", err)
			}
			if string(value) != test.want {
				t.Fatal("unexpected selected field result")
			}
			if test.kind == "totp" && service.accessed {
				t.Fatal("TOTP enrollment material accessed")
			}
			if !test.sensitive && service.accessed {
				t.Fatal("public field used sealed access")
			}
			if service.accessed && test.wantErr {
				for _, b := range test.value {
					if b != 0 {
						t.Fatal("failed private buffer retained")
					}
				}
			}
			if err != nil && err.Error() == "do-not-display-upstream" {
				t.Fatal("provider error leaked")
			}
		})
	}
}

func TestTUISecretRefusesWrongSecretOrField(t *testing.T) {
	for _, s := range []*client.Secret{
		nil, {ID: "sec_other", Lifecycle: "active"}, {ID: "sec_selected", Lifecycle: "archived"},
		{ID: "sec_selected", Lifecycle: "active", Fields: []client.SecretField{{ID: "fld_other", Sensitive: true}}},
	} {
		service := &fakeTUISecretReader{secret: s}
		value, err := revealTUISecret(context.Background(), service, "sec_selected", "fld_selected")
		if value != nil || err == nil || service.accessed {
			clear(value)
			t.Fatal("nonmatching or inactive field reached access")
		}
	}
}

func TestTUISecretTypedReasons(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want agenttui.SecretRevealReason
	}{
		{secretclient.ErrKeyUnavailable, agenttui.SecretUnenrolled},
		{secretclient.ErrKeyMismatch, agenttui.SecretMismatch},
		{context.Canceled, agenttui.SecretCanceled},
		{errors.New("PRIVATE_VALUE /private/path"), agenttui.SecretUnavailable},
	} {
		service := &fakeTUISecretReader{secret: &client.Secret{ID: "sec_selected", Lifecycle: "active", Fields: []client.SecretField{{ID: "fld_selected", Kind: "password", Sensitive: true, Encoding: sealed.ValueEncodingUTF8}}}, value: []byte("discard"), err: tc.err}
		value, err := revealTUISecret(context.Background(), service, "sec_selected", "fld_selected")
		if value != nil || !errors.Is(err, tc.want) {
			t.Fatal("incorrect typed reveal reason")
		}
		for _, b := range service.value {
			if b != 0 {
				t.Fatal("failed reveal retained bytes")
			}
		}
	}
}
