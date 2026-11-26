package common

import "testing"

func TestShortenK3dName(t *testing.T) {
	short := "kbrun-1-vinodh-node0"
	if got := shortenK3dName(short); got != short {
		t.Errorf("a name that fits must not change: %q", got)
	}
	long := "kbrun-1-vinodh-krishnaswamy-node0"
	got := shortenK3dName(long)
	if len(got) > K3dNameLimit {
		t.Errorf("%q is %d chars, limit %d", got, len(got), K3dNameLimit)
	}
	if got != shortenK3dName(long) {
		t.Errorf("shortening must be stable")
	}
	if other := shortenK3dName("kbrun-1-vinodh-krishnaswamy-node1"); other == got {
		t.Errorf("node0 and node1 collide: %q", got)
	}
	t.Logf("%s -> %s", long, got)
}
