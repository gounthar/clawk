package agentembed

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/clawkwork/clawk/internal/revfwd"
	"github.com/clawkwork/clawk/internal/serialfwd"
	"github.com/stretchr/testify/require"
)

// The guest agent builds standalone inside the guest, so it can't import
// internal/revfwd — the reverse-forward protocol is transcribed into
// main.go.in by hand. Nothing else notices when the two drift: a renamed
// JSON field or a moved port compiles fine on both sides and fails only as
// a sandbox where reverse forwards silently never appear.
//
// So: check the parts of the protocol that have to match byte-for-byte
// against the parent-module definition, deriving them from the structs
// rather than restating them, so this test can't drift either.
func TestReverseForwardProtocolMirroredInAgent(t *testing.T) {
	src := string(AgentMainGo)

	require.Contains(t, src, fmt.Sprintf("reverseForwardVSockPort = %d", revfwd.VSockPort),
		"guest dials a different vsock port than the host listens on")
	require.Contains(t, src, fmt.Sprintf("reverseForwardProtoVersion = %d", revfwd.ProtoVersion),
		"guest announces a protocol version the host will hang up on")

	// Op values are string literals on the wire, sent by the guest and
	// switched on by the host.
	for _, op := range []string{revfwd.OpControl, revfwd.OpConnect} {
		require.Contains(t, src, fmt.Sprintf("Op: %q", op),
			"guest never sends the %q greeting", op)
	}

	for _, typ := range []any{
		revfwd.Greeting{}, revfwd.Snapshot{}, revfwd.Forward{}, revfwd.ConnectReply{},
	} {
		rt := reflect.TypeOf(typ)
		for i := range rt.NumField() {
			tag, ok := rt.Field(i).Tag.Lookup("json")
			if !ok {
				continue
			}
			want := fmt.Sprintf("`json:%q`", tag)
			require.Contains(t, src, want,
				"guest is missing the %s.%s field tag %s",
				rt.Name(), rt.Field(i).Name, want)
		}
	}
}

// The guest's own fixed ports must not collide with each other — a
// duplicate would make one service unreachable in a way that looks like
// the host end being down.
func TestGuestVSockPortsAreDistinct(t *testing.T) {
	src := string(AgentMainGo)
	seen := map[string]string{}
	for _, decl := range []struct{ name, port string }{
		{"sshAgentVSockPort", "1026"},
		{"memReportVSockPort", "1027"},
		{"reverseForwardVSockPort", fmt.Sprint(revfwd.VSockPort)},
		{"serialVSockPort", fmt.Sprint(serialfwd.VSockPort)},
	} {
		require.Contains(t, src, decl.name+" = "+decl.port,
			"%s moved; update this test and every mirror of it", decl.name)
		require.NotContains(t, seen, decl.port,
			"port %s claimed by both %s and %s", decl.port, seen[decl.port], decl.name)
		seen[decl.port] = decl.name
	}
}
