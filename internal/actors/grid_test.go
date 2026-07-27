package actors

import (
	"reflect"
	"testing"
)

func TestGridDims(t *testing.T) {
	cases := []struct {
		in   string
		want []int
		ok   bool
	}{
		{"2x3x4", []int{2, 3, 4}, true},
		{"1x1x1", []int{1, 1, 1}, true},
		{"2X3X4", []int{2, 3, 4}, true},   // uppercase separator
		{" 2x3x4 ", []int{2, 3, 4}, true}, // surrounding whitespace
		{"10x2x2", []int{10, 2, 2}, true},
		{"2 3 4", []int{2, 3, 4}, true},         // space-separated
		{"  2   3   4  ", []int{2, 3, 4}, true}, // extra whitespace
		{"2x3 4", []int{2, 3, 4}, true},         // mixed separators
		{"2X3 4", []int{2, 3, 4}, true},         // mixed, uppercase
		{"3x4", []int{3, 4}, true},              // 2-D
		{"3 4", []int{3, 4}, true},              // 2-D space-separated
		{"4", []int{4}, true},                   // 1-D
		{" 4 ", []int{4}, true},                 // 1-D with whitespace
		{"2xx4", []int{2, 4}, true},             // repeated separators collapse
		{"2x3x4x5", []int{2, 3, 4, 5}, true},    // arity is not enforced here
		{"0x3x4", nil, false},                   // zero not allowed
		{"2x-1x4", nil, false},                  // negative not allowed
		{"axbxc", nil, false},                   // non-numeric
		{"3xfoo", nil, false},                   // non-numeric token
	}

	for _, c := range cases {
		got, err := gridDims(c.in)
		if c.ok {
			if err != nil {
				t.Errorf("gridDims(%q) unexpected error: %v", c.in, err)
				continue
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("gridDims(%q) = %v, want %v", c.in, got, c.want)
			}
		} else if err == nil {
			t.Errorf("gridDims(%q) expected error, got %v", c.in, got)
		}
	}

	// Empty spec parses to zero dimensions without error; arity is validated by
	// the caller (classifyGrid).
	if got, err := gridDims(""); err != nil || len(got) != 0 {
		t.Errorf("gridDims(\"\") = (%v, %v), want ([], nil)", got, err)
	}
}

func TestClassifyGrid(t *testing.T) {
	cases := []struct {
		name string
		dims []int
		here bool
		want gridKind
	}{
		{"1D auto -> same lane", []int{4}, false, gridSameLane},
		{"1D with --here -> same lane", []int{4}, true, gridSameLane},
		{"2D auto -> active tab", []int{3, 4}, false, gridActiveTab},
		{"2D with --here -> active tab", []int{3, 4}, true, gridActiveTab},
		{"3D auto -> new tabs", []int{2, 3, 4}, false, gridNewTabs},
		{"3D with --here -> invalid", []int{2, 3, 4}, true, gridInvalid},
		{"4D -> invalid", []int{1, 2, 3, 4}, false, gridInvalid},
		{"empty -> invalid", []int{}, false, gridInvalid},
		{"empty with --here -> invalid", []int{}, true, gridInvalid},
	}

	for _, c := range cases {
		if got := classifyGrid(c.dims, c.here); got != c.want {
			t.Errorf("%s: classifyGrid(%v, here=%v) = %d, want %d", c.name, c.dims, c.here, got, c.want)
		}
	}
}
