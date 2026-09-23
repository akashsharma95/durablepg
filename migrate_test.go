package durablepg

import (
	"context"
	"testing"
)

func TestEnsurePartitionsBoundsValidation(t *testing.T) {
	e := &Engine{schema: "durable", qSchema: quoteIdentifier("durable")}
	for _, months := range []int{-1, 121} {
		err := e.EnsurePartitions(context.Background(), months)
		if err == nil {
			t.Errorf("EnsurePartitions(%d) expected error for out-of-bounds value, got nil", months)
		}
	}
}
