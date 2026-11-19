package retention

import (
	"context"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func pruneReceipts(ctx context.Context, p *pass, cutoff time.Time) error {
	return p.delete(ctx, "receipts", "created_at < ?", core.ReceiptTimestamp(cutoff))
}
