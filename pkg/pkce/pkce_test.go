package pkce

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCodeVerifier(t *testing.T) {
	cv, err := NewCodeVerifier()

	assert.NoError(t, err)
	assert.NotNil(t, cv)

	cco := cv.CodeChallengeOptions()
	assert.Len(t, cco, 2)

	vo := cv.VerifierOptions()
	assert.Len(t, vo, 1)
}
