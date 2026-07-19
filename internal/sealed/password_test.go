package sealed

import (
	"errors"
	"strings"
	"testing"
)

func TestGeneratePasswordDefaultPolicy(t *testing.T) {
	seen := map[string]bool{}
	for range 128 {
		password, err := GeneratePassword(DefaultPasswordPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if len(password) != DefaultPasswordLength {
			t.Fatalf("password length = %d", len(password))
		}
		for name, class := range map[string]string{
			"lowercase": lowercaseCharacters,
			"uppercase": uppercaseCharacters,
			"digit":     digitCharacters,
			"symbol":    symbolCharacters,
		} {
			if !containsAny(password, class) {
				t.Fatalf("password lacks required %s character: %q", name, password)
			}
		}
		seen[password] = true
	}
	if len(seen) != 128 {
		t.Fatalf("generated only %d distinct passwords out of 128", len(seen))
	}
}

func TestGeneratePasswordExcludesAmbiguousAndHonorsSelectedClasses(t *testing.T) {
	policy := PasswordPolicy{Length: 64, Lowercase: true, Uppercase: true, Digits: true, ExcludeAmbiguous: true}
	for range 64 {
		password, err := GeneratePassword(policy)
		if err != nil {
			t.Fatal(err)
		}
		if containsAny(password, ambiguousCharacters) || containsAny(password, symbolCharacters) {
			t.Fatalf("password contains excluded character: %q", password)
		}
		for _, class := range []string{
			filteredClass(lowercaseCharacters, true),
			filteredClass(uppercaseCharacters, true),
			filteredClass(digitCharacters, true),
		} {
			if !containsAny(password, class) {
				t.Fatalf("password lacks selected class: %q", password)
			}
		}
	}
}

func TestGeneratePasswordRejectsImpossiblePolicies(t *testing.T) {
	for _, policy := range []PasswordPolicy{
		{},
		{Length: 8},
		{Length: 0, Lowercase: true},
		{Length: 3, Lowercase: true, Uppercase: true, Digits: true, Symbols: true},
		{Length: MaxPasswordLength + 1, Lowercase: true},
	} {
		if password, err := GeneratePassword(policy); password != "" || !errors.Is(err, ErrInvalidPasswordPolicy) {
			t.Fatalf("GeneratePassword(%+v) = %q, %v", policy, password, err)
		}
	}
}

func TestGeneratePasswordBytesReturnsCallerOwnedClearableBuffer(t *testing.T) {
	password, err := GeneratePasswordBytes(DefaultPasswordPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(password) != DefaultPasswordLength {
		t.Fatalf("password length = %d", len(password))
	}
	clear(password)
	for _, value := range password {
		if value != 0 {
			t.Fatal("password buffer was not clearable")
		}
	}
}

func containsAny(value, set string) bool {
	return strings.IndexFunc(value, func(r rune) bool { return strings.ContainsRune(set, r) }) >= 0
}
