package main

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/metadata"
)

// firestoreProbe writes a document and reads it back with the official
// Firestore client, at the emulator q's "endpoint" names (the Service's
// address, which a pod dials directly), in q's "project", sending q's
// "traceparent" with each call (#1288): the call front in the emulator's pod
// records a pod's calls, which the host tunnel never sees.
func firestoreProbe(ctx context.Context, q url.Values) (string, error) {
	if err := os.Setenv("FIRESTORE_EMULATOR_HOST", q.Get("endpoint")); err != nil {
		return "", err
	}
	c, err := firestore.NewClient(ctx, q.Get("project"))
	if err != nil {
		return "", err
	}
	defer c.Close()
	if tp := q.Get("traceparent"); tp != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "traceparent", tp)
	}
	doc := c.Collection(q.Get("collection")).Doc(q.Get("doc"))
	if _, err := doc.Set(ctx, map[string]any{"from": "pod"}); err != nil {
		return "", fmt.Errorf("Set: %w", err)
	}
	snap, err := doc.Get(ctx)
	if err != nil {
		return "", fmt.Errorf("Get: %w", err)
	}
	return fmt.Sprintf("endpoint=%s from=%v", q.Get("endpoint"), snap.Data()["from"]), nil
}
