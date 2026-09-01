package shape

import (
	"math"
	"testing"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestWasserstein1(t *testing.T) {
	cases := []struct {
		name string
		a, b []uint32
		want float64
	}{
		{"identical", []uint32{0, 10}, []uint32{0, 10}, 0},
		{"two point masses", []uint32{0, 0}, []uint32{10, 10}, 10},
		{"one point shifted", []uint32{0, 10}, []uint32{0, 20}, 5},
		{"unequal sizes, same shape", []uint32{5, 5}, []uint32{5, 5, 5, 5}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Wasserstein1(c.a, c.b); !approx(got, c.want, 1e-9) {
				t.Fatalf("Wasserstein1 = %v, want %v", got, c.want)
			}
		})
	}
	// Symmetry.
	a := []uint32{1, 4, 9, 16}
	b := []uint32{2, 3, 10, 40}
	if !approx(Wasserstein1(a, b), Wasserstein1(b, a), 1e-9) {
		t.Fatal("Wasserstein1 is not symmetric")
	}
	// A bigger shift means a bigger distance.
	near := Wasserstein1([]uint32{100, 100, 100}, []uint32{110, 110, 110})
	far := Wasserstein1([]uint32{100, 100, 100}, []uint32{200, 200, 200})
	if !(far > near) {
		t.Fatalf("expected far (%v) > near (%v)", far, near)
	}
	// Empty is undefined → 0.
	if Wasserstein1(nil, []uint32{1}) != 0 {
		t.Fatal("empty input should give 0")
	}
}

func TestBimodalityCoefficient(t *testing.T) {
	// Too few samples, or no spread: undefined.
	if _, ok := BimodalityCoefficient([]uint32{1, 2, 3}); ok {
		t.Fatal("fewer than 4 samples should be not-ok")
	}
	if _, ok := BimodalityCoefficient([]uint32{5, 5, 5, 5, 5}); ok {
		t.Fatal("zero spread should be not-ok")
	}

	// Two tight, well-separated clusters: clearly bimodal, well above 0.555.
	var bi []uint32
	for i := 0; i < 20; i++ {
		bi = append(bi, 1000)
	}
	for i := 0; i < 20; i++ {
		bi = append(bi, 9000)
	}
	bc, ok := BimodalityCoefficient(bi)
	if !ok || bc <= 0.555 {
		t.Fatalf("two clusters: bc=%v ok=%v, want > 0.555", bc, ok)
	}

	// A single tight peak with a light spread: unimodal, below the 0.555 mark.
	uni := []uint32{995, 998, 999, 1000, 1000, 1000, 1001, 1002, 1005, 1000, 1000, 999, 1001, 1000, 1000, 1000}
	bcu, ok := BimodalityCoefficient(uni)
	if !ok {
		t.Fatal("unimodal sample should be ok")
	}
	if bcu >= bc {
		t.Fatalf("unimodal bc (%v) should be below bimodal bc (%v)", bcu, bc)
	}
}
