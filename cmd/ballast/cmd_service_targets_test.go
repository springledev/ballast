package main

import (
	"reflect"
	"testing"

	"github.com/springledev/ballast/internal/inventory"
)

func TestParseTargetFlag(t *testing.T) {
	tests := []struct {
		in      string
		want    inventory.Target
		wantErr bool
	}{
		{in: "b2", want: inventory.Target{Destination: "b2"}},
		{in: "b2=library", want: inventory.Target{Destination: "b2", Parts: []string{"library"}}},
		{in: "b2 = database, library", want: inventory.Target{Destination: "b2", Parts: []string{"database", "library"}}},
		{in: "", wantErr: true},
		{in: "=library", wantErr: true},
		{in: "b2=", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseTargetFlag(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
