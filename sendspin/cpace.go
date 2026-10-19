package sendspin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"math/big"

	"golang.org/x/crypto/curve25519"
)

// CPACE-X25519-SHA512 (draft-irtf-cfrg-cpace-21) in initiator-responder mode with explicit mutual
// confirmation: the PAKE the code-based pairing flows run. The light is always the responder (B).

var (
	cpDSI      = []byte("CPace255")
	cpDSIISK   = []byte("CPace255_ISK")
	cpMACLabel = []byte("CPaceMac")
	fieldQ     = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	errCPace   = errors.New("cpace: invalid share")
)

type cpace struct {
	sid, ad, scalar, share []byte
	isk, macKey            []byte
	ya, ada, yb, adb       []byte // transcript: initiator (A) side, responder (B) side
}

func prependLen(b []byte) []byte {
	var out []byte
	n := len(b)
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n > 0 {
			c |= 0x80
		}
		out = append(out, c)
		if n == 0 {
			break
		}
	}
	return append(out, b...)
}

func lvCat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, prependLen(p)...)
	}
	return out
}

func cpaceGenerator(prs, ci, sid []byte) []byte {
	zpad := max(0, sha512.BlockSize-1-len(prependLen(prs))-len(prependLen(cpDSI)))
	h := sha512.Sum512(lvCat(cpDSI, prs, make([]byte, zpad), ci, sid))
	u := make([]byte, 32)
	copy(u, h[:32])
	u[31] &= 0x7f
	return elligator2(leInt(u))
}

func leInt(b []byte) *big.Int {
	be := make([]byte, len(b))
	for i := range b {
		be[len(b)-1-i] = b[i]
	}
	return new(big.Int).SetBytes(be)
}

func leBytes(x *big.Int) []byte {
	be := x.FillBytes(make([]byte, 32))
	out := make([]byte, 32)
	for i := range be {
		out[31-i] = be[i]
	}
	return out
}

// elligator2 maps a field element to a Curve25519 u-coordinate (RFC 9380 map, Z = 2).
func elligator2(r *big.Int) []byte {
	q := fieldQ
	A := big.NewInt(486662)
	mod := func(x *big.Int) *big.Int { return x.Mod(x, q) }
	r = mod(new(big.Int).Set(r))
	den := mod(new(big.Int).Add(big.NewInt(1), new(big.Int).Mul(big.NewInt(2), new(big.Int).Mul(r, r))))
	inv := new(big.Int).Exp(den, new(big.Int).Sub(q, big.NewInt(2)), q) // inv0: 0 -> 0
	v := mod(new(big.Int).Mul(new(big.Int).Neg(A), inv))
	v2 := new(big.Int).Mul(v, v)
	t := mod(new(big.Int).Add(new(big.Int).Add(new(big.Int).Mul(v2, v), new(big.Int).Mul(A, v2)), v))
	eps := new(big.Int).Exp(t, new(big.Int).Rsh(new(big.Int).Sub(q, big.NewInt(1)), 1), q)
	inv2 := new(big.Int).ModInverse(big.NewInt(2), q)
	// x = eps*v - (1-eps)*A/2
	x := new(big.Int).Mul(eps, v)
	x.Sub(x, new(big.Int).Mul(new(big.Int).Mul(new(big.Int).Sub(big.NewInt(1), eps), A), inv2))
	return leBytes(mod(x))
}

// newCPaceResponder starts B's side: samples a scalar and computes Yb.
func newCPaceResponder(prs, sid, ad []byte) (*cpace, error) {
	s := make([]byte, 32)
	if _, err := rand.Read(s); err != nil {
		return nil, err
	}
	return newCPaceWithScalar(prs, sid, ad, s)
}

func newCPaceWithScalar(prs, sid, ad, scalar []byte) (*cpace, error) {
	share, err := curve25519.X25519(scalar, cpaceGenerator(prs, nil, sid))
	if err != nil {
		return nil, errCPace
	}
	return &cpace{sid: sid, ad: ad, scalar: scalar, share: share}, nil
}

// derive takes A's share and associated data and derives ISK and the confirmation key.
func (c *cpace) derive(ya, ada []byte) error {
	if len(ya) != 32 || c.scalar == nil {
		return errCPace
	}
	k, err := curve25519.X25519(c.scalar, ya) // rejects low-order points (all-zero result)
	c.scalar = nil
	if err != nil {
		return errCPace
	}
	c.ya, c.ada, c.yb, c.adb = ya, ada, c.share, c.ad
	transcript := append(lvCat(c.ya, c.ada), lvCat(c.yb, c.adb)...)
	isk := sha512.Sum512(append(lvCat(cpDSIISK, c.sid, k), transcript...))
	c.isk = isk[:]
	mk := sha512.Sum512(append(append(append([]byte{}, cpMACLabel...), c.sid...), c.isk...))
	c.macKey = mk[:]
	return nil
}

func (c *cpace) mac(y, ad []byte) []byte {
	m := hmac.New(sha512.New, c.macKey)
	m.Write(lvCat(y, ad))
	return m.Sum(nil)
}

// tag is B's confirmation tag Tb.
func (c *cpace) tag() []byte { return c.mac(c.yb, c.adb) }

// verify checks A's tag Ta (refusing a reflected transcript).
func (c *cpace) verify(ta []byte) bool {
	if hmac.Equal(c.ya, c.yb) && hmac.Equal(c.ada, c.adb) {
		return false
	}
	return hmac.Equal(ta, c.mac(c.ya, c.ada))
}
