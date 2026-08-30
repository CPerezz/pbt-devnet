package migmon

import "testing"

func TestSpecFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		node string
		want bool
	}{
		{"geth by full service name", "el-1-geth-lighthouse", true},
		{"bare key", "geth", true},
		{"unknown client", "el-2-nethermind-teku", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, ok := SpecFor(tc.node)
			if ok != tc.want {
				t.Fatalf("SpecFor(%q) ok = %v, want %v", tc.node, ok, tc.want)
			}
			if ok && spec != Registry["geth"] {
				t.Fatalf("SpecFor(%q) = %+v, want the geth entry", tc.node, spec)
			}
		})
	}
}
