package sealed

import "crypto/rand"

const (
	// DefaultPasswordLength is the generated password length used by the
	// default policy.
	DefaultPasswordLength = 32
	// MaxPasswordLength bounds generated password allocation and work.
	MaxPasswordLength = 4096
)

const (
	lowercaseCharacters = "abcdefghijklmnopqrstuvwxyz"
	uppercaseCharacters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digitCharacters     = "0123456789"
	symbolCharacters    = "!@#$%^&*()-_=+[]{}:,.?"
	ambiguousCharacters = "01IOilo|"
)

// PasswordPolicy selects the independent character classes used by
// GeneratePassword. The result contains at least one character from every
// enabled class.
type PasswordPolicy struct {
	Length           int
	Lowercase        bool
	Uppercase        bool
	Digits           bool
	Symbols          bool
	ExcludeAmbiguous bool
}

// DefaultPasswordPolicy returns the v1 32-character policy.
func DefaultPasswordPolicy() PasswordPolicy {
	return PasswordPolicy{
		Length:           DefaultPasswordLength,
		Lowercase:        true,
		Uppercase:        true,
		Digits:           true,
		Symbols:          true,
		ExcludeAmbiguous: false,
	}
}

// GeneratePassword returns an unbiased cryptographically random password with
// at least one character from every enabled class. Call GeneratePasswordBytes
// when the password will be sealed immediately and should remain clearable.
func GeneratePassword(policy PasswordPolicy) (string, error) {
	password, err := GeneratePasswordBytes(policy)
	if err != nil {
		return "", err
	}
	result := string(password)
	clear(password)
	return result, nil
}

// GeneratePasswordBytes is the clearable form used by generate-and-store
// clients. The caller owns the returned buffer and must clear it after use.
func GeneratePasswordBytes(policy PasswordPolicy) ([]byte, error) {
	classes := make([]string, 0, 4)
	if policy.Lowercase {
		classes = append(classes, filteredClass(lowercaseCharacters, policy.ExcludeAmbiguous))
	}
	if policy.Uppercase {
		classes = append(classes, filteredClass(uppercaseCharacters, policy.ExcludeAmbiguous))
	}
	if policy.Digits {
		classes = append(classes, filteredClass(digitCharacters, policy.ExcludeAmbiguous))
	}
	if policy.Symbols {
		classes = append(classes, filteredClass(symbolCharacters, policy.ExcludeAmbiguous))
	}
	if policy.Length < len(classes) || policy.Length < 1 ||
		policy.Length > MaxPasswordLength || len(classes) == 0 {
		return nil, ErrInvalidPasswordPolicy
	}

	combined := ""
	for _, class := range classes {
		if class == "" {
			return nil, ErrInvalidPasswordPolicy
		}
		combined += class
	}
	password := make([]byte, policy.Length)
	for i, class := range classes {
		index, err := randomIndex(len(class))
		if err != nil {
			clear(password)
			return nil, err
		}
		password[i] = class[index]
	}
	for i := len(classes); i < len(password); i++ {
		index, err := randomIndex(len(combined))
		if err != nil {
			clear(password)
			return nil, err
		}
		password[i] = combined[index]
	}
	for i := len(password) - 1; i > 0; i-- {
		j, err := randomIndex(i + 1)
		if err != nil {
			clear(password)
			return nil, err
		}
		password[i], password[j] = password[j], password[i]
	}
	return password, nil
}

func filteredClass(class string, excludeAmbiguous bool) string {
	if !excludeAmbiguous {
		return class
	}
	filtered := make([]byte, 0, len(class))
	for i := range len(class) {
		if !containsByte(ambiguousCharacters, class[i]) {
			filtered = append(filtered, class[i])
		}
	}
	return string(filtered)
}

func containsByte(set string, value byte) bool {
	for i := range len(set) {
		if set[i] == value {
			return true
		}
	}
	return false
}

// randomIndex uses rejection sampling so byte-to-alphabet reduction is not
// biased. Every v1 alphabet is smaller than 256 characters.
func randomIndex(size int) (int, error) {
	if size < 1 || size > 256 {
		return 0, ErrInvalidPasswordPolicy
	}
	limit := 256 - (256 % size)
	var sample [1]byte
	for {
		if _, err := rand.Read(sample[:]); err != nil {
			return 0, ErrRandomSource
		}
		if int(sample[0]) < limit {
			return int(sample[0]) % size, nil
		}
	}
}
