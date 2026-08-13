package auth

import (
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	const pw = "correct horse battery staple"
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(h, scheme+"$") {
		t.Errorf("hash %q does not carry its scheme", h)
	}
	if strings.Contains(h, pw) {
		t.Fatal("hash contains the plaintext password")
	}
	if !VerifyPassword(h, pw) {
		t.Error("correct password rejected")
	}
	if VerifyPassword(h, pw+"x") {
		t.Error("wrong password accepted")
	}
	if VerifyPassword(h, "") {
		t.Error("empty password accepted")
	}
}

func TestHashIsSalted(t *testing.T) {
	a, err := HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two hashes of the same password are identical; salt is not random")
	}
	if !VerifyPassword(a, "same") || !VerifyPassword(b, "same") {
		t.Error("salted hashes must both still verify")
	}
}

func TestEmptyPasswordRejected(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Error("HashPassword(\"\") should fail")
	}
}

// TestMalformedHashNeverVerifies is the important negative case: a corrupt or
// absent credential must lock everyone out, not let everyone in.
func TestMalformedHashNeverVerifies(t *testing.T) {
	good, err := HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, "$")

	bad := []string{
		"",
		"$",
		"not-a-hash",
		"bcrypt$600000$c2FsdA$a2V5",       // wrong scheme
		"pbkdf2-sha256$0$c2FsdA$a2V5",     // zero iterations
		"pbkdf2-sha256$-1$c2FsdA$a2V5",    // negative iterations
		"pbkdf2-sha256$abc$c2FsdA$a2V5",   // non-numeric iterations
		"pbkdf2-sha256$600000$!!!$a2V5",   // bad base64 salt
		"pbkdf2-sha256$600000$c2FsdA$!!!", // bad base64 key
		"pbkdf2-sha256$600000$$",          // empty salt and key
		strings.Join(parts[:3], "$"),      // truncated
		good + "$extra",                   // trailing field
	}
	for _, h := range bad {
		if VerifyPassword(h, "pw") {
			t.Errorf("malformed hash %q accepted a password", h)
		}
		if VerifyPassword(h, "") {
			t.Errorf("malformed hash %q accepted an empty password", h)
		}
	}
}

func TestTamperedHashFails(t *testing.T) {
	h, err := HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	// Flip a character in the derived key.
	b := []byte(h)
	last := b[len(b)-1]
	if last == 'A' {
		b[len(b)-1] = 'B'
	} else {
		b[len(b)-1] = 'A'
	}
	if VerifyPassword(string(b), "pw") {
		t.Error("tampered hash still verified")
	}
}
