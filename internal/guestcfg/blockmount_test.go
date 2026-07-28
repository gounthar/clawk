package guestcfg

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMountBlockAdditive verifies Block/FSType are a clean additive change,
// the same contract TestMountNinePVSockPortAdditive holds for 9p: they
// round-trip, and empty values stay off the wire so a manifest without a
// block mount is byte-identical to the pre-change JSON — no Version bump,
// no forced sandbox recreation.
func TestMountBlockAdditive(t *testing.T) {
	m := Manifest{
		Version: Version,
		Mounts: []Mount{
			{Path: "/workspace/proj", Block: "/dev/vdc", FSType: "ext4"},
			{Tag: "claude_agents", Path: "/home/agent/.claude/agents"}, // virtio-fs only
		},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	if strings.Contains(js, `"block":""`) || strings.Contains(js, `"fstype":""`) {
		t.Errorf("empty block fields not omitted (old inits would see new keys): %s", js)
	}
	if !strings.Contains(js, `"block":"/dev/vdc"`) || !strings.Contains(js, `"fstype":"ext4"`) {
		t.Errorf("block mount missing from wire: %s", js)
	}

	var got Manifest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Mounts[0].Block != "/dev/vdc" || got.Mounts[0].FSType != "ext4" {
		t.Errorf("block mount = %+v, want /dev/vdc ext4", got.Mounts[0])
	}
	if got.Mounts[1].Block != "" {
		t.Errorf("share mount picked up a block device: %+v", got.Mounts[1])
	}
}
