package broker

// ApplyFill folds a fill into a position: it blends the average price when the
// fill adds exposure, and realizes P&L when the fill reduces it.
//
// This is THE position arithmetic for the whole platform. The paper broker, the
// backtester, and any live attribution book all call it, which is what makes a
// backtest and a paper run agree by construction rather than by hopeful
// coincidence — the alternative is two implementations that drift apart and
// silently disagree about what a strategy earned.
func ApplyFill(p *Position, f Fill) {
	signedQty := f.Quantity
	if f.Side == SideSell {
		signedQty = -f.Quantity
	}

	addingExposure := (p.NetQuantity >= 0 && signedQty > 0) ||
		(p.NetQuantity <= 0 && signedQty < 0)

	if addingExposure {
		totalQty := p.NetQuantity + signedQty
		if totalQty != 0 {
			p.AveragePrice = (p.AveragePrice*float64(absInt(p.NetQuantity)) +
				f.Price*float64(f.Quantity)) / float64(absInt(totalQty))
		}
		p.NetQuantity = totalQty
	} else {
		// Reducing or closing: realize P&L on the portion that closed.
		closingQty := absInt(signedQty)
		if closingQty > absInt(p.NetQuantity) {
			closingQty = absInt(p.NetQuantity)
		}
		sign := 1
		if p.NetQuantity < 0 {
			sign = -1
		}
		p.PnL += float64(sign) * (f.Price - p.AveragePrice) * float64(closingQty)
		p.NetQuantity += signedQty
		if p.NetQuantity == 0 {
			p.AveragePrice = 0
		}
	}

	p.LastPrice = f.Price
	p.Updated = f.Timestamp
}

// MarkToMarket returns a position's realized plus unrealized P&L at `last`.
func MarkToMarket(p Position, last float64) float64 {
	if p.NetQuantity == 0 || last <= 0 {
		return p.PnL // realized only
	}
	sign := 1.0
	if p.NetQuantity < 0 {
		sign = -1.0
	}
	return p.PnL + sign*(last-p.AveragePrice)*float64(absInt(p.NetQuantity))
}
