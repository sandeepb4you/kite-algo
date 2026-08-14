package kite

import "context"

// Profile is the GET /user/profile response (trimmed to fields we use).
// ValidateToken fetches it mainly to confirm the access token works at startup.
type Profile struct {
	UserName string `json:"user_name"`
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	Broker   string `json:"broker"`
}

// GetProfile returns the user profile. Use it at startup to validate that the
// access token is live.
func (c *Client) GetProfile(ctx context.Context) (Profile, error) {
	var out struct {
		Profile
	}
	if err := c.get(ctx, "/user/profile", nil, &out); err != nil {
		return Profile{}, err
	}
	return out.Profile, nil
}

// KitePosition is one row of GET /portfolio/positions (day or net).
type KitePosition struct {
	Tradingsymbol     string  `json:"tradingsymbol"`
	Exchange          string  `json:"exchange"`
	Product           string  `json:"product"`
	Quantity          int     `json:"quantity"` // net (signed)
	OvernightQuantity int     `json:"overnight_quantity"`
	Multiplier        float64 `json:"multiplier"`
	LastPrice         float64 `json:"last_price"`
	AveragePrice      float64 `json:"average_price"`
	ClosePrice        float64 `json:"close_price"`
	PnL               float64 `json:"pnl"`
	SellValue         float64 `json:"sell_value"`
	BuyValue          float64 `json:"buy_value"`
	DaySellQuantity   int     `json:"day_sell_quantity"`
	DayBuyQuantity    int     `json:"day_buy_quantity"`
}

// GetPositions returns Kite's positions for the requested view ("net" or "day").
func (c *Client) GetPositions(ctx context.Context) (map[string][]KitePosition, error) {
	var out struct {
		Net []KitePosition `json:"net"`
		Day []KitePosition `json:"day"`
	}
	if err := c.get(ctx, "/portfolio/positions", nil, &out); err != nil {
		return nil, err
	}
	return map[string][]KitePosition{"net": out.Net, "day": out.Day}, nil
}

// Margin is the GET /user/margins/{segment} response (trimmed).
type Margin struct {
	Available struct {
		LiveBalance    float64 `json:"live_balance"`
		Cash           float64 `json:"cash"`
		OpeningBalance float64 `json:"opening_balance"`
	} `json:"available"`
	Used struct {
		Debits float64 `json:"debits"`
	} `json:"used"`
}

// GetMargins returns the margin summary for a segment (e.g. "equity" or "commodity").
func (c *Client) GetMargins(ctx context.Context, segment string) (Margin, error) {
	var out struct {
		Margin
	}
	if err := c.get(ctx, "/user/margins/"+segment, nil, &out); err != nil {
		return Margin{}, err
	}
	return out.Margin, nil
}
