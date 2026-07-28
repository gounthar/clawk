//go:build darwin || linux

package usermode

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGuestMAC(t *testing.T) {
	t.Run("deterministic in id", func(t *testing.T) {
		// Stability is load-bearing: gvproxy's static DHCP lease keys on this
		// MAC, so a value that drifted between boots would move the guest to a
		// dynamic IP and break every port forward.
		require.Equal(t, guestMAC("proj-a"), guestMAC("proj-a"))
	})

	t.Run("distinct per id", func(t *testing.T) {
		seen := map[string]string{}
		for _, id := range []string{"a", "b", "proj", "proj-2", "INFRA-123", "test-proj"} {
			mac := guestMAC(id)
			if prev, dup := seen[mac]; dup {
				t.Fatalf("ids %q and %q collide on %s", prev, id, mac)
			}
			seen[mac] = id
		}
	})

	t.Run("well-formed, locally administered unicast", func(t *testing.T) {
		for _, id := range []string{"", "a", "some-long-sandbox-name"} {
			hw, err := net.ParseMAC(guestMAC(id))
			require.NoErrorf(t, err, "id %q produced %q", id, guestMAC(id))
			require.Len(t, hw, 6)
			require.EqualValues(t, 0, hw[0]&0x1, "multicast bit must be clear (id %q)", id)
			require.EqualValues(t, 0x2, hw[0]&0x2, "locally-administered bit must be set (id %q)", id)
		}
	})

	t.Run("empty id falls back", func(t *testing.T) {
		require.Equal(t, fallbackGuestMAC, guestMAC(""))
	})
}

// The NIC MAC the backend configures and gvproxy's static lease must agree, or
// the guest gets a dynamic address on a different IP.
func TestGvproxyConfigLeaseMatchesGuestMAC(t *testing.T) {
	cfg := Config{ID: "proj-a", SockPath: "/tmp/x"}
	gv := buildGvproxyConfig(cfg)
	require.Equal(t, guestMAC(cfg.ID), gv.DHCPStaticLeases[defaultGuestIP])
}
