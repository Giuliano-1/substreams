package block

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBoundedRange_computeInitialBounds(t *testing.T) {
	type fields struct {
		moduleInitBlock          uint64
		requestStartBlock        uint64
		requestExclusiveEndBlock uint64
	}
	tests := []struct {
		name   string
		fields fields
		want   string
	}{
		{
			"after init block",
			fields{8, 10, 20},
			"10-20",
		},
		{
			"after init block, start off bound",
			fields{8, 12, 20},
			"10-20", // fixme: simple solution for the production-mode issue
		},
		{
			"range below interval",
			fields{8, 12, 18},
			"10-18", // fixme: simple solution for the production-mode issue
		},
		{
			"below module init block",
			fields{8, 2, 20},
			"8-10",
		},
		{
			"just above module init block",
			fields{8, 9, 20},
			"8-10",
		},
		{
			"below module init block 2",
			fields{8, 9, 20},
			"8-10",
		},
		{
			"init block beyond",
			fields{32, 2, 8},
			"nil",
		},
		{
			"init block beyond 2",
			fields{32, 2, 12},
			"nil",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewBoundedRange(tt.fields.moduleInitBlock, 10, tt.fields.requestStartBlock, tt.fields.requestExclusiveEndBlock)

			res := r.Range
			if tt.want == "nil" {
				assert.Nil(t, res)
			} else {
				assert.Equalf(t, ParseRange(tt.want), res, "computeInitialBounds()")
			}
		})
	}
}
