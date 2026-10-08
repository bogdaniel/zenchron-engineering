package shapes

import "testing"

func TestTotal(t *testing.T) {
	if Total(nil) != 0 {
		t.Fatal("empty total")
	}
}

func TestSquare_Area(t *testing.T) {
	_ = NewSquare(2).Area()
}
