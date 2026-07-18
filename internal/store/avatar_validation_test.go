package store

import (
	"errors"
	"testing"
)

func TestNormalizeAvatarClientAcceptsProviderModelDisplayName(t *testing.T) {
	got, err := normalizeAvatarClient(AvatarClientProvenance{
		Runtime: " cursor ",
		Model:   " GPT-5.6 Sol ",
		Recipe:  " avatar initial ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Runtime != "cursor" || got.Model != "GPT-5.6 Sol" || got.Recipe != "avatar initial" {
		t.Fatalf("normalized provenance = %#v", got)
	}
}

func TestNormalizeAvatarClientRejectsNonPortableWhitespace(t *testing.T) {
	for _, model := range []string{"GPT\t5", "GPT\n5", "GPT\u00a05"} {
		if _, err := normalizeAvatarClient(AvatarClientProvenance{Model: model}); !errors.Is(err, ErrAvatarInputInvalid) {
			t.Errorf("normalize model %q error = %v", model, err)
		}
	}
}
