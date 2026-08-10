package revfwd

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGreetingRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteLine(&buf, Greeting{Op: OpConnect, V: ProtoVersion, Port: 63342}))

	var got Greeting
	require.NoError(t, ReadLine(NewReader(&buf), &got))
	require.Equal(t, Greeting{Op: OpConnect, V: ProtoVersion, Port: 63342}, got)
}

func TestSnapshotRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := Snapshot{Forwards: []Forward{
		{GuestPort: 63342, HostPort: 63342},
		{GuestPort: 15432, HostPort: 5432},
	}}
	require.NoError(t, WriteLine(&buf, want))

	var got Snapshot
	require.NoError(t, ReadLine(NewReader(&buf), &got))
	require.Equal(t, want, got)
}

// The handshake and the proxied payload share one connection, so the
// reader that read the greeting must be the one that keeps reading — a
// peer that pipelines its first bytes behind the newline would otherwise
// lose them to the discarded buffer.
func TestReadLineLeavesTrailingBytesReadable(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteLine(&buf, ConnectReply{OK: true}))
	buf.WriteString("GET / HTTP/1.1\r\n")

	r := NewReader(&buf)
	var reply ConnectReply
	require.NoError(t, ReadLine(r, &reply))
	require.True(t, reply.OK)

	rest, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, "GET / HTTP/1.1\r\n", string(rest))
}

// A peer that never sends a newline must not make the reader buffer
// without bound.
func TestReadLineRejectsOverlongLine(t *testing.T) {
	r := NewReader(strings.NewReader(strings.Repeat("x", MaxLineBytes+10)))
	var g Greeting
	require.ErrorIs(t, ReadLine(r, &g), ErrLineTooLong)
}

func TestReadLineReportsEOF(t *testing.T) {
	var g Greeting
	require.ErrorIs(t, ReadLine(NewReader(strings.NewReader("")), &g), io.EOF)
}

func TestReadLineRejectsGarbage(t *testing.T) {
	var g Greeting
	err := ReadLine(NewReader(strings.NewReader("not json\n")), &g)
	require.Error(t, err)
	require.False(t, errors.Is(err, io.EOF))
}
