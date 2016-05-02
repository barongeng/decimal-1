package decimal

import "testing"

var x Big

func BenchmarkAdd(b *testing.B) {
	y := New(5678, 3)
	for i := 0; i < b.N; i++ {
		x.Add(&x, y)
	}
}

func BenchmarkSub(b *testing.B) {
	y := New(5678, 3)
	for i := 0; i < b.N; i++ {
		x.Sub(&x, y)
	}
}

func BenchmarkMul(b *testing.B) {
	y := New(12345, 4)
	for i := 0; i < b.N; i++ {
		x.Mul(&x, y)
	}
}

func BenchmarkQuo(b *testing.B) {
	y := New(1234, 3)
	for i := 0; i < b.N; i++ {
		x.Quo(&x, y)
	}
}
