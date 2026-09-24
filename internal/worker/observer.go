// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

// StartPassiveObserver runs a background loop that periodically pages over all
// deployed resources and enqueues passive observation messages for drift detection.
func (w *Worker) StartPassiveObserver(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.schedulePassiveObservations(ctx)
			}
		}
	}()
}

func (w *Worker) schedulePassiveObservations(ctx context.Context) {
	limit := 100
	sequence := uint64(0)
	for {
		var page application.ResourceInventoryPage
		err := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
			var err error
			page, err = tx.Resources().ListResources(ctx, application.ResourceListQuery{
				Limit:         limit,
				AfterSequence: sequence,
				Unrestricted:  true,
			})
			return err
		})
		if err != nil {
			break
		}

		for _, item := range page.Items {
			if item.Status.State == domain.ResourceStateReady {
				w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
					loaded, err := tx.Resources().LookupResource(ctx, item.ID)
					if err == nil {
						tx.Outbox().Enqueue(ctx, application.PassiveObserveMessage(item.ID, 0, loaded.Version))
					}
					return nil
				})
			}
		}

		if page.NextSequence == 0 {
			break
		}
		sequence = page.NextSequence
	}
}
