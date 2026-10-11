package bridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The router empties a bridge that failed to start (*br = Bridge{}), but
// goroutines the bridge already started may still read its config. That used
// to panic and crash the whole process.
func TestDisabledBridgeConfigGetters(t *testing.T) {
	b := &Bridge{}
	assert.NotPanics(t, func() {
		assert.False(t, b.GetBool("ShowUserTyping"))
		assert.Equal(t, 0, b.GetInt("x"))
		assert.Equal(t, "", b.GetString("x"))
		assert.Nil(t, b.GetStringSlice("x"))
		assert.Nil(t, b.GetStringSlice2D("x"))
		assert.False(t, b.IsKeySet("x"))
	})
}
