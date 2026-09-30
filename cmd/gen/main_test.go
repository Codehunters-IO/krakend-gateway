package main

import (
	"reflect"
	"testing"
)

func TestSplitProducts(t *testing.T) {
	cases := []struct {
		name string
		csv  string
		want []string
	}{
		{"plain", "a,b", []string{"a", "b"}},
		{"trims surrounding spaces", "forgeos, vitxo", []string{"forgeos", "vitxo"}},
		{"empty value is nil", "", nil},
		{"empty element is dropped", "a,,b", []string{"a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := splitProducts(c.csv); !reflect.DeepEqual(got, c.want) {
				t.Errorf("splitProducts(%q) = %v, want %v", c.csv, got, c.want)
			}
		})
	}
}
