package history

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"kite-algo/internal/kite"
	"kite-algo/internal/storage"
)

// SnapshotInstruments records today's instrument master.
//
// THIS IS THE MOST TIME-SENSITIVE OPERATION IN THE PLATFORM.
//
// Kite's /instruments feed lists only contracts that are currently live, and
// historical candles are keyed by instrument_token. The moment a weekly option
// expires, its token disappears from the API — and with it any ability to
// resolve that contract for a backtest. The data cannot be recovered later at
// any price.
//
// So: every day the server runs without writing a snapshot is a day that can
// never be backtested. It is called on every successful Zerodha login, and is a
// no-op once the day already has a snapshot.
func SnapshotInstruments(ctx context.Context, store storage.HistoryStore, m *kite.Instruments, asOf time.Time, logger *slog.Logger) error {
	if store == nil || m == nil {
		return nil
	}

	have, err := store.HasInstrumentSnapshot(ctx, asOf)
	if err != nil {
		return err
	}
	if have {
		return nil
	}

	rows := toRows(m)
	if len(rows) == 0 {
		return nil
	}
	if err := store.SaveInstrumentSnapshot(ctx, asOf, rows); err != nil {
		return fmt.Errorf("save instrument snapshot: %w", err)
	}

	if logger != nil {
		logger.Info("instrument master snapshotted for backtesting",
			"as_of", asOf.In(IST).Format("2006-01-02"), "instruments", len(rows))
	}
	return nil
}

// toRows converts the live instrument master to storable rows.
func toRows(m *kite.Instruments) []storage.InstrumentRow {
	all := m.All()
	out := make([]storage.InstrumentRow, 0, len(all))
	for i := range all {
		inst := &all[i]
		out = append(out, storage.InstrumentRow{
			InstrumentToken: inst.InstrumentToken,
			TradingSymbol:   inst.TradingSymbol,
			Name:            inst.Name,
			Expiry:          inst.Expiry,
			Strike:          inst.Strike,
			LotSize:         inst.LotSize,
			InstrumentType:  inst.InstrumentType,
			Segment:         inst.Segment,
			Exchange:        inst.Exchange,
			TickSize:        inst.TickSize,
		})
	}
	return out
}

// AsOfInstruments is a point-in-time instrument master, reconstructed from a
// snapshot. Backtests resolve their option chains through this rather than the
// live master, which only knows about contracts that have not yet expired.
type AsOfInstruments struct {
	date     time.Time
	bySymbol map[string]storage.InstrumentRow
	rows     []storage.InstrumentRow
}

// LoadAsOf reads the instrument master as it stood on a date.
func LoadAsOf(ctx context.Context, store storage.HistoryStore, date time.Time) (*AsOfInstruments, error) {
	rows, err := store.InstrumentsAsOf(ctx, date)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no instrument snapshot on or before %s — "+
			"contracts from that date cannot be resolved, and the data is not "+
			"recoverable from Kite after expiry", date.In(IST).Format("2006-01-02"))
	}

	idx := make(map[string]storage.InstrumentRow, len(rows))
	for _, r := range rows {
		idx[r.TradingSymbol] = r
	}
	return &AsOfInstruments{date: date, bySymbol: idx, rows: rows}, nil
}

// Lookup resolves a symbol as of the snapshot date.
func (a *AsOfInstruments) Lookup(symbol string) (storage.InstrumentRow, bool) {
	r, ok := a.bySymbol[symbol]
	return r, ok
}

// Options returns the option chain for an underlying's nearest expiry on or
// after minExpiry, as it existed on the snapshot date.
func (a *AsOfInstruments) Options(underlying string, minExpiry time.Time) []storage.InstrumentRow {
	var target time.Time
	for _, r := range a.rows {
		if r.Name != underlying || !isOption(r) {
			continue
		}
		if !minExpiry.IsZero() && r.Expiry.Before(minExpiry) {
			continue
		}
		if target.IsZero() || r.Expiry.Before(target) {
			target = r.Expiry
		}
	}

	var out []storage.InstrumentRow
	for _, r := range a.rows {
		if r.Name == underlying && isOption(r) && r.Expiry.Equal(target) {
			out = append(out, r)
		}
	}
	return out
}

// Count reports how many instruments the snapshot holds.
func (a *AsOfInstruments) Count() int { return len(a.rows) }

func isOption(r storage.InstrumentRow) bool {
	return r.InstrumentType == "CE" || r.InstrumentType == "PE"
}
