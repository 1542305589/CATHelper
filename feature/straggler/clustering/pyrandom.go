package clustering

import (
	"math"
	"math/bits"
)

// pyRandom reproduces CPython's random.Random (MT19937 + genrand_res53) for a
// single integer seed. The kmeans++ seeding draws from this stream, so matching
// it exactly is what makes Go's cluster assignment — and therefore every
// degradation ratio — numerically identical to the Python skill.
type pyRandom struct {
	mt  [624]uint32
	mti int
}

const (
	mtN        = 624
	mtM        = 397
	mtMatrixA  = 0x9908b0df
	mtUpperMsk = 0x80000000
	mtLowerMsk = 0x7fffffff
)

// newPyRandom mirrors random.Random(seed) for integer seeds.
func newPyRandom(seed int64) *pyRandom {
	r := &pyRandom{}
	key := seedToKey(seed)
	r.initByArray(key)
	return r
}

// seedToKey mirrors CPython's int-seed → 32-bit word array conversion: the
// absolute value's little-endian words, padded to nbits/32+1 entries.
func seedToKey(seed int64) []uint32 {
	v := uint64(seed)
	if seed < 0 {
		v = uint64(-seed)
	}
	if v == 0 {
		return []uint32{0}
	}
	nbits := 64 - bits.LeadingZeros64(v)
	keyused := (nbits + 31) / 32
	key := make([]uint32, keyused)
	for i := range key {
		key[i] = uint32(v >> (32 * uint(i)))
	}
	return key
}

func (r *pyRandom) initGenrand(s uint32) {
	r.mt[0] = s
	for i := 1; i < mtN; i++ {
		r.mt[i] = 1812433253*(r.mt[i-1]^(r.mt[i-1]>>30)) + uint32(i)
	}
	r.mti = mtN
}

// initByArray ports CPython's init_by_array verbatim (uint32 wraparound).
func (r *pyRandom) initByArray(key []uint32) {
	r.initGenrand(19650218)
	i, j := 1, 0
	k := mtN
	if len(key) > k {
		k = len(key)
	}
	for ; k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1664525)) + key[j] + uint32(j)
		i++
		j++
		if i >= mtN {
			r.mt[0] = r.mt[mtN-1]
			i = 1
		}
		if j >= len(key) {
			j = 0
		}
	}
	for k = mtN - 1; k > 0; k-- {
		r.mt[i] = (r.mt[i] ^ ((r.mt[i-1] ^ (r.mt[i-1] >> 30)) * 1566083941)) - uint32(i)
		i++
		if i >= mtN {
			r.mt[0] = r.mt[mtN-1]
			i = 1
		}
	}
	r.mt[0] = 0x80000000
}

// uint32 returns the next MT19937 output word.
func (r *pyRandom) uint32() uint32 {
	if r.mti >= mtN {
		i := 0
		for ; i < mtN-mtM; i++ {
			y := (r.mt[i] & mtUpperMsk) | (r.mt[i+1] & mtLowerMsk)
			r.mt[i] = r.mt[i+mtM] ^ (y >> 1) ^ (mtMatrixA & -(y & 1))
		}
		for ; i < mtN-1; i++ {
			y := (r.mt[i] & mtUpperMsk) | (r.mt[i+1] & mtLowerMsk)
			r.mt[i] = r.mt[i+(mtM-mtN)] ^ (y >> 1) ^ (mtMatrixA & -(y & 1))
		}
		y := (r.mt[mtN-1] & mtUpperMsk) | (r.mt[0] & mtLowerMsk)
		r.mt[mtN-1] = r.mt[mtM-1] ^ (y >> 1) ^ (mtMatrixA & -(y & 1))
		r.mti = 0
	}
	y := r.mt[r.mti]
	r.mti++
	y ^= y >> 11
	y ^= (y << 7) & 0x9d2c5680
	y ^= (y << 15) & 0xefc60000
	y ^= y >> 18
	return y
}

// Float64 mirrors CPython's genrand_res53: two 32-bit draws combined into 53
// random bits, which is what random.random() returns.
func (r *pyRandom) Float64() float64 {
	a := float64(r.uint32() >> 5)
	b := float64(r.uint32() >> 6)
	return (a*67108864.0 + b) * (1.0 / 9007199254740992.0)
}

// pySum mirrors CPython 3.12+'s builtin sum() over floats, which uses Neumaier
// compensated summation. Plain left-to-right addition differs by ~1 ulp on
// fractional data, and that is enough to flip a kmeans assignment when two
// distances tie — so parity requires the same summation.
func pySum(values []float64) float64 {
	var total, c float64
	for _, x := range values {
		t := total + x
		if math.Abs(total) >= math.Abs(x) {
			c += (total - t) + x
		} else {
			c += (x - t) + total
		}
		total = t
	}
	if c != 0 {
		total += c
	}
	return total
}
