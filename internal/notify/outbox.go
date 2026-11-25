package notify

import (
	"context"
	"errors"
	"github.com/maxya/TapePilot/internal/store"
	"time"
)

// DeliverOne reserves and delivers one terse durable event. SMTP failure is
// recorded in the inbox/outbox and never changes archive or job integrity.
func DeliverOne(ctx context.Context, db *store.Store, c Config) (bool, error) {
	return deliverOne(ctx, db, c, Send)
}
func deliverOne(ctx context.Context, db *store.Store, c Config, sender func(context.Context, Config, Message) (Result, error)) (bool, error) {
	pending, e := db.ClaimOutbox(ctx, time.Now().UTC())
	if errors.Is(e, store.ErrNotFound) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	result, deliveryErr := sender(ctx, c, Message{pending.ID, pending.JobID, pending.Code})
	code := "SMTP_DELIVERED"
	permanent := false
	if deliveryErr != nil {
		var classified *DeliveryError
		if errors.As(deliveryErr, &classified) {
			code = classified.Code
			permanent = !classified.Temporary
		} else {
			code = "SMTP_DELIVERY_FAILED"
			permanent = true
		}
	}
	if result.Accepted {
		permanent = false
	}
	next := time.Now().UTC().Add(time.Minute * time.Duration(1<<uint(pending.Attempts-1)))
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if e = db.FinishOutbox(recordCtx, pending, result.Accepted, permanent, code, next); e != nil {
		return true, e
	}
	return true, nil
}
func Run(ctx context.Context, db *store.Store, c Config) error {
	if e := c.Validate(); e != nil {
		return e
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		for n := 0; n < 16; n++ {
			if e := ctx.Err(); e != nil {
				return nil
			}
			worked, e := DeliverOne(ctx, db, c)
			if e != nil {
				return e
			}
			if !worked {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
