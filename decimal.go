// Package decimal is a high-performance, arbitrary precision, fixed-point
// decimal library.
package decimal

import (
	"bytes"
	"errors"
	"math"
	"math/big"
	"runtime"
	"strconv"
	"strings"

	"github.com/EricLagergren/decimal/internal/arith"
	"github.com/EricLagergren/decimal/internal/arith/checked"
	"github.com/EricLagergren/decimal/internal/arith/pow"
	"github.com/EricLagergren/decimal/internal/c"
)

// Note: For +/-inf/nan checks: https://play.golang.org/p/RtH3UCt5IH

// Big represents a fixed-point, multi-precision
// decimal number.
//
// A Big decimal is an arbitrary-precision number and a
// scale; the latter representing the number of digits to the
// right of the radix.
//
// A negative scale indicates the lack of a radix (typically a
// very large number).
type Big struct {
	// If |v| <= 1 << 64 - 1 the mantissa will be stored in this field.
	compact int64
	scale   int32
	ctx     Context
	form    form // zero, finite, or inf.

	// If the mantissa is not stored in the compact field, it's held here.
	mantissa big.Int
}

// form represents whether the Big decimal is normal, infinite, or
// NaN.
type form byte

// Do not change these constants -- their order is important.
const (
	zero = iota
	finite
	inf
)

// An ErrNaN panic is raised by a Decimal operation that would lead to a NaN
// under IEEE-754 rules. An ErrNaN implements the error interface.
type ErrNaN struct {
	msg string
}

func (e ErrNaN) Error() string {
	return e.msg
}

func (x *Big) isInflated() bool {
	return x.compact == c.Inflated
}

func (x *Big) isCompact() bool {
	return x.compact != c.Inflated
}

func (x *Big) isEvenInt() bool {
	return x.IsInt() &&
		(x.isCompact() && x.compact&1 == 0) ||
		(x.isInflated() && x.mantissa.And(&x.mantissa, oneInt).Sign() == 0)
}

// New creates a new Big decimal with the given value and scale.
func New(value int64, scale int32) *Big {
	return new(Big).SetMantScale(value, scale)
}

// Abs sets z to the absolute value of x if x is finite and returns z.
func (z *Big) Abs(x *Big) *Big {
	if x.form != finite {
		return z
	}
	if x.isCompact() {
		z.compact = arith.Abs(x.compact)
	} else {
		z.mantissa.Abs(&x.mantissa)
	}
	z.scale = x.scale
	z.ctx = x.ctx
	z.form = finite
	return z
}

// Add sets z to x + y and returns z.
func (z *Big) Add(x, y *Big) *Big {
	if x.form == finite && y.form == finite {
		z.form = finite
		if x.isCompact() {
			if y.isCompact() {
				return z.addCompact(x, y)
			}
			return z.addHalf(x, y)
		}
		if y.isCompact() {
			return z.addHalf(y, x)
		}
		return z.addBig(x, y)
	}

	if x.form == inf && y.form == inf &&
		x.SignBit() != y.SignBit() {
		// +Inf + -Inf
		// -Inf + +Inf
		z.form = zero
		panic(ErrNaN{"addition of infinities with opposing signs"})
	}

	if x.form == zero && y.form == zero {
		// ±0 + ±0
		z.form = zero
		return z
	}

	if x.form == inf || y.form == zero {
		// ±Inf + y
		// x + ±0
		return z.Set(x)
	}

	// ±0 + y
	// x + ±Inf
	return z.Set(y)
}

// addCompact sets z to x + y and returns z.
func (z *Big) addCompact(x, y *Big) *Big {
	// Fast path: if the scales are the same we can just add
	// without adjusting either number.
	if x.scale == y.scale {
		z.scale = x.scale
		sum, ok := checked.Add(x.compact, y.compact)
		if ok {
			z.compact = sum
			if sum == 0 {
				z.form = zero
			}
		} else {
			z.mantissa.Add(big.NewInt(x.compact), big.NewInt(y.compact))
			z.compact = c.Inflated
			if z.mantissa.Sign() == 0 {
				z.form = zero
			}
		}
		return z
	}

	// Guess the scales. We need to inflate lo.
	hi, lo := x, y
	if hi.scale < lo.scale {
		hi, lo = lo, hi
	}

	// Power of 10 we need to multiply our lo value by in order
	// to equalize the scales.
	inc := hi.scale - lo.scale
	z.scale = hi.scale

	scaledLo, ok := checked.MulPow10(lo.compact, inc)
	if ok {
		sum, ok := checked.Add(hi.compact, scaledLo)
		if ok {
			z.compact = sum
			return z
		}
	}
	scaled := checked.MulBigPow10(big.NewInt(lo.compact), inc)
	z.mantissa.Add(scaled, big.NewInt(hi.compact))
	z.compact = c.Inflated
	if z.mantissa.Sign() == 0 {
		z.form = zero
	}
	return z
}

// addHalf adds a compact Big with a non-compact Big.
// addHalf will panic if the first argument is not compact.
func (z *Big) addHalf(comp, non *Big) *Big {
	if comp.isInflated() {
		panic("decimal.Add (bug) comp.isInflated() == true")
	}
	if comp.scale == non.scale {
		z.mantissa.Add(big.NewInt(comp.compact), &non.mantissa)
		z.scale = comp.scale
		z.compact = c.Inflated
		if z.mantissa.Sign() == 0 {
			z.form = zero
		}
		return z
	}
	// Since we have to rescale we need to add two big.Ints
	// together because big.Int doesn't have an API for
	// increasing its value by an integer.
	return z.addBig(&Big{
		mantissa: *big.NewInt(comp.compact),
		scale:    comp.scale,
	}, non)
}

func (z *Big) addBig(x, y *Big) *Big {
	hi, lo := x, y
	if hi.scale < lo.scale {
		hi, lo = lo, hi
	}

	inc := hi.scale - lo.scale
	scaled := checked.MulBigPow10(&lo.mantissa, inc)
	z.mantissa.Add(&hi.mantissa, scaled)
	z.compact = c.Inflated
	z.scale = hi.scale
	if z.mantissa.Sign() == 0 {
		z.form = zero
	}
	return z
}

// log2(10)
const ln210 = 3.321928094887362347870319429489390175864831393024580612054

// BitLen returns the absolute value of x in bits.
func (x *Big) BitLen() int {
	// If using an artificially inflated number determine the
	// bitlen using the number of digits.
	//
	// http://www.exploringbinary.com/number-of-bits-in-a-decimal-integer/
	if x.scale < 0 {
		// Number of zeros in scale + digits in z.
		d := -int(x.scale) + x.Prec()
		return int(math.Ceil(float64(d-1) * ln210))
	}
	if x.isCompact() {
		return arith.BitLen(x.compact)
	}
	return x.mantissa.BitLen()
}

// Cmp compares d and x and returns:
//
//   -1 if z <  x
//    0 if z == x
//   +1 if z >  x
//
// It does not modify d or x.
func (z *Big) Cmp(x *Big) int {
	// Check for same pointers.
	if z == x {
		return 0
	}

	// Same scales means we can compare straight across.
	if z.scale == x.scale {
		if z.isCompact() && x.isCompact() {
			if z.compact > x.compact {
				return +1
			}
			if z.compact < x.compact {
				return -1
			}
			return 0
		}
		if z.isInflated() && x.isInflated() {
			if z.mantissa.Sign() != x.mantissa.Sign() {
				return z.mantissa.Sign()
			}

			if z.scale < 0 {
				return z.mantissa.Cmp(&x.mantissa)
			}

			zb := z.mantissa.Bits()
			xb := x.mantissa.Bits()

			min := len(zb)
			if len(xb) < len(zb) {
				min = len(xb)
			}
			i := 0
			for i < min-1 && zb[i] == xb[i] {
				i++
			}
			if zb[i] > xb[i] {
				return +1
			}
			if zb[i] < xb[i] {
				return -1
			}
			return 0
		}
	}

	// Different scales -- check signs and/or if they're
	// both zero.

	ds := z.Sign()
	xs := x.Sign()
	switch {
	case ds > xs:
		return +1
	case ds < xs:
		return -1
	case ds == 0 && xs == 0:
		return 0
	}

	// Scales aren't equal, the signs are the same, and both
	// are non-zero.
	dl := int32(z.Prec()) - z.scale
	xl := int32(x.Prec()) - x.scale
	if dl > xl {
		return +1
	}
	if dl < xl {
		return -1
	}

	// We need to inflate one of the numbers.

	dc := z.compact // hi
	xc := x.compact // lo

	var swap bool

	hi, lo := z, x
	if hi.scale < lo.scale {
		hi, lo = lo, hi
		dc, xc = xc, dc
		swap = true // d is lo
	}

	diff := hi.scale - lo.scale
	if diff <= c.BadScale {
		var ok bool
		xc, ok = checked.MulPow10(xc, diff)
		if !ok && dc == c.Inflated {
			// d is lo
			if swap {
				zm := new(big.Int).Set(&z.mantissa)
				return checked.MulBigPow10(zm, diff).Cmp(&x.mantissa)
			}
			// x is lo
			xm := new(big.Int).Set(&x.mantissa)
			return z.mantissa.Cmp(checked.MulBigPow10(xm, diff))
		}
	}

	if swap {
		dc, xc = xc, dc
	}

	if dc != c.Inflated {
		if xc != c.Inflated {
			return arith.AbsCmp(dc, xc)
		}
		return big.NewInt(dc).Cmp(&x.mantissa)
	}
	if xc != c.Inflated {
		return z.mantissa.Cmp(big.NewInt(xc))
	}
	return z.mantissa.Cmp(&x.mantissa)
}

// Context returns x's Context.
func (x *Big) Context() Context {
	return x.ctx
}

// Exp sets z to e ** x and returns z.
/*func (z *Big) Exp(x *Big) *Big {
	if x.form == zero {
		// e ** 0 == 1
		return z.SetMantScale(1, 0)
	}
	if x.SignBit() {
		// 1 / (e ** -x)
		return z.Quo(one, z.Exp(z.Neg(x)))
	}
	intg, frac := new(Big).Modf(x)
	if intg.form == zero {
		return frac.taylor(x)
	}

	// n = e ** (1 + frac / integ)
	n := new(Big).taylor(z.Add(z.Quo(frac, intg), one))

	r := New(1, 0)
	zp := z.ctx.prec()
	var tmp Big
	for intg.Cmp(max64) >= 0 {
		r.Mul(r, tmp.powInt(n, math.MaxInt64)).Round(zp)
		frac.Sub(frac, max64)
	}
	return z.Mul(r, n.powInt(n, intg.compact)).Round(zp)
}*/

// Format implements the fmt.Formatter interface.
// func (z *Big) Format(s fmt.State, r rune) {
// 	switch r {
// 	case 'e', 'g', 's', 'f':
// 		s.Write([]byte(z.String()))
// 	case 'E':
// 		s.Write([]byte(z.toString(true, upper)))
// 	default:
// 		fmt.Fprint(s, *z)
// 	}
// }

// IsBig returns true if x, with its fractional part truncated, cannot fit
// inside an int64.
func (x *Big) IsBig() bool {
	return (x.isCompact() && (x.scale < -19 || x.scale > 19)) ||
		(x.mantissa.Cmp(c.MinInt64) < 0 || x.mantissa.Cmp(c.MaxInt64) >= 0)
}

// Int returns x as a big.Int, truncating the fractional portion, if any.
func (x *Big) Int() *big.Int {
	var b big.Int
	if x.isCompact() {
		b.SetInt64(x.compact)
	} else {
		b.Set(&x.mantissa)
	}
	if x.scale == 0 {
		return &b
	}
	if x.scale < 0 {
		return checked.MulBigPow10(&b, -x.scale)
	}
	p := pow.BigTen(int64(x.scale))
	return b.Div(&b, &p)
}

// Int64 returns x as an int64, truncating the fractional portion, if any.
func (x *Big) Int64() int64 {
	var b int64
	if x.isCompact() {
		b = x.compact
	} else {
		b = x.mantissa.Int64()
	}
	if x.scale == 0 {
		return b
	}
	if x.scale < 0 {
		// Undefined. checked.MulPow10 returns 0 when ok is false.
		// IMO, 0 is a better choice than 1 << 64 - 1 because it could cause a
		// division by zero panic which would be a clear indication something is
		// incorrect.
		b, _ = checked.MulPow10(b, -x.scale)
		return b
	}
	p, ok := pow.Ten64(int64(x.scale))
	// See above comment.
	if !ok {
		return 0
	}
	return b / p
}

// IsFinite returns true if x is finite.
func (x *Big) IsFinite() bool {
	return x.form == finite
}

// IsInf returns true if x is an infinity.
func (x *Big) IsInf() bool {
	return x.form == inf
}

// IsInt reports whether x is an integer.
// ±Inf values are not integers.
func (x *Big) IsInt() bool {
	if x.form != finite {
		return x.form == zero
	}
	// Prec doesn't count trailing zeros,
	// so precision <= scale means scale is all
	// trailing zeros.
	// E.g., 12.000:    scale == 3, prec == 2
	//       1234.0000: scale == 4, prec == 4
	return x.scale <= 0 || x.Prec() < int(x.scale)
}

// Log sets z to the base-e logarithm of x and returns z.
/*func (z *Big) Log(x *Big) *Big {
	if x.ltez() {
		panic(ErrNaN{"base-e logarithm of x <= 0"})
	}
	if x.form == inf {
		z.form = inf
		return z
	}
	mag := int64(x.Prec() - int(x.scale) - 1)
	if mag < 3 {
		return z.logNewton(x)
	}
	root := z.integralRoot(x, mag)
	lnRoot := root.logNewton(root)
	return z.Mul(New(mag, 0), lnRoot)
}*/

// MarshalText implements encoding/TextMarshaler.
func (x *Big) MarshalText() ([]byte, error) {
	// TODO: Don't convert to []byte.
	return []byte(x.String()), nil
}

// Mode returns the rounding mode of x.
func (x *Big) Mode() RoundingMode {
	return x.ctx.mode
}

// Mul sets z to x * y and returns z.
func (z *Big) Mul(x, y *Big) *Big {
	if x.form == finite && y.form == finite {
		z.form = finite
		if x.isCompact() {
			if y.isCompact() {
				return z.mulCompact(x, y)
			}
			return z.mulHalf(x, y)
		}
		if y.isCompact() {
			return z.mulHalf(y, x)
		}
		return z.mulBig(x, y)
	}

	if x.form == zero && y.form == inf || x.form == inf && y.form == zero {
		// ±0 * ±Inf
		// ±Inf * ±0
		z.form = zero
		panic(ErrNaN{"multiplication of zero with infinity"})
	}

	if x.form == inf || y.form == inf {
		// ±Inf * y
		// x * ±Inf
		z.form = inf
		return z
	}

	// ±0 * y
	// x * ±0
	z.form = zero
	return z
}

func (z *Big) mulCompact(x, y *Big) *Big {
	scale, ok := checked.Add32(x.scale, y.scale)
	if !ok {
		z.form = inf
		return z
	}

	prod, ok := checked.Mul(x.compact, y.compact)
	if ok {
		z.compact = prod
	} else {
		z.mantissa.Mul(big.NewInt(x.compact), big.NewInt(y.compact))
		z.compact = c.Inflated
	}
	z.scale = scale
	z.form = finite
	return z
}

func (z *Big) mulHalf(comp, non *Big) *Big {
	if comp.isInflated() {
		panic("decimal.Mul (bug) comp.isInflated() == true")
	}
	if comp.scale == non.scale {
		scale, ok := checked.Add32(comp.scale, non.scale)
		if !ok {
			z.form = inf
			return z
		}
		z.mantissa.Mul(big.NewInt(comp.compact), &non.mantissa)
		z.compact = c.Inflated
		z.scale = scale
		z.form = finite
		return z
	}
	return z.mulBig(&Big{
		mantissa: *big.NewInt(comp.compact),
		scale:    comp.scale,
	}, non)
}

func (z *Big) mulBig(x, y *Big) *Big {
	scale, ok := checked.Add32(x.scale, y.scale)
	if !ok {
		z.form = inf
		return z
	}
	z.mantissa.Mul(&x.mantissa, &y.mantissa)
	z.compact = c.Inflated
	z.scale = scale
	z.form = finite
	return z
}

// Neg sets z to -x and returns z.
func (z *Big) Neg(x *Big) *Big {
	if x.isCompact() {
		z.compact = -x.compact
	} else {
		z.mantissa.Neg(&x.mantissa)
		z.compact = c.Inflated
	}
	z.scale = x.scale
	z.form = x.form
	return z
}

// Prec returns the precision of z. That is, it returns the number of
// decimal digits z requires.
func (x *Big) Prec() int {
	// 0 and Inf.
	if x.form != finite {
		return 0
	}
	if x.isCompact() {
		return arith.Length(x.compact)
	}
	return arith.BigLength(&x.mantissa)
}

// Quo sets z to x / y and returns z.
func (z *Big) Quo(x, y *Big) *Big {
	if x.form == finite && y.form == finite {
		z.form = finite
		// x / y (common case)
		if x.isCompact() {
			if y.isCompact() {
				return z.quoCompact(x, y)
			}
			return z.quoBig(&Big{
				compact:  c.Inflated,
				mantissa: *big.NewInt(x.compact),
				ctx:      x.ctx,
				form:     x.form,
				scale:    x.scale,
			}, y)
		}
		if y.isCompact() {
			return z.quoBig(x, &Big{
				compact:  c.Inflated,
				mantissa: *big.NewInt(y.compact),
				ctx:      y.ctx,
				form:     y.form,
				scale:    y.scale,
			})
		}
		return z.quoBig(x, y)
	}

	if x.form == zero && y.form == zero || x.form == inf && y.form == inf {
		// ±0 / ±0
		// ±Inf / ±Inf
		z.form = zero
		panic(ErrNaN{"division of zero by zero or infinity by infinity"})
	}

	if x.form == zero || y.form == inf {
		// ±0 / y
		// x / ±Inf
		z.form = zero
		return z
	}

	// x / ±0
	// ±Inf / y
	z.form = inf
	return z
}

func (z *Big) quoAndRound(x, y int64) *Big {
	// Quotient
	z.compact = x / y

	// ToZero means we can ignore remainder.
	if z.ctx.mode == ToZero {
		return z
	}

	// Remainder
	r := x % y

	sign := int64(1)
	if (x < 0) != (y < 0) {
		sign = -1
	}
	if r != 0 && z.needsInc(y, r, sign > 0, z.compact&1 != 0) {
		z.compact += sign
	}
	return z.Round(z.ctx.prec())
}

func (z *Big) quoCompact(x, y *Big) *Big {
	if x.compact == 0 {
		if y.compact == 0 {
			panic(ErrNaN{"division of zero by zero"})
		}
		z.form = 0
		return z
	}

	scale, ok := checked.Sub32(x.scale, y.scale)
	if !ok {
		z.form = inf
		return z
	}

	zp := z.ctx.prec()
	xp := int32(x.Prec())
	yp := int32(y.Prec())

	// Multiply y by 10 if x' > y'
	if cmpNorm(x.compact, xp, y.compact, yp) {
		yp--
	}

	scale, ok = checked.Int32(int64(scale) + int64(yp) - int64(xp) + int64(zp))
	if !ok {
		z.form = inf
		return z
	}
	z.scale = scale

	shift, ok := checked.SumSub(zp, yp, xp)
	if !ok {
		z.form = inf
		return z
	}

	xs, ys := x.compact, y.compact
	if shift > 0 {
		xs, ok = checked.MulPow10(x.compact, shift)
		if !ok {
			x0 := checked.MulBigPow10(big.NewInt(x.compact), shift)
			return z.quoBigAndRound(x0, big.NewInt(y.compact))
		}
		return z.quoAndRound(xs, ys)
	}

	// shift < 0
	ns, ok := checked.Sub32(xp, zp)
	if !ok {
		z.form = inf
		return z
	}

	// new scale == yp, so no inflation needed.
	if ns == yp {
		return z.quoAndRound(xs, ys)
	}
	shift, ok = checked.Sub32(ns, yp)
	if !ok {
		z.form = inf
		return z
	}
	ys, ok = checked.MulPow10(ys, shift)
	if !ok {
		y0 := checked.MulBigPow10(big.NewInt(y.compact), shift)
		return z.quoBigAndRound(big.NewInt(x.compact), y0)
	}
	return z.quoAndRound(xs, ys)
}

func (z *Big) quoBig(x, y *Big) *Big {
	scale, ok := checked.Sub32(x.scale, y.scale)
	if !ok {
		z.form = inf
		return z
	}

	zp := z.ctx.prec()
	xp := int32(x.Prec())
	yp := int32(y.Prec())

	// Multiply y by 10 if x' > y'
	if cmpNormBig(&x.mantissa, xp, &y.mantissa, yp) {
		yp--
	}

	scale, ok = checked.Int32(int64(scale) + int64(yp) - int64(xp) + int64(zp))
	if !ok {
		z.form = inf
		return z
	}
	z.scale = scale

	shift, ok := checked.SumSub(zp, yp, xp)
	if !ok {
		z.form = inf
		return z
	}
	if shift > 0 {
		xs := checked.MulBigPow10(new(big.Int).Set(&x.mantissa), shift)
		return z.quoBigAndRound(xs, &y.mantissa)
	}

	// shift < 0
	ns, ok := checked.Sub32(xp, zp)
	if !ok {
		z.form = inf
		return z
	}
	shift, ok = checked.Sub32(ns, yp)
	if !ok {
		z.form = inf
		return z
	}
	ys := checked.MulBigPow10(new(big.Int).Set(&y.mantissa), shift)
	return z.quoBigAndRound(&x.mantissa, ys)
}

func (z *Big) quoBigAndRound(x, y *big.Int) *Big {
	z.compact = c.Inflated

	q, r := z.mantissa.QuoRem(x, y, new(big.Int))

	if z.ctx.mode == ToZero {
		return z
	}

	sign := int64(1)
	if (x.Sign() < 0) != (y.Sign() < 0) {
		sign = -1
	}
	odd := new(big.Int).And(q, oneInt).Sign() != 0

	if r.Sign() != 0 && z.needsIncBig(y, r, sign > 0, odd) {
		z.mantissa.Add(&z.mantissa, big.NewInt(sign))
	}
	return z.Round(z.ctx.prec())
}

// Round rounds z down to n digits of precision and returns z. The result is
// undefined if n is less than zero. No rounding will occur if n is zero.
// The result of Round will always be within the interval [⌊z⌋, z].
func (z *Big) Round(n int32) *Big {
	zp := z.Prec()
	if n <= 0 || int(n) < zp-int(z.scale) || z.form != finite {
		return z
	}

	shift, ok := checked.Sub(int64(zp), int64(n))
	if !ok {
		z.form = inf
		return z
	}
	if shift <= 0 {
		return z
	}
	z.scale -= int32(shift)

	if z.isCompact() {
		val, ok := pow.Ten64(shift)
		if ok {
			return z.quoAndRound(z.compact, val)
		}
		z.mantissa.SetInt64(z.compact)
	}
	val := pow.BigTen(shift)
	return z.quoBigAndRound(&z.mantissa, &val)
}

// Scale returns x's scale.
func (x *Big) Scale() int32 {
	return x.scale
}

// Set sets z to x and returns z.
func (z *Big) Set(x *Big) *Big {
	if z != x {
		z.compact = x.compact
		z.ctx = x.ctx
		z.form = x.form
		z.scale = x.scale

		// Copy over mantissa if need be.
		if x.isInflated() {
			z.mantissa.Set(&x.mantissa)
		}
	}
	return z
}

// SetBigMantScale sets z to the given value and scale.
func (z *Big) SetBigMantScale(value *big.Int, scale int32) *Big {
	if value.Sign() == 0 {
		z.form = zero
		return z
	}
	z.scale = scale
	z.mantissa.Set(value)
	z.form = finite
	z.compact = c.Inflated
	return z
}

// SetContext sets z's Context and returns z.
func (z *Big) SetContext(ctx Context) *Big {
	z.ctx = ctx
	return z
}

// SetFloat64 sets z to the provided float64.
//
// Remember floating-point to decimal conversions can be lossy. For example,
// the floating-point number `0.1' appears to simply be 0.1, but its actual
// value is 0.1000000000000000055511151231257827021181583404541015625.
//
// SetFloat64 is particularly lossy because will round non-integer values.
// For example, if passed the value `3.1415' it attempts to do the same as if
// SetMantScale(31415, 4) were called.
//
// To do this, it scales up the provided number by its scale. This involves
// rounding, so approximately 2.3% of decimals created from floats will have a
// rounding imprecision of ± 1 ULP.
func (z *Big) SetFloat64(value float64) *Big {
	if value == 0 {
		z.form = 0
		return z
	}

	var scale int32

	// If value is not an integer (has a fractional part) bump its value up
	// and find the appropriate scale.
	_, fr := math.Modf(value)
	if fr != 0 {
		scale = findScale(value)
		value *= math.Pow10(int(scale))
	}

	if math.IsNaN(value) {
		panic(ErrNaN{"NewFromFloat(NaN)"})
	}
	if math.IsInf(value, 0) {
		z.form = inf
		return z
	}

	// Given float64(math.MaxInt64) == math.MaxInt64.
	if value <= math.MaxInt64 {
		z.compact = int64(value)
	} else {
		if value <= math.MaxUint64 {
			z.mantissa.SetUint64(uint64(value))
		} else {
			z.mantissa.Set(bigIntFromFloat(value))
		}
		z.compact = c.Inflated
	}
	z.scale = scale
	z.form = finite
	return z
}

// SetInf sets z to Inf and returns z.
func (x *Big) SetInf() *Big {
	x.form = inf
	return x
}

// SetMantScale sets z to the given value and scale.
func (z *Big) SetMantScale(value int64, scale int32) *Big {
	if value == 0 {
		z.form = zero
		return z
	}
	z.scale = scale
	if value == c.Inflated {
		z.mantissa.SetInt64(value)
	}
	z.compact = value
	z.form = finite
	return z
}

// SetMode sets z's RoundingMode to mode and returns z.
func (z *Big) SetMode(mode RoundingMode) *Big {
	z.ctx.mode = mode
	return z
}

// SetPrec sets z's precision to prec and returns z.
// This method is distinct from Prec. This sets the internal context
// which dictates rounding and digits after the radix for lossy operations.
// The latter describes the number of digits in the decimal.
func (z *Big) SetPrec(prec int32) *Big {
	z.ctx.precision = prec
	return z
}

// SetScale sets z's scale to scale and returns z.
func (z *Big) SetScale(scale int32) *Big {
	z.scale = scale
	return z
}

// SetString sets z to the value of s, returning z and a bool
// indicating success. s must be a string in one of the following
// formats:
//
// 	1.234
// 	1234
// 	1.234e+5
// 	1.234E-5
// 	0.000001234
// 	Inf
// 	+Inf
// 	-Inf
//
//	No distinction is made between +Inf and -Inf.
func (z *Big) SetString(s string) (*Big, bool) {
	// Inf or +Inf or -Inf
	if (len(s) == 3 && equalFold(s, "Inf")) ||
		(len(s) == 4 && (s[0] == '+' || s[0] == '-') &&
			equalFold(s[1:], "Inf")) {
		z.form = inf
		return z, true
	}

	var scale int32

	// Check for a scientific string.
	i := strings.LastIndexAny(s, "Ee")
	if i > 0 {
		eint, err := strconv.ParseInt(s[i+1:], 10, 32)
		if err != nil {
			return nil, false
		}
		s = s[:i]
		scale = -int32(eint)
	}

	switch strings.Count(s, ".") {
	case 0:
	case 1:
		i = strings.IndexByte(s, '.')
		s = s[:i] + s[i+1:]
		scale += int32(len(s) - i)
	default:
		return nil, false
	}

	var err error
	// Numbers == 19 can be out of range, but try the edge case anyway.
	if len(s) <= 19 {
		z.compact, err = strconv.ParseInt(s, 10, 64)
		if err != nil {
			nerr, ok := err.(*strconv.NumError)
			if !ok || nerr.Err == strconv.ErrSyntax {
				return nil, false
			}
			err = nerr.Err
		}
	}
	if (err == strconv.ErrRange && len(s) == 19) || len(s) > 19 {
		_, ok := z.mantissa.SetString(s, 10)
		if !ok {
			return nil, false
		}
		z.compact = c.Inflated
	}
	z.scale = scale
	z.form = finite
	return z, true
}

// Sign returns:
//
//	-1 if x <   0
//	 0 if x is ±0
//	+1 if x >   0
//
// Undefined if
func (x *Big) Sign() int {
	if x.form == zero {
		return 0
	}
	if x.isCompact() {
		// See: https://github.com/golang/go/issues/16203
		if runtime.GOARCH == "amd64" {
			// Hacker's Delight, page 21, section 2-8.
			// This prevents the incorrect answer for -1 << 63.
			return int((x.compact >> 63) | int64(uint64(-x.compact)>>63))
		}
		if x.compact == 0 {
			return 0
		}
		if x.compact < 0 {
			return -1
		}
		return +1
	}
	return x.mantissa.Sign()
}

// SignBit returns true if x is negative.
func (x *Big) SignBit() bool {
	return (x.isCompact() && x.compact < 0) ||
		(x.isInflated() && x.mantissa.Sign() < 0)
}

// String returns the scientific string representation of x.
// For special cases, x == nil returns "<nil>" and x.IsInf() returns "Inf".
func (x *Big) String() string {
	return x.toString(true, lower)
}

// PlainString returns the plain string representation of x.
// For special cases, if x == nil returns "<nil>" and x.IsInf() returns "Inf".
func (x *Big) PlainString() string {
	return x.toString(false, lower)
}

const (
	lower = 0 // opts for lowercase sci notation
	upper = 1 // opts for uppercase sci notation
)

func (x *Big) toString(sci bool, opts byte) string {
	if x == nil {
		return "<nil>"
	}
	if x.IsInf() {
		return "Inf"
	}

	// Fast path: return our value as-is.
	if x.scale == 0 {
		if x.isInflated() {
			return x.mantissa.String()
		}
		return strconv.FormatInt(x.compact, 10)
	}

	// Keep from allocating a buffer if x is zero.
	if (x.isCompact() && x.compact == 0) ||
		(x.isInflated() && x.mantissa.Sign() == 0) {
		return "0"
	}

	// (x.scale > 0 || x.scale < 0) && x != 0

	var (
		str string
		b   buffer // is bytes.Buffer
	)

	if x.isInflated() {
		str = x.mantissa.String()
	} else {
		str = strconv.FormatInt(x.compact, 10)
	}

	// Either this or we have to use a boolean flag plus format the mantissa
	// and/or compact integer as unsigned values which incurs
	// an allocation with the mantissa.
	if str[0] == '-' {
		b.WriteByte('-')
		str = str[1:]
	}
	if sci {
		return x.toSciString(str, &b, opts)
	}
	return x.toPlainString(str, &b)
}

func (x *Big) toSciString(str string, b writer, opts byte) string {
	if debug && (opts < 0 || opts > 1) {
		panic("toSciString: (bug) opts != 0 || opts != 1")
	}

	// Following quotes are from:
	// http://speleotrove.com/decimal/daconvs.html#reftostr

	adj := -int(x.scale) + (len(str) - 1)
	pos := adj > 0

	// "If the exponent is less than or equal to zero and the
	// adjusted exponent is greater than or equal to -6..."
	if x.scale >= 0 && adj >= -6 {
		// "...the number will be converted to a character
		// form without using exponential notation."
		return x.normString(str, b)
	}

	b.WriteByte(str[0])
	if len(str) > 1 {
		b.WriteByte('.')
		b.WriteString(str[1:])
	}
	if adj != 0 {
		b.WriteByte([2]byte{'e', 'E'}[opts])
		// If !pos the following strconv.Itoa call will add
		// the minus sign for us.
		if pos {
			b.WriteByte('+')
		}
		b.WriteString(strconv.Itoa(adj))
	}
	return b.String()
}

// toPlainString returns the plain string version of x.
func (x *Big) toPlainString(str string, b writer) string {
	// Just mantissa + z.scale "0"s -- no radix.
	if x.scale < 0 {
		b.WriteString(str)
		b.Write(bytes.Repeat([]byte{'0'}, -int(x.scale)))
		return b.String()
	}
	return x.normString(str, new(buffer))
}

// normString returns the plain string version of x.
// It's distinct from 'toPlainString' in that toPlainString
// calls this method once it's done its own internal checks.
// Additionally, toSciString also calls this method if it
// does not need to add the {e,E} suffix.
func (x *Big) normString(str string, b writer) string {
	switch pad := len(str) - int(x.scale); {

	// log10(mantissa) == scale, so immediately before str.
	case pad == 0:
		b.WriteString("0.")
		b.WriteString(str)

	// log10(mantissa) > scale, so somewhere inside str.
	case pad > 0:
		b.WriteString(str[:pad])
		b.WriteByte('.')
		b.WriteString(str[pad:])

	// log10(mantissa) < scale, so before p "0s" and before str.
	default:
		b.WriteString("0.")
		b.Write(bytes.Repeat([]byte{'0'}, -pad))
		b.WriteString(str)
	}
	return b.String()
}

// Sqrt sets z to the square root of x and returns z.
// The precision of Sqrt is determined by z's Context.
// Sqrt will panic on negative values since Big cannot
// represent imaginary numbers.
func (z *Big) Sqrt(x *Big) *Big {
	if x.SignBit() {
		panic("math.Sqrt: cannot take square root of negative number")
	}

	switch {
	case x.form == inf:
		z.form = inf
		return z
	case x.Sign() == 0:
		z.form = zero
		return z
	}

	// First fast path---check if x is a perfect square. If it is, we can avoid
	// having to inflate x and can possibly use can use the hardware SQRT.
	// Note that we can only catch perfect squares that aren't big.Ints.
	if sq, ok := perfectSquare(x); ok {
		z.ctx = x.ctx
		return z.SetMantScale(sq, 0)
	}

	zp := z.ctx.prec()

	// Temporary inflation. Should be enough to accurately determine the sqrt
	// with at least zp digits after the radix.
	zpadj := int(zp) << 1

	var tmp *Big
	if z != x {
		zctx := z.ctx
		tmp = z.Set(x)
		tmp.ctx = zctx
	} else {
		tmp = new(Big).Set(x)
	}
	if !shiftRadixRight(tmp, zpadj) {
		z.form = inf
		return z
	}

	// Second fast path. Check to see if we can calculate the square root without
	// using big.Int
	if !x.IsBig() && zpadj <= 19 {
		n := tmp.Int64()
		ix := n >> uint((arith.BitLen(n)+1)>>1)
		var p int64
		for {
			p = ix
			ix += n / ix
			ix >>= 1
			if ix == p {
				return z.SetMantScale(ix, zp)
			}
		}
	}

	// x isn't a perfect square or x is a big.Int

	n := tmp.Int()
	ix := new(big.Int).Rsh(n, uint((n.BitLen()+1)>>1))

	var a, p big.Int
	for {
		p.Set(ix)
		ix.Add(ix, a.Quo(n, ix)).Rsh(ix, 1)
		if ix.Cmp(&p) == 0 {
			return z.SetBigMantScale(ix, zp)
		}
	}
}

// perfectSquare algorithm slightly partially borrowed from
// https://stackoverflow.com/a/295678/2967113
func perfectSquare(x *Big) (square int64, ok bool) {
	if x.IsBig() || !x.IsInt() {
		return 0, false
	}
	xc := x.Int64()
	h := xc & 0xF
	if h > 9 {
		return 0, false
	}
	if h != 2 && h != 3 && h != 5 && h != 6 && h != 7 && h != 8 {
		// "Show that floating point sqrt(x*x) >= x for all long x."
		// https://math.stackexchange.com/a/238885/153292
		tst := int64(math.Sqrt(float64(xc)))
		return tst, tst*tst == xc
	}
	return 0, false
}

// Sub sets z to x - y and returns z.
func (z *Big) Sub(x, y *Big) *Big {
	if x.form == finite && y.form == finite {
		// TODO: Write this without using Neg to save an allocation.
		return z.Add(x, new(Big).Neg(y))
	}

	if x.form == inf && y.form == inf &&
		x.Sign() == y.Sign() {
		// +Inf - +Inf
		// -Inf - -Inf
		z.form = zero
		panic(ErrNaN{"subtraction of infinities with equal signs"})
	}

	if x.form == zero && y.form == zero {
		// ±0 - ±0
		z.form = zero
		return z
	}

	if x.form == inf || y.form == zero {
		// ±Inf - y
		// x - ±0
		return z.Set(x)
	}

	// ±0 - y
	// x - ±Inf
	return z.Neg(y)
}

// UnmarshalText implements encoding/TextUnmarshaler.
func (x *Big) UnmarshalText(data []byte) error {
	_, ok := x.SetString(string(data))
	if !ok {
		return errors.New("Big.UnmarshalText: invalid decimal format")
	}
	return nil
}
