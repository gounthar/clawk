package cli

import (
	"encoding/json"
	"testing"

	"github.com/clawkwork/clawk/internal/config"
	"github.com/stretchr/testify/require"
)

func TestForwardAddReverse(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "rev", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
	}))

	out, err := executeCommand("forward", "add-reverse", "rev", "63342", "5432:15432")
	require.NoError(t, err)
	// The direction is the whole point of the verb, so it has to be in the
	// output — a bare "63342:63342" wouldn't say which end listens.
	require.Contains(t, out, "guest 127.0.0.1:63342 → host 127.0.0.1:63342")
	require.Contains(t, out, "guest 127.0.0.1:15432 → host 127.0.0.1:5432")
	// Nothing is running under the mock provider, so the live push reports
	// that instead of claiming an apply that never happened.
	require.Contains(t, out, "applies on next 'clawk up'")

	sb, err := s.Load("rev")
	require.NoError(t, err)
	require.Equal(t, []config.PortForward{
		{HostPort: 63342, GuestPort: 63342},
		{HostPort: 5432, GuestPort: 15432},
	}, sb.ReverseForwards)
	require.Empty(t, sb.Forwards, "reverse forwards must not leak into the outbound list")
}

func TestForwardAddReverseIsIdempotent(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "rev", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
	}))

	_, err := executeCommand("forward", "add-reverse", "rev", "63342")
	require.NoError(t, err)
	out, err := executeCommand("forward", "add-reverse", "rev", "63342")
	require.NoError(t, err)
	require.Contains(t, out, "already reverse-forwarded")

	sb, err := s.Load("rev")
	require.NoError(t, err)
	require.Len(t, sb.ReverseForwards, 1)
}

// Only one thing can bind a given guest port, so a second mapping onto it
// has to be refused rather than silently losing inside the guest.
func TestForwardAddReverseRejectsGuestPortClash(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "rev", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
	}))

	_, err := executeCommand("forward", "add-reverse", "rev", "5432:15432")
	require.NoError(t, err)
	_, err = executeCommand("forward", "add-reverse", "rev", "6432:15432")
	require.Error(t, err)
	require.Contains(t, err.Error(), "guest port 15432 is already reverse-forwarded")

	sb, err := s.Load("rev")
	require.NoError(t, err)
	require.Len(t, sb.ReverseForwards, 1)
}

func TestForwardRemoveReverse(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "rev", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
		ReverseForwards: []config.PortForward{
			{HostPort: 63342, GuestPort: 63342},
			{HostPort: 5432, GuestPort: 15432},
		},
	}))

	out, err := executeCommand("forward", "remove-reverse", "rev", "63342")
	require.NoError(t, err)
	require.Contains(t, out, "Reverse forward removed")

	sb, err := s.Load("rev")
	require.NoError(t, err)
	require.Equal(t, []config.PortForward{{HostPort: 5432, GuestPort: 15432}}, sb.ReverseForwards)
}

// Both directions ride the same record, and `forward list --json` is the
// scriptable view of it — they must stay distinguishable there.
func TestForwardListJSONSeparatesDirections(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "rev", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
		Forwards:        []config.PortForward{{HostPort: 3000, GuestPort: 3000}},
		ReverseForwards: []config.PortForward{{HostPort: 5432, GuestPort: 15432}},
	}))

	out, err := executeCommand("forward", "list", "rev", "--json")
	require.NoError(t, err)
	var got struct {
		Forwards        []statusJSONForward `json:"forwards"`
		ReverseForwards []statusJSONForward `json:"reverse_forwards"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), "not JSON\n%s", out)
	require.Equal(t, []statusJSONForward{{HostPort: 3000, GuestPort: 3000}}, got.Forwards)
	require.Equal(t, []statusJSONForward{{HostPort: 5432, GuestPort: 15432}}, got.ReverseForwards)
}

func TestStatusShowsReverseForwards(t *testing.T) {
	s, _ := setupTest(t)
	require.NoError(t, s.Save(&config.Sandbox{
		Name: "rev", Provider: config.ProviderVZ, VMState: config.VMStateStopped,
		ReverseForwards: []config.PortForward{{HostPort: 5432, GuestPort: 15432}},
	}))

	out, err := executeCommand("status", "rev")
	require.NoError(t, err)
	require.Contains(t, out, "Reverse")
	require.Contains(t, out, "15432 → 5432")
}
