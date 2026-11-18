package main

import (
	"strconv"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
)

func TestDeliveryPatchLimitHasACeiling(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
		ok    bool
	}{
		{strconv.Itoa(server.DefaultMaxPatchBytes), server.DefaultMaxPatchBytes, true},
		{strconv.Itoa(server.MaxPatchBytesCeiling), server.MaxPatchBytesCeiling, true},
		{strconv.Itoa(server.MaxPatchBytesCeiling + 1), 0, false},
		{"0", 0, false},
		{"-1", 0, false},
		{"16MiB", 0, false},
	} {
		got, err := deliveryPatchLimit(tc.value)
		if got != tc.want || (err == nil) != tc.ok {
			t.Errorf("deliveryPatchLimit(%q) = %d, %v; want %d, ok=%v", tc.value, got, err, tc.want, tc.ok)
		}
	}
}
