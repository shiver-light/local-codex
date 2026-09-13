package calculator

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Errorf("Add(2,3) = %d, want 5", Add(2, 3))
	}
}

func TestSub(t *testing.T) {
	if Sub(10, 4) != 6 {
		t.Errorf("Sub(10,4) = %d, want 6", Sub(10, 4))
	}
}

func TestMul(t *testing.T) {
	if Mul(3, 4) != 12 {
		t.Errorf("Mul(3,4) = %d, want 12", Mul(3, 4))
	}
}
