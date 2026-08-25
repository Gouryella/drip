// Package auth turns the token a tunnel client presents at registration into a
// concrete identity (client + account), and hashes admin panel passwords.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// CredentialPrefix marks a token as a Drip client credential. Tokens without
// this prefix are treated as legacy shared-token values.
const CredentialPrefix = "drip"

const (
	// credentialIDBytes is the length of the lookup half of a credential. It is
	// stored in the clear as the primary key of the clients table.
	credentialIDBytes = 8
	// credentialSecretBytes is the length of the secret half. 24 bytes of
	// crypto/rand is 192 bits of entropy.
	credentialSecretBytes = 24
)

var (
	// ErrMalformedCredential means the token is not shaped like drip_<id>_<secret>.
	ErrMalformedCredential = errors.New("malformed credential")
)

// Credential is a parsed client token.
type Credential struct {
	ID     string
	Secret string
}

// String reassembles the credential in wire form.
func (c Credential) String() string {
	return CredentialPrefix + "_" + c.ID + "_" + c.Secret
}

// GenerateCredential mints a new client credential. The returned value is the
// only time the secret exists in plaintext; store HashSecret(cred.Secret).
func GenerateCredential() (Credential, error) {
	idBytes := make([]byte, credentialIDBytes)
	if _, err := rand.Read(idBytes); err != nil {
		return Credential{}, fmt.Errorf("failed to generate credential ID: %w", err)
	}

	secretBytes := make([]byte, credentialSecretBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		return Credential{}, fmt.Errorf("failed to generate credential secret: %w", err)
	}

	return Credential{
		ID:     hex.EncodeToString(idBytes),
		Secret: base64.RawURLEncoding.EncodeToString(secretBytes),
	}, nil
}

// IsCredential reports whether token looks like a Drip client credential.
func IsCredential(token string) bool {
	return strings.HasPrefix(token, CredentialPrefix+"_")
}

// ParseCredential splits a token into its ID and secret halves.
//
// The secret is base64url-encoded and may itself contain '_', so the split is
// bounded to three fields: everything after the second separator is the secret.
func ParseCredential(token string) (Credential, error) {
	parts := strings.SplitN(token, "_", 3)
	if len(parts) != 3 || parts[0] != CredentialPrefix {
		return Credential{}, ErrMalformedCredential
	}

	id, secret := parts[1], parts[2]
	if len(id) != credentialIDBytes*2 || secret == "" {
		return Credential{}, ErrMalformedCredential
	}
	if _, err := hex.DecodeString(id); err != nil {
		return Credential{}, ErrMalformedCredential
	}

	return Credential{ID: id, Secret: secret}, nil
}

// HashSecret returns the hex-encoded SHA-256 of a credential secret.
//
// A fast hash is correct here, unlike for human passwords: the secret is 192
// bits of crypto/rand output, so an attacker holding the database gains nothing
// from a brute-force search regardless of the KDF cost. Using SHA-256 keeps
// registration cheap under reconnect storms. Admin passwords are low-entropy
// human input and use Argon2id instead - see password.go.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// VerifySecret compares a presented secret against a stored hash in constant time.
func VerifySecret(secret, storedHash string) bool {
	computed := HashSecret(secret)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(storedHash)) == 1
}
