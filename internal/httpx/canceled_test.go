package httpx

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestCanceled(t *testing.T) {
	if !Canceled(context.Canceled) || !Canceled(fmt.Errorf("query: %w", context.Canceled)) {
		t.Error("a cancelled request, even wrapped, is the client leaving")
	}
	if Canceled(errors.New("boom")) || Canceled(context.DeadlineExceeded) || Canceled(nil) {
		t.Error("real failures (and our own timeouts) are still errors")
	}
}
