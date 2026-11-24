package apilog

import "context"

type releaseKey struct{}

// WithRelease records the ACS patch that started this work.
func WithRelease(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, releaseKey{}, version)
}

// Release returns the ACS patch associated with the work.
func Release(ctx context.Context) string {
	version, _ := ctx.Value(releaseKey{}).(string)
	return version
}
