package sendspin

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Vectors from the Python cpace package aiosendspin uses (itself checked against the draft's test vectors),
// with fixed scalars for both sides.
func TestCPaceMatchesReference(t *testing.T) {
	prs := unhex(t, "313233343536")
	sid := unhex(t, "73656e647370696e2d706169722d70616b652d7631000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f0000000100000001")
	sb := unhex(t, "01080f161d242b323940474e555c636a71787f868d949ba2a9b0b7bec5ccd3da")
	ya := unhex(t, "e90201fa97bcacdf584717a73acaf0fc662ecf9389a3ed6a82493c5224568e57")

	if g := cpaceGenerator(prs, nil, sid); !bytes.Equal(g, unhex(t, "cc3258bda4601bb47d2f04808245e424798572a90d16fee9381394b9f9a42028")) {
		t.Fatalf("generator %x", g)
	}
	c, err := newCPaceWithScalar(prs, sid, []byte("client"), sb)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.share, unhex(t, "3b8331692f2e4b9edafc031865f010f6dd417002a7f28969003442a3246e4718")) {
		t.Fatalf("Yb %x", c.share)
	}
	if err := c.derive(ya, []byte("server")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.isk, unhex(t, "f839a347f322b1c15965656bd8fcee441516ca59b2fe8845978db1d8dd354270ef55e05edc7c89c87d1abce25212fa4b95df27421d2a93950fff80f7f55243d1")) {
		t.Fatalf("ISK %x", c.isk)
	}
	if !c.verify(unhex(t, "d6c986182c420c9c8962172fe579b96c232f19cf3e6b22034d176178a8f7d9def5891495bae669a06d240bc7e5484e9b2d194fdd1bb0afc1455d3364d6aa7cd4")) {
		t.Fatal("Ta did not verify")
	}
	if !bytes.Equal(c.tag(), unhex(t, "8cf4f3a7365c17d1a5dbd8173256385804e3b4d25f24b1070675c10d7b4ca5927bb4b3e96428342dd933a9d27b61e22274852e5dea35d8d023697178f4e75cfb")) {
		t.Fatalf("Tb %x", c.tag())
	}
	if c.verify(make([]byte, 64)) {
		t.Fatal("a wrong Ta verified")
	}
}

func TestCPaceRejectsLowOrderShare(t *testing.T) {
	c, err := newCPaceResponder([]byte("123456"), []byte("sid"), []byte("client"))
	if err != nil {
		t.Fatal(err)
	}
	if c.derive(make([]byte, 32), []byte("server")) == nil {
		t.Fatal("all-zero share accepted")
	}
}
