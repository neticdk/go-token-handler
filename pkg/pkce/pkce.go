package pkce

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"

	"golang.org/x/oauth2"
)

const (
	ParamCodeVerifier        = "code_verifier"
	ParamCodeChallenge       = "code_challenge"
	ParamCodeChallengeMethod = "code_challenge_method"
)

type ChallengeMethod string

const (
	ChallengeMethodPlain ChallengeMethod = "plain"
	ChallengeMethodS256  ChallengeMethod = "S256"
)

type CodeVerifier struct {
	Verifier string
	Method   ChallengeMethod
}

func NewCodeVerifier() (*CodeVerifier, error) {
	v, err := randomBytesInHex(32)
	if err != nil {
		return nil, err
	}
	return &CodeVerifier{
		Verifier: v,
		Method:   ChallengeMethodS256,
	}, nil
}

func (c *CodeVerifier) VerifierOptions() []oauth2.AuthCodeOption {
	return []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam(ParamCodeVerifier, c.Verifier),
	}
}

func (c *CodeVerifier) CodeChallengeOptions() []oauth2.AuthCodeOption {
	challenge := c.Verifier
	if c.Method == ChallengeMethodS256 {
		s := sha256.Sum256([]byte(challenge))
		challenge = base64.RawURLEncoding.EncodeToString(s[:])
	}
	return []oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam(ParamCodeChallenge, challenge),
		oauth2.SetAuthURLParam(ParamCodeChallengeMethod, string(c.Method)),
	}
}

func randomBytesInHex(bytes int) (string, error) {
	buf := make([]byte, bytes)
	_, err := io.ReadFull(rand.Reader, buf)
	if err != nil {
		return "", fmt.Errorf("could not generate %d random bytes: %v", bytes, err)
	}
	return hex.EncodeToString(buf), nil
}
