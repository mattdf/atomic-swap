package net

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHost_Initiate(t *testing.T) {
	ha := newHost(t, defaultPort)
	err := ha.Start()
	require.NoError(t, err)
	hb := newHost(t, defaultPort+1)
	err = hb.Start()
	require.NoError(t, err)

	defer func() {
		_ = ha.Stop()
		_ = hb.Stop()
	}()

	err = ha.h.Connect(ha.ctx, hb.addrInfo())
	require.NoError(t, err)

	err = ha.Initiate(hb.addrInfo(), &SendKeysMessage{}, nil)
	require.NoError(t, err)
	time.Sleep(time.Millisecond * 500)
	require.NotNil(t, ha.swapStream)
	require.NotNil(t, hb.swapStream)
	require.NotNil(t, ha.swapState)
	require.NotNil(t, hb.swapState)
}
