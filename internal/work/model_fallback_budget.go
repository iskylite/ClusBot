package work

import "context"

// The execution host reserves each additional model attempt before dispatch.
// Contexts without a host (for example an explicit standalone preview) have
// no durable run budget and still obey the adapter's per-policy attempt cap.
type modelFallbackReservationKey struct{}

type modelFallbackReservation func(context.Context) error

func withModelFallbackReservation(ctx context.Context, reserve modelFallbackReservation) context.Context {
	return context.WithValue(ctx, modelFallbackReservationKey{}, reserve)
}

func ReserveModelFallbackAttempt(ctx context.Context) error {
	if reserve, ok := ctx.Value(modelFallbackReservationKey{}).(modelFallbackReservation); ok {
		return reserve(ctx)
	}
	return nil
}
