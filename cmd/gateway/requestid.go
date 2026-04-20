package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

type requestIDKeyType struct{}

var requestIDKey = requestIDKeyType{}

// newRequestID returns a 12-character random hex string.
// Falls back to a timestamp-derived value if the OS entropy pool is unavailable.
func newRequestID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	return hex.EncodeToString(b)
}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

func requestIDFromCtx(ctx context.Context) string {
	if ctx != nil {
		if id, ok := ctx.Value(requestIDKey).(string); ok {
			return id
		}
	}
	return newRequestID()
}
